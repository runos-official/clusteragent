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
	const cachedJobID = "00000000-0000-4000-8000-000000000001"
	vcsWorkdirCachePut(osid, sha, cachedJobID, VcsWorkdirBuildPaths{
		Workdir:                     t.TempDir(),
		AuthorizationClassification: datastore.AuthorizationScoped,
		ConductorJobID:              cachedJobID,
	})
	t.Cleanup(func() { vcsWorkdirCache.Delete(vcsWorkdirCacheKey(osid, sha, cachedJobID)) })

	payload, err := commons.JsonB64Encode(VcsBuildRequest{
		OSID: osid, SHA: sha, JobID: sha,
		AuthorizationClassification: datastore.AuthorizationScoped,
		ConductorJobID:              "00000000-0000-4000-8000-000000000002", // different from the cached entry: own slot is simply absent
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

// TestVcsBuild_ScopedMissingDurableIDRefuses pins review finding 1397: an
// explicitly scoped request with no well-formed durable job id refuses
// outright (before any cache lookup), it does not fall back to legacy
// behavior.
func TestVcsBuild_ScopedMissingDurableIDRefuses(t *testing.T) {
	defer vcsBuildFixture(t)()

	called := false
	RequestJobStagePermit = func(context.Context, string, string) error {
		called = true
		return nil
	}

	payload, err := commons.JsonB64Encode(VcsBuildRequest{
		OSID: "app-missing-id", SHA: "deadbeef", JobID: "deadbeef",
		AuthorizationClassification: datastore.AuthorizationScoped,
		// ConductorJobID intentionally omitted.
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
		t.Fatal("expected a refusal for a scoped request with no durable job id")
	}
	if called {
		t.Error("must not request a permit without a valid authority id")
	}
}

// TestVcsBuild_LegacyIgnoresConductorJobID pins review finding 1397's other
// half: a request with NO explicit scoped classification never performs a
// recheck, even when ConductorJobID happens to be present (Conductor sends
// it unconditionally, including for legacy and human-initiated jobs).
func TestVcsBuild_LegacyIgnoresConductorJobID(t *testing.T) {
	defer vcsBuildFixture(t)()
	beginWork = func() (func(), error) { return func() {}, nil }

	permitCalled := false
	RequestJobStagePermit = func(context.Context, string, string) error {
		permitCalled = true
		return fmt.Errorf("must not be called")
	}
	legacyCalls := 0
	createBuildKitJob = func(string, string, string, string, string) error { legacyCalls++; return nil }
	createScopedBuildKitJob = func(string, string, string, string, string, string) error {
		t.Fatal("must not call the scoped creator for a legacy build")
		return nil
	}
	done := make(chan VcsBuildExecutorInput, 1)
	prevExecutor := VcsBuildExecutor
	VcsBuildExecutor = func(in VcsBuildExecutorInput) { done <- in }
	defer func() { VcsBuildExecutor = prevExecutor }()

	const osid, sha = "app-legacy-with-id", "cafed00d"
	// A legacy fetch ignores any raw conductorJobId too, so its slot is
	// keyed by "" regardless of what Conductor sent.
	vcsWorkdirCachePut(osid, sha, "", VcsWorkdirBuildPaths{Workdir: t.TempDir()})
	t.Cleanup(func() { vcsWorkdirCache.Delete(vcsWorkdirCacheKey(osid, sha, "")) })

	payload, err := commons.JsonB64Encode(VcsBuildRequest{
		OSID: osid, SHA: sha, JobID: sha,
		ConductorJobID: "00000000-0000-4000-8000-000000000009", // present, but classification is not scoped
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
	if permitCalled {
		t.Error("a legacy request must never call the recheck transport")
	}
	if legacyCalls != 1 {
		t.Errorf("legacyCalls = %d, want 1", legacyCalls)
	}
	select {
	case in := <-done:
		if in.AuthorizationClassification != "" || in.AuthorityJobID != "" {
			t.Errorf("executor classification=%q authorityJobID=%q, want both empty", in.AuthorizationClassification, in.AuthorityJobID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executor was never invoked")
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
	vcsWorkdirCachePut(osid, sha, conductorJobID, VcsWorkdirBuildPaths{
		Workdir:                     t.TempDir(),
		AuthorizationClassification: datastore.AuthorizationScoped,
		ConductorJobID:              conductorJobID,
	})
	t.Cleanup(func() { vcsWorkdirCache.Delete(vcsWorkdirCacheKey(osid, sha, conductorJobID)) })

	payload, err := commons.JsonB64Encode(VcsBuildRequest{
		OSID: osid, SHA: sha, JobID: sha,
		AuthorizationClassification: datastore.AuthorizationScoped,
		ConductorJobID:              conductorJobID,
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
	vcsWorkdirCachePut(osid, sha, conductorJobID, VcsWorkdirBuildPaths{
		Workdir:                     t.TempDir(),
		AuthorizationClassification: datastore.AuthorizationScoped,
		ConductorJobID:              conductorJobID,
	})
	t.Cleanup(func() { vcsWorkdirCache.Delete(vcsWorkdirCacheKey(osid, sha, conductorJobID)) })

	payload, err := commons.JsonB64Encode(VcsBuildRequest{
		OSID: osid, SHA: sha, JobID: sha,
		AuthorizationClassification: datastore.AuthorizationScoped,
		ConductorJobID:              conductorJobID,
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

// TestVcsWorkdirCache_OverlappingScopedFetchesStayIsolated pins review
// finding 1400 directly at the cache layer: two scoped fetches for the
// SAME app+SHA under DIFFERENT durable job ids must land in separate
// slots, and cleanup of either must never touch the other's.
func TestVcsWorkdirCache_OverlappingScopedFetchesStayIsolated(t *testing.T) {
	const osid, sha = "app-interleaved", "f00dface"
	const jobA, jobB = "00000000-0000-4000-8000-00000000000a", "00000000-0000-4000-8000-00000000000b"
	dirA, dirB := t.TempDir(), t.TempDir()
	t.Cleanup(func() {
		vcsWorkdirCache.Delete(vcsWorkdirCacheKey(osid, sha, jobA))
		vcsWorkdirCache.Delete(vcsWorkdirCacheKey(osid, sha, jobB))
	})

	vcsWorkdirCachePut(osid, sha, jobA, VcsWorkdirBuildPaths{
		Workdir: dirA, AuthorizationClassification: datastore.AuthorizationScoped, ConductorJobID: jobA,
	})
	// The second fetch for the SAME osid+sha under a DIFFERENT job must
	// not displace the first job's entry.
	vcsWorkdirCachePut(osid, sha, jobB, VcsWorkdirBuildPaths{
		Workdir: dirB, AuthorizationClassification: datastore.AuthorizationScoped, ConductorJobID: jobB,
	})

	gotA, okA := vcsWorkdirCacheGet(osid, sha, jobA)
	if !okA || gotA.Workdir != dirA {
		t.Fatalf("job A entry = (ok=%v, workdir=%q), want (true, %q)", okA, gotA.Workdir, dirA)
	}
	gotB, okB := vcsWorkdirCacheGet(osid, sha, jobB)
	if !okB || gotB.Workdir != dirB {
		t.Fatalf("job B entry = (ok=%v, workdir=%q), want (true, %q)", okB, gotB.Workdir, dirB)
	}

	// Cleanup of job A's slot must preserve job B's.
	vcsWorkdirCacheDeleteIfPath(osid, sha, jobA, dirA)
	if _, ok := vcsWorkdirCacheGet(osid, sha, jobA); ok {
		t.Error("job A's entry should have been removed")
	}
	if gotB, okB := vcsWorkdirCacheGet(osid, sha, jobB); !okB || gotB.Workdir != dirB {
		t.Fatalf("job B's entry was disturbed by job A's cleanup: ok=%v workdir=%q", okB, gotB.Workdir)
	}
}
