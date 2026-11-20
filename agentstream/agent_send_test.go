package agentstream

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"google.golang.org/grpc/metadata"
)

// TestSendAndWait_SendPhaseBoundedByContext pins review finding 1077: the
// send phase must be bounded by ctx too, not only the reply wait. Before the
// fix, a stalled send() call blocked sendAndWait past its caller's deadline
// (RequestStageRecheck's 5s ceiling included) because send(msg) ran
// synchronously with no select against ctx. Here send blocks far longer
// than the context's timeout; sendAndWait must still return promptly with
// a context error, and must leave no pendingResponses entry behind.
func TestSendAndWait_SendPhaseBoundedByContext(t *testing.T) {
	before := countPendingResponses()

	blockUntil := make(chan struct{})
	t.Cleanup(func() { close(blockUntil) }) // let the leaked goroutine finish so it doesn't outlive the test

	slowSend := func(*l2sec.FromClusterAgent) error {
		<-blockUntil
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := sendAndWait(ctx, slowSend, &l2sec.FromClusterAgent{Type: "TEST"})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("sendAndWait took %v, want a prompt return bounded by the 20ms context", elapsed)
	}
	if got := countPendingResponses(); got != before {
		t.Fatalf("pendingResponses count = %d, want %d (the entry must be cleaned up)", got, before)
	}
}

// TestSendAndWait_WaitPhaseBoundedByContext confirms the existing guarantee
// still holds after the send-phase change: a send that succeeds immediately
// but gets no reply must still time out on ctx during the wait, with
// cleanup.
func TestSendAndWait_WaitPhaseBoundedByContext(t *testing.T) {
	before := countPendingResponses()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, _, err := sendAndWait(ctx, func(*l2sec.FromClusterAgent) error { return nil }, &l2sec.FromClusterAgent{Type: "TEST"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if got := countPendingResponses(); got != before {
		t.Fatalf("pendingResponses count = %d, want %d (the entry must be cleaned up)", got, before)
	}
}

// TestSendAndWait_ConcurrentCallsResolveIndependently proves two concurrent
// sendAndWait calls (shaped like a heartbeat racing a deployment stage
// recheck) each receive their own matched response and never cross-wire,
// even though they share the same pendingResponses map and the same
// production send path's correlation-by-tag.
func TestSendAndWait_ConcurrentCallsResolveIndependently(t *testing.T) {
	// sendAndWait only accepts a reply while the Lifetime it was sent on is
	// still live (L3); give it one, since this test is about correlation,
	// not disconnect handling.
	prev := currentLifetime.Load()
	currentLifetime.Store(newLifetime(&fakeBidiStream{}))
	t.Cleanup(func() { currentLifetime.Store(prev) })

	// The fake send immediately "delivers" a reply correlated to msg.Tag,
	// the same way the real stream dispatcher resolves a reply by tag.
	fakeSend := func(msg *l2sec.FromClusterAgent) error {
		if v, ok := pendingResponses.Load(msg.Tag); ok {
			ch := v.(chan *l2sec.ToClusterAgent)
			ch <- &l2sec.ToClusterAgent{Tag: msg.Tag, Type: "ECHO:" + msg.Type}
		}
		return nil
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	results := make([]*l2sec.ToClusterAgent, 2)
	types := []string{"HEARTBEAT", "RECHECK"}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, _, err := sendAndWait(ctx, fakeSend, &l2sec.FromClusterAgent{Type: types[i]})
			if err != nil {
				t.Errorf("call %d: unexpected error: %v", i, err)
				return
			}
			results[i] = resp
		}(i)
	}
	wg.Wait()

	for i, want := range types {
		if results[i] == nil || results[i].Type != "ECHO:"+want {
			t.Errorf("call %d: response = %+v, want echo of %q", i, results[i], want)
		}
	}
}

// TestSendAndWait_RefusesReplyAfterLifetimeEnded pins review finding 1313
// (repeat of 1245/1169): a reply already buffered for a pending call's tag
// must not be accepted once the Lifetime that was current when the call
// sent has since ended - even though the buffered channel read itself
// succeeds (closing a channel does not discard a value already written to
// it). Replaces TestRequestStageRecheck_RefusesWhenGenerationMovesDuringSend,
// which used to pin this one layer up, in deploymentrecheck.go, with a
// generation counter; the guarantee now lives once, here, for every
// sendAndWait caller.
func TestSendAndWait_RefusesReplyAfterLifetimeEnded(t *testing.T) {
	prev := currentLifetime.Load()
	lt := newLifetime(&fakeBidiStream{})
	currentLifetime.Store(lt)
	t.Cleanup(func() { currentLifetime.Store(prev) })

	// The fake send delivers the reply, then - still inside send, before
	// sendAndWait's own select can read it - ends the Lifetime that was
	// current when sendAndWait sampled it. This models a disconnect
	// detected while the reply is already sitting in the buffered channel.
	fakeSend := func(msg *l2sec.FromClusterAgent) error {
		if v, ok := pendingResponses.Load(msg.Tag); ok {
			ch := v.(chan *l2sec.ToClusterAgent)
			ch <- &l2sec.ToClusterAgent{Tag: msg.Tag, Type: "ECHO:" + msg.Type}
		}
		lt.end()
		return nil
	}

	resp, gotLifetime, err := sendAndWait(context.Background(), fakeSend, &l2sec.FromClusterAgent{Type: "RECHECK"})
	if err == nil {
		t.Fatalf("expected a refusal once the Lifetime ended before the reply was accepted, got %+v", resp)
	}
	if resp != nil || gotLifetime != nil {
		t.Errorf("expected no response or Lifetime alongside the refusal, got resp=%+v lifetime=%v", resp, gotLifetime)
	}
}

// countPendingResponses lets a test assert that sendAndWait's defer cleanup
// actually ran, independent of any other test's entries.
func countPendingResponses() int {
	n := 0
	pendingResponses.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// fakeBidiStream is a minimal grpc.BidiStreamingClient fake used only to
// exercise sendOnStream's own serialization (review finding 1077: gRPC does
// not allow concurrent Send calls on one ClientStream). sending/overlap use
// atomics because many goroutines call Send concurrently by design in the
// test that uses this fake.
type fakeBidiStream struct {
	sendDelay time.Duration
	sendErr   error
	inFlight  atomic.Int32
	overlap   atomic.Bool
}

func (f *fakeBidiStream) Send(*l2sec.FromClusterAgent) error {
	if f.inFlight.Add(1) > 1 {
		f.overlap.Store(true)
	}
	time.Sleep(f.sendDelay)
	f.inFlight.Add(-1)
	return f.sendErr
}
func (f *fakeBidiStream) Recv() (*l2sec.ToClusterAgent, error) { return nil, io.EOF }
func (f *fakeBidiStream) Header() (metadata.MD, error)         { return nil, nil }
func (f *fakeBidiStream) Trailer() metadata.MD                 { return nil }
func (f *fakeBidiStream) CloseSend() error                     { return nil }
func (f *fakeBidiStream) Context() context.Context             { return context.Background() }
func (f *fakeBidiStream) SendMsg(m any) error                  { return nil }
func (f *fakeBidiStream) RecvMsg(m any) error                  { return nil }

// TestSendOnStream_SerializesConcurrentSends pins review finding 1077's
// "serialize sends per stream": many goroutines calling SendToServer at
// once (the realistic shape of a heartbeat tick racing an HTTP-triggered
// deployment stage recheck) must never have two Send calls in flight on the
// stream at the same time.
func TestSendOnStream_SerializesConcurrentSends(t *testing.T) {
	fake := &fakeBidiStream{sendDelay: 5 * time.Millisecond}

	prev := currentLifetime.Load()
	currentLifetime.Store(newLifetime(fake))
	t.Cleanup(func() { currentLifetime.Store(prev) })

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = SendToServer(&l2sec.FromClusterAgent{Type: "CONCURRENT_TEST"})
		}(i)
	}
	wg.Wait()

	if fake.overlap.Load() {
		t.Fatal("two Send calls were in flight on the stream at once; sends must be serialized")
	}
}

// TestListenForInstructions_EndsLifetimeAtDetection pins review finding
// 1313 (repeat of 1270/1240/1169): the stream's own Lifetime must end the
// moment listenForInstructions itself observes the stream has failed, not
// later when establishAndMaintainStream's own goroutine is scheduled to
// dequeue errChan and run its cleanup (runStream's pendingResponses
// drain). That scheduling gap is exactly where a reply already buffered
// for a pending recheck could be read as still belonging to a live
// connection, because the Lifetime had not ended yet. fakeBidiStream.Recv
// always returns io.EOF, so one call reproduces the failure
// deterministically with no real stream. Replaces
// TestListenForInstructions_InvalidatesStreamGenerationAtDetection (same
// case, generation counter replaced by Lifetime).
func TestListenForInstructions_EndsLifetimeAtDetection(t *testing.T) {
	lt := newLifetime(&fakeBidiStream{})
	sm := &streamManager{}
	if err := sm.listenForInstructions(&fakeBidiStream{}, lt); err == nil {
		t.Fatal("expected an error once Recv() fails")
	}
	if lt.Live() {
		t.Fatal("expected the Lifetime to end synchronously inside listenForInstructions, not later")
	}
}

// TestRunHeartbeat_EndsLifetimeAtDetection is
// TestListenForInstructions_EndsLifetimeAtDetection's sibling case: three
// consecutive heartbeat failures must also end the Lifetime at the point
// runHeartbeat itself gives up, not later. Replaces
// TestRunHeartbeat_InvalidatesStreamGenerationAtDetection (same case,
// generation counter replaced by Lifetime).
func TestRunHeartbeat_EndsLifetimeAtDetection(t *testing.T) {
	fake := &fakeBidiStream{sendErr: errors.New("send boom")}
	lt := newLifetime(fake)
	sm := &streamManager{heartbeatInterval: 1 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := sm.runHeartbeat(ctx, fake, lt)
	if err == nil {
		t.Fatal("expected an error after three consecutive heartbeat failures")
	}
	if lt.Live() {
		t.Fatal("expected the Lifetime to end synchronously inside runHeartbeat, not later")
	}
}
