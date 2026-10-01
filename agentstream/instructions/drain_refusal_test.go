package instructions

import (
	"strings"
	"testing"

	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/drain"
)

// refuseWork makes beginWork behave as it does once the agent drains for a
// restart, and restores it when the test ends.
func refuseWork(t *testing.T) {
	t.Helper()
	prev := beginWork
	beginWork = func() (func(), error) { return nil, drain.ErrDraining }
	t.Cleanup(func() { beginWork = prev })
}

// While the agent drains, VCS_BUILD must refuse the build and say why. It must
// refuse before it writes a build row, because a row would never get a build.
func TestVcsBuild_RefusesWhileDraining(t *testing.T) {
	refuseWork(t)

	payload, err := commons.JsonB64Encode(VcsBuildRequest{OSID: "app-ab1cd", SHA: "abc123", JobID: "job-1"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, body, err := VcsBuild(payload)
	if err != nil {
		t.Fatalf("VcsBuild: %v", err)
	}
	var resp VcsBuildResponse
	if err := commons.JsonB64Decode(body, &resp); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if resp.Success {
		t.Fatal("VcsBuild succeeded while draining, want a refusal")
	}
	if !strings.Contains(resp.Message, "draining") {
		t.Errorf("message = %q, want it to say the agent is draining", resp.Message)
	}
}

// RUN_ONESHOT_JOB must refuse before it creates the Kubernetes Job, so a
// refused run leaves no Job and no audit row behind.
func TestRunOneShotJob_RefusesWhileDraining(t *testing.T) {
	refuseWork(t)

	payload, err := commons.JsonB64Encode(RunOneShotJobRequest{
		RunID:   "run-1",
		OSID:    "app-ab1cd",
		Image:   "registry.example.test/runos-apps/app-ab1cd:abc123",
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, body, err := RunOneShotJob(payload)
	if err != nil {
		t.Fatalf("RunOneShotJob: %v", err)
	}
	var resp RunOneShotJobResponse
	if err := commons.JsonB64Decode(body, &resp); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if resp.Success {
		t.Fatal("RunOneShotJob succeeded while draining, want a refusal")
	}
	if !strings.Contains(resp.Message, "draining") {
		t.Errorf("message = %q, want it to say the agent is draining", resp.Message)
	}
}
