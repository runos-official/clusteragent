package instructions

import (
	"testing"

	"github.com/runos-official/clusteragent/datastore"
)

func TestClassifyOneShotOutcome(t *testing.T) {
	cases := []struct {
		name       string
		in         oneShotOutcomeInputs
		wantStatus string
		wantExit   int
	}{
		{
			// Deadline kill wins even when the pod's SIGKILL exit (137) is
			// already readable: this is the race the fix targets.
			name:       "deadline exceeded beats pod 137",
			in:         oneShotOutcomeInputs{deadlineExceeded: true, podTerminated: true, podExitCode: 137, jobFailed: true},
			wantStatus: datastore.OneShotStatusTimeout,
			wantExit:   124,
		},
		{
			// Signal-killed pod with no deadline condition known yet: after the
			// wait window the classifier falls back to the raw 137.
			name:       "pod 137 without deadline condition falls back to failed/137",
			in:         oneShotOutcomeInputs{podTerminated: true, podExitCode: 137},
			wantStatus: datastore.OneShotStatusFailed,
			wantExit:   137,
		},
		{
			name:       "clean non-zero exit propagates real code",
			in:         oneShotOutcomeInputs{podTerminated: true, podExitCode: 7},
			wantStatus: datastore.OneShotStatusFailed,
			wantExit:   7,
		},
		{
			name:       "clean zero exit is success",
			in:         oneShotOutcomeInputs{podTerminated: true, podExitCode: 0},
			wantStatus: datastore.OneShotStatusSuccess,
			wantExit:   0,
		},
		{
			name:       "no pod state, job succeeded counter",
			in:         oneShotOutcomeInputs{jobSucceeded: true},
			wantStatus: datastore.OneShotStatusSuccess,
			wantExit:   0,
		},
		{
			name:       "no pod state, job failed counter",
			in:         oneShotOutcomeInputs{jobFailed: true},
			wantStatus: datastore.OneShotStatusFailed,
			wantExit:   1,
		},
		{
			// Sub-second success whose per-container exit code was torn down
			// before it could be read: the aggregate Succeeded phase resolves it
			// as success/0 instead of a false failure (the bug this fix targets).
			name:       "pod phase Succeeded without per-container exit code is success",
			in:         oneShotOutcomeInputs{podSucceeded: true},
			wantStatus: datastore.OneShotStatusSuccess,
			wantExit:   0,
		},
		{
			name:       "pod phase Failed without per-container exit code is failed/1",
			in:         oneShotOutcomeInputs{podFailed: true},
			wantStatus: datastore.OneShotStatusFailed,
			wantExit:   1,
		},
		{
			// The real exit code from a readable terminated state wins over the
			// aggregate phase when both are present.
			name:       "container terminated exit 0 beats Failed phase",
			in:         oneShotOutcomeInputs{podFailed: true, podTerminated: true, podExitCode: 0},
			wantStatus: datastore.OneShotStatusSuccess,
			wantExit:   0,
		},
		{
			name:       "wholly indeterminate is failed/1 (never a false success)",
			in:         oneShotOutcomeInputs{},
			wantStatus: datastore.OneShotStatusFailed,
			wantExit:   1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, gotExit := classifyOneShotOutcome(tc.in)
			if gotStatus != tc.wantStatus || gotExit != tc.wantExit {
				t.Fatalf("classifyOneShotOutcome(%+v) = (%q, %d), want (%q, %d)",
					tc.in, gotStatus, gotExit, tc.wantStatus, tc.wantExit)
			}
		})
	}
}

func TestLooksLikeSignalKill(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{0, false},
		{7, false},
		{127, false},
		{128, true},
		{137, true}, // SIGKILL (deadline kill)
		{143, true}, // SIGTERM
	}
	for _, tc := range cases {
		if got := looksLikeSignalKill(tc.code); got != tc.want {
			t.Errorf("looksLikeSignalKill(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}
