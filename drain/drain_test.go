package drain

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestBegin_CountsInFlightWork(t *testing.T) {
	tr := New()
	end1, err := tr.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	end2, err := tr.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got := tr.InFlight(); got != 2 {
		t.Fatalf("InFlight = %d, want 2", got)
	}
	end1()
	end2()
	if got := tr.InFlight(); got != 0 {
		t.Fatalf("InFlight = %d, want 0 after both end", got)
	}
}

func TestEnd_IsIdempotent(t *testing.T) {
	tr := New()
	end, _ := tr.Begin()
	other, _ := tr.Begin()
	end()
	end() // a second call must not release the other unit of work
	if got := tr.InFlight(); got != 1 {
		t.Fatalf("InFlight = %d, want 1 (double end must not steal a count)", got)
	}
	other()
}

func TestDrain_WaitsForRunningWorkThenReturnsZero(t *testing.T) {
	tr := New()
	end, _ := tr.Begin()

	done := make(chan int, 1)
	go func() { done <- tr.Drain(5 * time.Second) }()

	// Drain has started: it must refuse new work while the old work runs.
	waitFor(t, tr.Draining)
	if _, err := tr.Begin(); !errors.Is(err, ErrDraining) {
		t.Fatalf("Begin during drain: err = %v, want ErrDraining", err)
	}

	select {
	case <-done:
		t.Fatal("Drain returned while work was still running")
	case <-time.After(50 * time.Millisecond):
	}

	end()
	select {
	case remaining := <-done:
		if remaining != 0 {
			t.Fatalf("Drain remaining = %d, want 0", remaining)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return after the work ended")
	}
}

func TestDrain_ReturnsRemainingCountAtTimeout(t *testing.T) {
	tr := New()
	end1, _ := tr.Begin()
	defer end1()
	end2, _ := tr.Begin()
	defer end2()

	start := time.Now()
	remaining := tr.Drain(100 * time.Millisecond)
	if remaining != 2 {
		t.Fatalf("Drain remaining = %d, want 2", remaining)
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Fatalf("Drain returned after %s, want it to wait out the timeout", elapsed)
	}
}

func TestDrain_ReturnsAtOnceWhenIdle(t *testing.T) {
	tr := New()
	start := time.Now()
	if remaining := tr.Drain(5 * time.Second); remaining != 0 {
		t.Fatalf("Drain remaining = %d, want 0", remaining)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Drain took %s on an idle tracker", elapsed)
	}
	if !tr.Draining() {
		t.Fatal("Draining = false after Drain, want true")
	}
}

func TestBegin_ConcurrentWithDrainNeverLosesWork(t *testing.T) {
	// Every Begin that succeeds must be counted by Drain. A Begin that loses
	// the race must get ErrDraining, never a silent success that Drain missed.
	tr := New()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ends []func()
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if end, err := tr.Begin(); err == nil {
				mu.Lock()
				ends = append(ends, end)
				mu.Unlock()
			}
		}()
	}
	remaining := tr.Drain(50 * time.Millisecond)
	wg.Wait()

	mu.Lock()
	accepted := len(ends)
	mu.Unlock()
	if got := tr.InFlight(); got != accepted {
		t.Fatalf("InFlight = %d, accepted = %d: the tracker lost work", got, accepted)
	}
	if remaining > accepted {
		t.Fatalf("remaining = %d exceeds accepted = %d", remaining, accepted)
	}
	for _, end := range ends {
		end()
	}
}

func TestHold_RefusesWorkUntilReleased(t *testing.T) {
	tr := New()
	tr.Hold()
	if _, err := tr.Begin(); !errors.Is(err, ErrStarting) {
		t.Fatalf("Begin while held: err = %v, want ErrStarting", err)
	}
	tr.Release()
	end, err := tr.Begin()
	if err != nil {
		t.Fatalf("Begin after release: %v", err)
	}
	end()
}

func TestRelease_DoesNotUndoADrain(t *testing.T) {
	// A SIGTERM that lands while the agent still holds work back must win:
	// releasing the hold afterwards must not reopen the agent for work.
	tr := New()
	tr.Hold()
	tr.Drain(0)
	tr.Release()
	if _, err := tr.Begin(); !errors.Is(err, ErrDraining) {
		t.Fatalf("Begin after drain and release: err = %v, want ErrDraining", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}
