package agentstream

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"google.golang.org/grpc"
)

// Lifetime is the single value a concrete control stream owns for its
// entire life (review finding 1313, repeat of 1270/1240/1169). Before this
// type, the agent tracked three independent globals - a stream pointer, a
// "connected" bool, and a generation counter - and they changed one at a
// time, not atomically together: SendToServer read only the pointer, so
// between the bool/counter flipping at the moment of detection and the
// manager's later cleanup nilling the pointer, SendToServer could still
// select and send on an already-dead stream.
//
// Exactly one Lifetime exists per concrete connected stream. It is created
// live when the stream connects and ended at most once, by whichever
// goroutine first detects that stream has failed (receive error or
// heartbeat exhaustion), before that goroutine logs anything or returns to
// the manager for its deferred cleanup. Ending an already-ended Lifetime,
// or ending one from a goroutine whose stream has already been superseded
// by a successor connection, changes nothing but this exact value: each
// goroutine only ever holds a reference to its own stream's Lifetime, never
// to "whichever is current", so a delayed callback from an old stream can
// never affect a successor's Lifetime.
//
// pending is THIS stream's own waiter map (tag -> reply channel), owned
// exclusively by it (review finding 1403: a single package-wide map let a
// delayed cleanup for one stream close a successor's still-pending
// waiter). A request registers here only after it has committed to
// sending on this exact Lifetime, so cleanup draining pending on end can
// never reach any other Lifetime's waiters.
type Lifetime struct {
	stream  grpc.BidiStreamingClient[l2sec.FromClusterAgent, l2sec.ToClusterAgent]
	live    atomic.Bool
	pending sync.Map
}

// newLifetime returns a Lifetime that is live for stream.
func newLifetime(stream grpc.BidiStreamingClient[l2sec.FromClusterAgent, l2sec.ToClusterAgent]) *Lifetime {
	lt := &Lifetime{stream: stream}
	lt.live.Store(true)
	return lt
}

// end marks lt over. Idempotent, and safe to call concurrently: two
// goroutines racing to end the same stream's Lifetime (a receive failure
// and a heartbeat exhaustion observed at nearly the same time) do not need
// to coordinate who calls it first.
func (lt *Lifetime) end() {
	lt.live.Store(false)
}

// Live reports whether lt's stream can still be used for anything: sending
// on it, delivering a reply to a waiter that sent on it, or treating a
// permit it carried as still valid. A nil Lifetime (no stream has ever
// connected) is never live.
func (lt *Lifetime) Live() bool {
	return lt != nil && lt.live.Load()
}

// currentLifetime is the control stream's current Lifetime: nil until the
// first stream connects. Replaces the old globalStream/streamConnected/
// streamGeneration trio.
var currentLifetime atomic.Pointer[Lifetime]

// CurrentLifetime returns the control stream's current Lifetime. A caller
// that captured a Lifetime earlier (for example, the one sendAndWait
// validated a reply against) can compare its own .Live() against this
// value's .Live() later: once a Lifetime goes live-to-ended it never comes
// back, so a difference proves the stream it named has disconnected,
// reconnected, or both since.
func CurrentLifetime() *Lifetime {
	return currentLifetime.Load()
}

// StreamConnected reports whether the agent currently holds a live gRPC
// stream to the control plane. Used by the webhook health endpoint to
// surface connection state without affecting liveness.
func StreamConnected() bool {
	return currentLifetime.Load().Live()
}

// NewTestLifetime returns a standalone Lifetime for tests outside this
// package: live if live is true, already ended otherwise. Production code
// never builds a Lifetime this way - the only Lifetime production code
// uses is the one runStream creates for a concrete connected stream.
func NewTestLifetime(live bool) *Lifetime {
	lt := &Lifetime{}
	lt.live.Store(live)
	return lt
}

// EndTestLifetime ends lt, for a test outside this package that needs to
// simulate a stale goroutine's delayed invalidation call explicitly (review
// finding 1313's "a delayed callback of the old stream arrives after
// reconnect" case), rather than waiting for a real receive failure or
// heartbeat exhaustion to trigger it.
func EndTestLifetime(lt *Lifetime) {
	lt.end()
}

// DriveTestStream runs the real production publish/heartbeat/listen/
// cleanup loop (runStream) against a caller-supplied fake stream, for a
// test outside this package that needs the real send, reply-dispatch, and
// lifetime-invalidation path with no live gRPC connection. It blocks until
// the stream ends (receive failure or heartbeat exhaustion) and returns the
// same error establishAndMaintainStream would have.
//
// pauseCleanup, if non-nil, is received from right after the terminating
// error is known and before the deferred pendingResponses drain, so a test
// can hold that drain back indefinitely while still observing that every
// Lifetime-gated behavior (SendToServer, sendAndWait, a recheck permit)
// already reflects the ended stream - proving the fix does not depend on
// that drain having run at all.
func DriveTestStream(
	stream grpc.BidiStreamingClient[l2sec.FromClusterAgent, l2sec.ToClusterAgent],
	heartbeatInterval time.Duration,
	pauseCleanup <-chan struct{},
) error {
	sm := &streamManager{heartbeatInterval: heartbeatInterval}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return sm.runStream(ctx, stream, pauseCleanup)
}
