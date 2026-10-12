package agentstream

import (
	"testing"
	"time"
)

// TestFullJitter pins full jitter: the wait is uniform in [0, cap].
func TestFullJitter(t *testing.T) {
	low := func(int64) int64 { return 0 }
	high := func(n int64) int64 { return n - 1 }

	if got := fullJitter(time.Second, low); got != 0 {
		t.Errorf("lowest draw = %s, want 0", got)
	}
	if got := fullJitter(time.Second, high); got != time.Second {
		t.Errorf("highest draw = %s, want the cap 1s", got)
	}
	if got := fullJitter(0, high); got != 0 {
		t.Errorf("zero cap = %s, want 0", got)
	}
}

// TestReconnectBackoffSchedule pins the reconnect schedule (FPL315): the first
// wait is in [0, 1 s], the cap doubles up to 30 s, and every wait stays in
// [0, cap].
func TestReconnectBackoffSchedule(t *testing.T) {
	high := func(n int64) int64 { return n - 1 }
	b := newReconnectBackoff(high)
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	for i, w := range want {
		if got := b.next(); got != w {
			t.Errorf("wait %d at the highest draw = %s, want cap %s", i+1, got, w)
		}
	}

	// With the real random source every wait stays inside its cap, and the
	// waits are spread, not all the same.
	r := newReconnectBackoff(nil)
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		r.attempt = 0
		got := r.next()
		if got < 0 || got > time.Second {
			t.Fatalf("first wait %s outside [0, 1s]", got)
		}
		seen[got] = true
	}
	if len(seen) < 100 {
		t.Fatalf("first waits are not spread: %d distinct values in 200 draws", len(seen))
	}
}

// TestReconnectBackoffResetsAfterStableConnection pins the reset rule: the
// schedule starts again only after a connection stayed up 60 s.
func TestReconnectBackoffResetsAfterStableConnection(t *testing.T) {
	high := func(n int64) int64 { return n - 1 }
	b := newReconnectBackoff(high)
	for i := 0; i < 5; i++ {
		b.next()
	}

	b.connectionEnded(59 * time.Second)
	if got := b.next(); got != 30*time.Second {
		t.Fatalf("after a 59 s connection the wait = %s, want the schedule to keep growing (30s)", got)
	}

	b.connectionEnded(60 * time.Second)
	if got := b.next(); got != time.Second {
		t.Fatalf("after a 60 s connection the wait = %s, want the schedule reset (1s)", got)
	}
}
