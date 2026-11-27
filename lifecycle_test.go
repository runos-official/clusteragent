package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestShutdown_CleanDrainLeavesRowsAlone(t *testing.T) {
	called := false
	shutdown(
		func(time.Duration) int { return 0 },
		func(string) (int, error) { called = true; return 0, nil },
		35*time.Minute,
	)
	if called {
		t.Fatal("rows were failed after a clean drain, want them left alone")
	}
}

func TestShutdown_TimeoutFailsTheRemainingRowsWithAReason(t *testing.T) {
	var reason string
	shutdown(
		func(time.Duration) int { return 2 },
		func(r string) (int, error) { reason = r; return 2, nil },
		35*time.Minute,
	)
	if !strings.Contains(reason, "shutdown") || !strings.Contains(reason, "35m") {
		t.Fatalf("reason = %q, want it to name the shutdown and the 35m wait", reason)
	}
}

func TestShutdown_PassesTheTimeoutToTheDrain(t *testing.T) {
	var got time.Duration
	shutdown(
		func(d time.Duration) int { got = d; return 0 },
		func(string) (int, error) { return 0, nil },
		90*time.Second,
	)
	if got != 90*time.Second {
		t.Fatalf("drain timeout = %s, want 1m30s", got)
	}
}

func TestReconcileOrphanedBuilds_UsesTheRestartReason(t *testing.T) {
	var reason string
	n, err := reconcileOrphanedBuilds(func(r string) (int, error) { reason = r; return 3, nil })
	if err != nil {
		t.Fatalf("reconcileOrphanedBuilds: %v", err)
	}
	if n != 3 {
		t.Fatalf("closed = %d, want 3", n)
	}
	if !strings.Contains(reason, "restart") {
		t.Fatalf("reason = %q, want it to say the agent restarted", reason)
	}
}

func TestReconcileOrphanedBuilds_ReturnsTheError(t *testing.T) {
	n, err := reconcileOrphanedBuilds(func(string) (int, error) { return 0, errFake })
	if err == nil {
		t.Fatal("err = nil, want the failure so the caller can retry")
	}
	if n != 0 {
		t.Fatalf("closed = %d, want 0 on error", n)
	}
}

func TestDrainTimeout_DefaultAndOverride(t *testing.T) {
	t.Setenv(drainTimeoutEnv, "")
	if got := drainTimeout(); got != 35*time.Minute {
		t.Fatalf("default = %s, want 35m", got)
	}
	t.Setenv(drainTimeoutEnv, "90s")
	if got := drainTimeout(); got != 90*time.Second {
		t.Fatalf("override = %s, want 1m30s", got)
	}
	t.Setenv(drainTimeoutEnv, "not-a-duration")
	if got := drainTimeout(); got != 35*time.Minute {
		t.Fatalf("invalid override = %s, want the 35m default", got)
	}
}

var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "fake" }

func TestReconcileUntilDone_RetriesThenReleases(t *testing.T) {
	attempts := 0
	released := 0
	reconcileUntilDone(
		func(string) (int, error) {
			attempts++
			if attempts < 3 {
				return 0, errFake // the database is not ready yet
			}
			return 1, nil
		},
		func() { released++ },
		time.Millisecond,
		5*time.Second,
	)
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (two failures, then success)", attempts)
	}
	if released != 1 {
		t.Fatalf("released = %d, want exactly 1", released)
	}
}

func TestReconcileUntilDone_GivesUpAfterTheCapAndStillReleases(t *testing.T) {
	// A reconcile that never works must not block builds for ever.
	attempts := 0
	released := 0
	reconcileUntilDone(
		func(string) (int, error) { attempts++; return 0, errFake },
		func() { released++ },
		time.Millisecond,
		30*time.Millisecond,
	)
	if attempts < 2 {
		t.Fatalf("attempts = %d, want several before giving up", attempts)
	}
	if released != 1 {
		t.Fatalf("released = %d, want exactly 1 after the cap", released)
	}
}

// A rolling update starts the new agent pod while the old one still runs.
// The old pod's build pods and open build rows are not orphans then, so the
// new pod must not sweep or close them until the old pod has gone. Before
// this change the new pod cleaned up at once and killed the old pod's
// builds.
func TestStartupCleanup_WaitsUntilNoOtherAgentPodRuns(t *testing.T) {
	var events []string
	alone := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		startupCleanup(
			func() { <-alone; events = append(events, "alone") },
			func() { events = append(events, "sweep pods") },
			func(string) (int, error) { events = append(events, "close rows"); return 0, nil },
			func() { events = append(events, "release") },
			time.Millisecond, time.Second,
		)
	}()
	select {
	case <-done:
		t.Fatalf("cleanup ran while another agent pod still ran: %v", events)
	case <-time.After(50 * time.Millisecond):
	}
	close(alone)
	<-done
	want := "[alone sweep pods close rows release]"
	if got := fmt.Sprint(events); got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
}
