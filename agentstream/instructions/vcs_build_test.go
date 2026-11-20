package instructions

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/datastore"
)

func vcsBuildFixture(t *testing.T) func() {
	t.Helper()
	prevCreate, prevScoped := createBuildKitJob, createScopedBuildKitJob
	prevPermit := RequestJobStagePermit
	prevBegin := beginWork
	return func() {
		createBuildKitJob, createScopedBuildKitJob = prevCreate, prevScoped
		RequestJobStagePermit = prevPermit
		beginWork = prevBegin
	}
}

func decodeVcsBuildResponse(t *testing.T, replyType, payload string) VcsBuildResponse {
	t.Helper()
	if replyType != "VCS_BUILD_RESPONSE" {
		t.Fatalf("replyType = %q, want VCS_BUILD_RESPONSE", replyType)
	}
	var resp VcsBuildResponse
	if err := commons.JsonB64Decode(payload, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

// TestVcsBuild_CacheBindingMismatchRefuses pins the plan's "VCS
// propagation" rule: a cache entry from a different job-authority binding
// cannot satisfy VCS_BUILD, even when OSID+SHA match. No job row is
// created and no executor runs.
func TestVcsBuild_CacheBindingMismatchRefuses(t *testing.T) {
	defer vcsBuildFixture(t)()

	createCalls, scopedCalls := 0, 0
	createBuildKitJob = func(string, string, string, string, string) error { createCalls++; return nil }
	createScopedBuildKitJob = func(string, string, string, string, string, string) error { scopedCalls++; return nil }
	executorCalled := false
	prevExecutor := VcsBuildExecutor
	VcsBuildExecutor = func(VcsBuildExecutorInput) { executorCalled = true }
	defer func() { VcsBuildExecutor = prevExecutor }()

	const osid, sha = "app-mismatch", "deadbeef"
	vcsWorkdirCachePut(osid, sha, VcsWorkdirBuildPaths{
		Workdir:                     t.TempDir(),
		AuthorizationClassification: datastore.AuthorizationScoped,
		ConductorJobID:              "00000000-0000-4000-8000-000000000001",
	})
	t.Cleanup(func() { vcsWorkdirCache.Delete(vcsWorkdirCacheKey(osid, sha)) })

	payload, err := commons.JsonB64Encode(VcsBuildRequest{
		OSID: osid, SHA: sha, JobID: sha,
		ConductorJobID: "00000000-0000-4000-8000-000000000002", // different from the cached entry
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	replyType, reply, err := VcsBuild(payload)
	if err != nil {
		t.Fatalf("VcsBuild: %v", err)
	}
	resp := decodeVcsBuildResponse(t, replyType, reply)
	if resp.Success {
		t.Fatal("expected a refusal for a mismatched job authority")
	}
	if createCalls != 0 || scopedCalls != 0 {
		t.Errorf("createCalls=%d scopedCalls=%d, want no job row created", createCalls, scopedCalls)
	}
	if executorCalled {
		t.Error("the executor must not run on a binding mismatch")
	}
}

// TestVcsBuild_QueueAdmissionRefusalCreatesNoJobRow pins "Request queue
// admission before the build row is created. ... If queue admission
// refuses, do not create or start a build job."
func TestVcsBuild_QueueAdmissionRefusalCreatesNoJobRow(t *testing.T) {
	defer vcsBuildFixture(t)()

	createCalls, scopedCalls := 0, 0
	createBuildKitJob = func(string, string, string, string, string) error { createCalls++; return nil }
	createScopedBuildKitJob = func(string, string, string, string, string, string) error { scopedCalls++; return nil }
	executorCalled := false
	prevExecutor := VcsBuildExecutor
	VcsBuildExecutor = func(VcsBuildExecutorInput) { executorCalled = true }
	defer func() { VcsBuildExecutor = prevExecutor }()

	var gotStage string
	RequestJobStagePermit = func(_ context.Context, authorityJobID, stage string) error {
		gotStage = stage
		return fmt.Errorf("denied")
	}

	const osid, sha = "app-denied", "cafebabe"
	const conductorJobID = "00000000-0000-4000-8000-000000000003"
	vcsWorkdirCachePut(osid, sha, VcsWorkdirBuildPaths{
		Workdir:                     t.TempDir(),
		AuthorizationClassification: datastore.AuthorizationScoped,
		ConductorJobID:              conductorJobID,
	})
	t.Cleanup(func() { vcsWorkdirCache.Delete(vcsWorkdirCacheKey(osid, sha)) })

	payload, err := commons.JsonB64Encode(VcsBuildRequest{
		OSID: osid, SHA: sha, JobID: sha, ConductorJobID: conductorJobID,
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	replyType, reply, err := VcsBuild(payload)
	if err != nil {
		t.Fatalf("VcsBuild: %v", err)
	}
	resp := decodeVcsBuildResponse(t, replyType, reply)
	if resp.Success {
		t.Fatal("expected a refusal for a denied queue_admission")
	}
	if gotStage != StageQueueAdmission {
		t.Errorf("stage = %q, want %q", gotStage, StageQueueAdmission)
	}
	if createCalls != 0 || scopedCalls != 0 {
		t.Errorf("createCalls=%d scopedCalls=%d, want no job row created", createCalls, scopedCalls)
	}
	if executorCalled {
		t.Error("the executor must not run after a queue_admission refusal")
	}
}

// TestVcsBuild_ScopedPermitCreatesScopedJobAndForwardsAuthority proves the
// happy path: a matching, permitted scoped build creates the job row via
// createScopedBuildKitJob (never the legacy creator) and forwards the
// SAME durable conductor job id to the executor as AuthorityJobID.
func TestVcsBuild_ScopedPermitCreatesScopedJobAndForwardsAuthority(t *testing.T) {
	defer vcsBuildFixture(t)()
	beginWork = func() (func(), error) { return func() {}, nil }

	var scopedJobID, scopedAuthorityJobID string
	createBuildKitJob = func(string, string, string, string, string) error {
		t.Fatal("must not call the legacy creator for a scoped build")
		return nil
	}
	createScopedBuildKitJob = func(jobID, authorityJobID, _, _, _, _ string) error {
		scopedJobID, scopedAuthorityJobID = jobID, authorityJobID
		return nil
	}
	RequestJobStagePermit = func(context.Context, string, string) error { return nil }

	done := make(chan VcsBuildExecutorInput, 1)
	prevExecutor := VcsBuildExecutor
	VcsBuildExecutor = func(in VcsBuildExecutorInput) { done <- in }
	defer func() { VcsBuildExecutor = prevExecutor }()

	const osid, sha = "app-scoped-ok", "0123456"
	const conductorJobID = "00000000-0000-4000-8000-000000000004"
	vcsWorkdirCachePut(osid, sha, VcsWorkdirBuildPaths{
		Workdir:                     t.TempDir(),
		AuthorizationClassification: datastore.AuthorizationScoped,
		ConductorJobID:              conductorJobID,
	})
	t.Cleanup(func() { vcsWorkdirCache.Delete(vcsWorkdirCacheKey(osid, sha)) })

	payload, err := commons.JsonB64Encode(VcsBuildRequest{
		OSID: osid, SHA: sha, JobID: sha, ConductorJobID: conductorJobID,
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	replyType, reply, err := VcsBuild(payload)
	if err != nil {
		t.Fatalf("VcsBuild: %v", err)
	}
	resp := decodeVcsBuildResponse(t, replyType, reply)
	if !resp.Success {
		t.Fatalf("expected success, message=%q", resp.Message)
	}
	if scopedJobID != resp.JobID {
		t.Errorf("job row created under %q, response names %q", scopedJobID, resp.JobID)
	}
	if scopedAuthorityJobID != conductorJobID {
		t.Errorf("authority job id persisted = %q, want %q", scopedAuthorityJobID, conductorJobID)
	}

	select {
	case in := <-done:
		if in.AuthorizationClassification != datastore.AuthorizationScoped {
			t.Errorf("executor classification = %q, want scoped", in.AuthorizationClassification)
		}
		if in.AuthorityJobID != conductorJobID {
			t.Errorf("executor authority job id = %q, want %q", in.AuthorityJobID, conductorJobID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executor was never invoked")
	}
}
