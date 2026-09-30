package instructions

import (
	"encoding/json"
	"testing"

	"github.com/runos-official/clusteragent/datastore"

	corev1 "k8s.io/api/core/v1"
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

// oneShotContainer builds a Job from a request and returns its only container.
func oneShotContainer(t *testing.T, req RunOneShotJobRequest) corev1.Container {
	t.Helper()
	job := buildOneShotJob("runos-run-x", "myapp", req, "cm", "sec", 60)
	cs := job.Spec.Template.Spec.Containers
	if len(cs) != 1 {
		t.Fatalf("want 1 container, got %d", len(cs))
	}
	return cs[0]
}

func qty(t *testing.T, list corev1.ResourceList, name corev1.ResourceName) string {
	t.Helper()
	q, ok := list[name]
	if !ok {
		return ""
	}
	return q.String()
}

func TestOneShotResourcesFromRequest(t *testing.T) {
	c := oneShotContainer(t, RunOneShotJobRequest{
		Image: "img", Command: []string{"x"},
		Resources: &OneShotResources{
			CPURequestMc: 100, CPULimitMc: 1000,
			MemoryRequestMb: 256, MemoryLimitMb: 2048,
			EphemeralStorageRequestMb: 2048, EphemeralStorageLimitMb: 2048,
		},
	})
	r := c.Resources
	want := map[string][3]string{
		"cpu":               {"100m", "1", ""},
		"memory":            {"256Mi", "2Gi", ""},
		"ephemeral-storage": {"2Gi", "2Gi", ""},
	}
	for name, w := range want {
		if got := qty(t, r.Requests, corev1.ResourceName(name)); got != w[0] {
			t.Errorf("request %s = %q, want %q", name, got, w[0])
		}
		if got := qty(t, r.Limits, corev1.ResourceName(name)); got != w[1] {
			t.Errorf("limit %s = %q, want %q", name, got, w[1])
		}
	}
}

// An old conductor sends no block. The pod must still never be BestEffort, and
// the agent adds no limit, so a run that worked before cannot start to be killed.
func TestOneShotResourcesDefaultWhenAbsent(t *testing.T) {
	c := oneShotContainer(t, RunOneShotJobRequest{Image: "img", Command: []string{"x"}})
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
		if qty(t, c.Resources.Requests, name) == "" {
			t.Errorf("default request for %s is missing; the pod would be BestEffort", name)
		}
	}
	if len(c.Resources.Limits) != 0 {
		t.Errorf("default must add no limits, got %v", c.Resources.Limits)
	}
}

func TestOneShotResourcesPartialAndInconsistent(t *testing.T) {
	t.Run("limit only: request follows the limit for storage, defaults for the rest", func(t *testing.T) {
		c := oneShotContainer(t, RunOneShotJobRequest{Resources: &OneShotResources{EphemeralStorageLimitMb: 1024, MemoryLimitMb: 512}})
		if got := qty(t, c.Resources.Requests, corev1.ResourceEphemeralStorage); got != "1Gi" {
			t.Errorf("storage request = %q, want 1Gi", got)
		}
		if qty(t, c.Resources.Requests, corev1.ResourceCPU) == "" {
			t.Error("cpu request must fall back to the default")
		}
	})
	t.Run("request above limit is lowered to the limit so the API accepts the Job", func(t *testing.T) {
		c := oneShotContainer(t, RunOneShotJobRequest{Resources: &OneShotResources{MemoryRequestMb: 4096, MemoryLimitMb: 512}})
		if got := qty(t, c.Resources.Requests, corev1.ResourceMemory); got != "512Mi" {
			t.Errorf("memory request = %q, want 512Mi", got)
		}
	})
	t.Run("negative values count as unset", func(t *testing.T) {
		c := oneShotContainer(t, RunOneShotJobRequest{Resources: &OneShotResources{CPULimitMc: -5, MemoryLimitMb: -1}})
		if len(c.Resources.Limits) != 0 {
			t.Errorf("negative limits must be ignored, got %v", c.Resources.Limits)
		}
	})
}

// A new conductor may send fields this agent does not know, and an old conductor
// sends no block: both must decode.
func TestOneShotRequestJSONCompat(t *testing.T) {
	var old RunOneShotJobRequest
	if err := json.Unmarshal([]byte(`{"runId":"r","osid":"o","image":"i","command":["c"]}`), &old); err != nil || old.Resources != nil {
		t.Fatalf("old request: err=%v resources=%v", err, old.Resources)
	}
	var newer RunOneShotJobRequest
	body := `{"runId":"r","resources":{"cpuLimitMc":500,"futureField":1},"futureTop":true}`
	if err := json.Unmarshal([]byte(body), &newer); err != nil || newer.Resources == nil || newer.Resources.CPULimitMc != 500 {
		t.Fatalf("new request: err=%v resources=%+v", err, newer.Resources)
	}
}
