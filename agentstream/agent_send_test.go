package agentstream

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
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
// a context error, and must leave no pending entry behind on lt.
func TestSendAndWait_SendPhaseBoundedByContext(t *testing.T) {
	lt := newLifetime(&fakeBidiStream{})
	before := countPending(lt)

	blockUntil := make(chan struct{})
	t.Cleanup(func() { close(blockUntil) }) // let the leaked goroutine finish so it doesn't outlive the test

	slowSend := func(*Lifetime, *l2sec.FromClusterAgent) error {
		<-blockUntil
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := sendAndWait(ctx, lt, slowSend, &l2sec.FromClusterAgent{Type: "TEST"})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("sendAndWait took %v, want a prompt return bounded by the 20ms context", elapsed)
	}
	if got := countPending(lt); got != before {
		t.Fatalf("lt.pending count = %d, want %d (the entry must be cleaned up)", got, before)
	}
}

// TestSendAndWait_WaitPhaseBoundedByContext confirms the existing guarantee
// still holds after the send-phase change: a send that succeeds immediately
// but gets no reply must still time out on ctx during the wait, with
// cleanup.
func TestSendAndWait_WaitPhaseBoundedByContext(t *testing.T) {
	lt := newLifetime(&fakeBidiStream{})
	before := countPending(lt)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, _, err := sendAndWait(ctx, lt, func(*Lifetime, *l2sec.FromClusterAgent) error { return nil }, &l2sec.FromClusterAgent{Type: "TEST"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if got := countPending(lt); got != before {
		t.Fatalf("lt.pending count = %d, want %d (the entry must be cleaned up)", got, before)
	}
}

// TestSendAndWait_ConcurrentCallsResolveIndependently proves two concurrent
// sendAndWait calls on the SAME lifetime (shaped like a heartbeat racing a
// deployment stage recheck) each receive their own matched response and
// never cross-wire, even though they share that lifetime's pending map and
// the same production send path's correlation-by-tag.
func TestSendAndWait_ConcurrentCallsResolveIndependently(t *testing.T) {
	lt := newLifetime(&fakeBidiStream{})

	// The fake send immediately "delivers" a reply correlated to msg.Tag,
	// the same way the real stream dispatcher resolves a reply by tag.
	fakeSend := func(lt *Lifetime, msg *l2sec.FromClusterAgent) error {
		if v, ok := lt.pending.Load(msg.Tag); ok {
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
			resp, _, err := sendAndWait(ctx, lt, fakeSend, &l2sec.FromClusterAgent{Type: types[i]})
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
// must not be accepted once the Lifetime it was sent on has since ended -
// even though the buffered channel read itself succeeds (closing a channel
// does not discard a value already written to it).
func TestSendAndWait_RefusesReplyAfterLifetimeEnded(t *testing.T) {
	lt := newLifetime(&fakeBidiStream{})

	// The fake send delivers the reply, then - still inside send, before
	// sendAndWait's own select can read it - ends the Lifetime it was sent
	// on. This models a disconnect detected while the reply is already
	// sitting in the buffered channel.
	fakeSend := func(lt *Lifetime, msg *l2sec.FromClusterAgent) error {
		if v, ok := lt.pending.Load(msg.Tag); ok {
			ch := v.(chan *l2sec.ToClusterAgent)
			ch <- &l2sec.ToClusterAgent{Tag: msg.Tag, Type: "ECHO:" + msg.Type}
		}
		lt.end()
		return nil
	}

	resp, gotLifetime, err := sendAndWait(context.Background(), lt, fakeSend, &l2sec.FromClusterAgent{Type: "RECHECK"})
	if err == nil {
		t.Fatalf("expected a refusal once the Lifetime ended before the reply was accepted, got %+v", resp)
	}
	if resp != nil || gotLifetime != nil {
		t.Errorf("expected no response or Lifetime alongside the refusal, got resp=%+v lifetime=%v", resp, gotLifetime)
	}
}

// TestSendOnLifetime_RefusesWhenEndedWhileQueuedForSend pins review finding
// 1403's "check the lifetime after waiting for send serialization": a send
// that is queued behind sendMu (another writer holds it) must not transmit
// once its turn comes if its Lifetime ended while it was waiting. Checking
// Live() only before contending for the mutex is not enough - holding
// sendMu directly from the test (white-box, same package) proves the
// second, post-acquire check is load-bearing: the queued call's own FIRST
// check passes (the Lifetime is still live when it starts), it blocks on
// the mutex, and only then does the Lifetime end.
func TestSendOnLifetime_RefusesWhenEndedWhileQueuedForSend(t *testing.T) {
	fake := &fakeBidiStream{}
	lt := newLifetime(fake)

	sendMu.Lock() // simulate another sender already in flight, holding the serialization point
	started := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		close(started)
		errCh <- sendOnLifetime(lt, &l2sec.FromClusterAgent{Type: "QUEUED"})
	}()
	<-started
	time.Sleep(20 * time.Millisecond) // let the goroutine's first check pass and reach the blocking sendMu.Lock()

	lt.end() // the Lifetime ends while that send is queued behind sendMu, not before it even started

	sendMu.Unlock() // release the queued send to its post-acquire check

	err := <-errCh
	if err == nil {
		t.Fatal("expected a refusal: the Lifetime ended while this send was queued for the mutex")
	}
	if fake.inFlight.Load() != 0 {
		t.Error("the queued send must never have reached Send() on an ended Lifetime")
	}
}

// countPending lets a test assert that sendAndWait's defer cleanup actually
// ran, independent of any other test's entries on the same lt.
func countPending(lt *Lifetime) int {
	n := 0
	lt.pending.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// fakeBidiStream is a minimal grpc.BidiStreamingClient fake used only to
// exercise sendOnLifetime's own serialization (review finding 1077: gRPC
// does not allow concurrent Send calls on one ClientStream). sending/overlap
// use atomics because many goroutines call Send concurrently by design in
// the test that uses this fake.
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
// later when runStream's own goroutine is scheduled to dequeue errChan and
// run its cleanup (the lt.pending drain). That scheduling gap is exactly
// where a reply already buffered for a pending recheck could be read as
// still belonging to a live connection, because the Lifetime had not ended
// yet. fakeBidiStream.Recv always returns io.EOF, so one call reproduces
// the failure deterministically with no real stream.
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
// runHeartbeat itself gives up, not later.
func TestRunHeartbeat_EndsLifetimeAtDetection(t *testing.T) {
	fake := &fakeBidiStream{sendErr: errors.New("send boom")}
	lt := newLifetime(fake)
	sm := &streamManager{heartbeatInterval: 1 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := sm.runHeartbeat(ctx, lt)
	if err == nil {
		t.Fatal("expected an error after three consecutive heartbeat failures")
	}
	if lt.Live() {
		t.Fatal("expected the Lifetime to end synchronously inside runHeartbeat, not later")
	}
}

// blockingWriter is an io.Writer that blocks on hold once it sees a line
// containing match, after signaling onMatch exactly once. Used to hold a
// log.Printf call open so a test can observe state while it is still
// blocked on that write.
type blockingWriter struct {
	match   string
	hold    <-chan struct{}
	onMatch func()
	matched sync.Once
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.match) {
		w.matched.Do(w.onMatch)
		<-w.hold
	}
	return len(p), nil
}

// TestRunHeartbeat_EndsLifetimeBeforeTerminalFailureLog pins review finding
// 1404: the third, terminal heartbeat failure must end this stream's own
// Lifetime BEFORE the "Heartbeat failed (3/3)" log line, not after. A
// blocked or slow log writer must never leave the Lifetime live past the
// point this goroutine has already decided the stream is dead. This holds
// that exact log write open and asserts the Lifetime is already ended
// while it is still blocked.
func TestRunHeartbeat_EndsLifetimeBeforeTerminalFailureLog(t *testing.T) {
	fake := &fakeBidiStream{sendErr: errors.New("send boom")}
	lt := newLifetime(fake)
	sm := &streamManager{heartbeatInterval: 1 * time.Millisecond}

	hold := make(chan struct{})
	triggered := make(chan struct{})
	prevOut := log.Writer()
	log.SetOutput(&blockingWriter{
		match:   "Heartbeat failed (3/3)",
		hold:    hold,
		onMatch: func() { close(triggered) },
	})
	t.Cleanup(func() { log.SetOutput(prevOut) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sm.runHeartbeat(ctx, lt) }()

	select {
	case <-triggered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the terminal failure log write to start")
	}

	if lt.Live() {
		t.Fatal("expected the Lifetime to already be ended while the terminal failure log write is still blocked")
	}

	close(hold) // let the blocked log write, and runHeartbeat, finish
	if err := <-done; err == nil {
		t.Fatal("expected an error after three consecutive heartbeat failures")
	}
}
