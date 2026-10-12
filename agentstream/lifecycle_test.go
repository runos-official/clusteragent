package agentstream

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// lifecycleStream is a fake control stream. It answers every heartbeat, and
// Recv blocks until the stream context ends or the test fails the stream.
type lifecycleStream struct {
	grpc.ClientStream
	ctx        context.Context
	fail       chan error
	heartbeats atomic.Int32
}

func newLifecycleStream() *lifecycleStream {
	return &lifecycleStream{fail: make(chan error, 1)}
}

func (s *lifecycleStream) Send(m *l2sec.FromClusterAgent) error {
	if m.Type == "ClusterAgentHeartbeatToServer" {
		s.heartbeats.Add(1)
		go handleInstruction(&l2sec.ToClusterAgent{Tag: m.Tag, Type: "ClusterAgentHeartbeatAck"})
	}
	return nil
}

func (s *lifecycleStream) Recv() (*l2sec.ToClusterAgent, error) {
	select {
	case <-s.ctx.Done():
		return nil, status.Error(codes.Canceled, "context canceled")
	case err := <-s.fail:
		return nil, err
	}
}

// lifecycleClient hands out the queued streams in order.
type lifecycleClient struct {
	l2sec.NodewardClient
	streams chan *lifecycleStream
}

func (c *lifecycleClient) ClusterAgentStream(ctx context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[l2sec.FromClusterAgent, l2sec.ToClusterAgent], error) {
	s := <-c.streams
	s.ctx = ctx
	return s, nil
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStreamDropStopsItsHeartbeat pins that a dropped stream takes its
// heartbeat goroutine with it (FPL315 review F1). Before the fix the old
// heartbeat lived on and sent on the next stream, so each restart of nodeward
// or nodehub added one more heartbeat per agent.
func TestStreamDropStopsItsHeartbeat(t *testing.T) {
	if n := activeHeartbeats.Load(); n != 0 {
		t.Fatalf("activeHeartbeats = %d before the test, want 0", n)
	}
	client := &lifecycleClient{streams: make(chan *lifecycleStream, 2)}
	sm := &streamManager{client: client, heartbeatInterval: 10 * time.Millisecond}

	first, second := newLifecycleStream(), newLifecycleStream()
	client.streams <- first
	client.streams <- second

	// First stream: heartbeats flow, then Recv fails as on a server restart.
	done1 := make(chan error, 1)
	go func() { done1 <- sm.establishAndMaintainStream() }()
	eventually(t, "heartbeats on the first stream", func() bool { return first.heartbeats.Load() >= 2 })
	first.fail <- status.Error(codes.Unavailable, "server restarting")
	if err := <-done1; err == nil {
		t.Fatal("establishAndMaintainStream returned nil after Recv failed")
	}
	if first.ctx.Err() == nil {
		t.Error("the first stream's context is still live after the stream dropped")
	}

	// Second stream comes up at once, as a fast jittered reconnect would.
	done2 := make(chan error, 1)
	go func() { done2 <- sm.establishAndMaintainStream() }()
	eventually(t, "heartbeats on the second stream", func() bool { return second.heartbeats.Load() >= 3 })
	eventually(t, "one heartbeat goroutine while the second stream is up", func() bool {
		return activeHeartbeats.Load() == 1
	})
	sentOnFirst := first.heartbeats.Load()
	time.Sleep(50 * time.Millisecond)
	if got := first.heartbeats.Load(); got != sentOnFirst {
		t.Errorf("heartbeats still sent on the dropped stream: %d then %d", sentOnFirst, got)
	}
	if got := activeHeartbeats.Load(); got != 1 {
		t.Errorf("activeHeartbeats = %d with one stream up, want 1", got)
	}

	second.fail <- status.Error(codes.Unavailable, "server restarting")
	<-done2
	eventually(t, "no heartbeat goroutine after the last stream dropped", func() bool {
		return activeHeartbeats.Load() == 0
	})
}

// fakeClock is a manual clock for the reconnect loop.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestReconnectLoopFeedsStreamUptime drives runWithReconnect's loop and checks
// the waits it asks for (FPL315 review F2). Only the time a stream was up
// counts towards the 60 s reset: the sleep and a slow failed dial must not.
func TestReconnectLoopFeedsStreamUptime(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	errDrop := errors.New("stream dropped")

	// Each establish call: how long the stream stays up, then its result.
	type run struct {
		upFor time.Duration
		err   error
	}
	runs := []run{
		{1 * time.Second, errDrop},  // short link: wait cap 1 s
		{1 * time.Second, errDrop},  // short again: cap 2 s; then the dial fails
		{0, errDrop},                // the old client fails at once: cap 4 s
		{61 * time.Second, errDrop}, // a stable link: schedule resets to 1 s
		{0, nil},                    // clean exit ends the loop
	}
	// Each connect call: how long the dial takes, and whether it works.
	type dial struct {
		takes time.Duration
		err   error
	}
	dials := []dial{
		{1 * time.Second, nil},
		{59 * time.Second, errors.New("dial timed out")},
		{1 * time.Second, nil},
		{1 * time.Second, nil},
	}

	var waits []time.Duration
	establishCalls, connectCalls := 0, 0
	sm := &streamManager{}
	sm.reconnectLoop(reconnectDeps{
		establish: func() error {
			r := runs[establishCalls]
			establishCalls++
			clock.Advance(r.upFor)
			return r.err
		},
		connect: func(context.Context) (l2sec.NodewardClient, *grpc.ClientConn, error) {
			d := dials[connectCalls]
			connectCalls++
			clock.Advance(d.takes)
			return nil, nil, d.err
		},
		sleep: func(d time.Duration) {
			waits = append(waits, d)
			clock.Advance(d)
		},
		now:     clock.Now,
		backoff: newReconnectBackoff(func(n int64) int64 { return n - 1 }),
	})

	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 1 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("wait %d = %s, want %s (all waits %v)", i+1, waits[i], want[i], waits)
		}
	}
	if establishCalls != len(runs) || connectCalls != len(dials) {
		t.Errorf("establish called %d times, connect %d times, want %d and %d",
			establishCalls, connectCalls, len(runs), len(dials))
	}
}

// TestRetryStepJitter pins that bootstrap retries draw a full-jitter wait under
// the doubling cap.
func TestRetryStepJitter(t *testing.T) {
	failThenSucceed := func(fails int) func() (int, error) {
		n := 0
		return func() (int, error) {
			n++
			if n <= fails {
				return 0, status.Error(codes.Unavailable, "not ready")
			}
			return 42, nil
		}
	}

	for name, tc := range map[string]struct {
		draw func(int64) int64
		want []time.Duration
	}{
		"highest draw waits the cap": {
			draw: func(n int64) int64 { return n - 1 },
			want: []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second},
		},
		"lowest draw waits nothing": {
			draw: func(int64) int64 { return 0 },
			want: []time.Duration{0, 0, 0},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var waits []time.Duration
			got := retryStepWith("test step", failThenSucceed(3),
				func(d time.Duration) { waits = append(waits, d) }, tc.draw)
			if got != 42 {
				t.Fatalf("retryStepWith = %d, want 42", got)
			}
			if len(waits) != len(tc.want) {
				t.Fatalf("waits = %v, want %v", waits, tc.want)
			}
			for i := range tc.want {
				if waits[i] != tc.want[i] {
					t.Errorf("wait %d = %s, want %s", i+1, waits[i], tc.want[i])
				}
			}
		})
	}
}
