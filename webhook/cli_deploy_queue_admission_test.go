package webhook

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/runos-official/clusteragent/agentstream"
)

// TestHandleCLIDeployUpload_QueueAdmissionRefusalRecordsPollableFailure
// pins review findings 1395/1402: the archive_persist recheck
// (admitScopedUpload) permits, the token is claimed and the archive
// pushed, but the SECOND recheck call (queue_admission) denies. No
// goroutine starts and no RUNNABLE build job is created - but, unlike a
// pre-claim refusal, this one is unconditionally terminal (the token is
// already consumed), so it MUST still leave a pollable failed row under
// uploadID, exactly like every other post-claim refusal in this handler;
// otherwise Conductor's deploy job waits out its whole poll budget at
// "Wait for build" (FCR 744) instead of failing with a reason.
func TestHandleCLIDeployUpload_QueueAdmissionRefusalRecordsPollableFailure(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          scopedUploadToken(),
		buildkitConfig: &agentstream.BuildKitConfig{},
	}
	h.install()

	prevRecheck := requestStageRecheck
	t.Cleanup(func() { requestStageRecheck = prevRecheck })
	requestStageRecheck = func(_ context.Context, authority agentstream.RecheckAuthority, stage agentstream.RecheckStage, _ string) (*agentstream.StageResponse, error) {
		if stage == agentstream.StageArchivePersist {
			return &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, Code: "ok", ExpiresAt: time.Now().Add(5 * time.Second)}, nil
		}
		return &agentstream.StageResponse{Outcome: agentstream.OutcomeDeny, Code: "queue_admission_denied"}, nil
	}

	w := doUpload(t, validTarGz(t))
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claimed) != 1 {
		t.Fatalf("claimed = %v, want exactly one claim (the archive is the only authorized effect)", h.claimed)
	}
	if !h.pushed {
		t.Error("expected the archive to be pushed (the archive_persist permit was granted)")
	}
	if h.cliStarted {
		t.Error("CLI deployment build must not start when queue_admission refuses")
	}
	if len(h.failedUploadJobIDs) != 1 || h.failedUploadJobIDs[0] != "00000000-0000-4000-8000-000000000002" {
		t.Errorf("queue_admission refusal must record exactly one pollable failed row under uploadID, got %v", h.failedUploadJobIDs)
	}
	if w.Code == http.StatusAccepted {
		t.Errorf("status = %d, want a refusal status, not 202", w.Code)
	}
}

// TestHandleCLIDeployUpload_ScopedPermitForwardsAuthorityToAsyncBuild proves
// a scoped upload that clears both the archive_persist AND queue_admission
// rechecks forwards the token's own classification and durable job id into
// the async build (never the upload binding's bindingId/uploadId - those
// are archive_persist-only authority, per handbook 949 C3's closed stage
// authority union).
func TestHandleCLIDeployUpload_ScopedPermitForwardsAuthorityToAsyncBuild(t *testing.T) {
	token := scopedUploadToken()
	h := &uploadHarness{
		t:              t,
		token:          token,
		buildkitConfig: &agentstream.BuildKitConfig{},
		recheckResp:    &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, Code: "ok", ExpiresAt: time.Now().Add(5 * time.Second)},
	}
	h.install()

	w := doUpload(t, validTarGz(t))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", w.Code, w.Body.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.cliStarted {
		t.Fatal("expected the CLI deployment build to start")
	}
	if h.cliStartedClassification != token.AuthorizationClassification {
		t.Errorf("forwarded classification = %q, want %q", h.cliStartedClassification, token.AuthorizationClassification)
	}
	if h.cliStartedAuthorityJobID != token.DurableJobID {
		t.Errorf("forwarded authority job id = %q, want %q", h.cliStartedAuthorityJobID, token.DurableJobID)
	}
}

// TestHandleCLIDeployUpload_ScopedWithoutDurableJobIDRefusesAtQueueAdmission
// defends in depth against review finding 1397's regression: datastore.
// CreateScopedUploadToken now refuses to mint a scoped token with no
// durable job id, so this shape should not reach the handler in practice -
// but a malformed or legacy-written row could still carry it, and the
// handler must still fail closed (refuse, no job row, no goroutine)
// rather than silently treating the missing id as "nothing to recheck,
// proceed like legacy". requireJobStagePermit's own authority-shape
// validation (agentstream.JobAuthority) is what actually refuses here.
func TestHandleCLIDeployUpload_ScopedWithoutDurableJobIDRefusesAtQueueAdmission(t *testing.T) {
	token := scopedUploadToken()
	token.DurableJobID = ""
	h := &uploadHarness{
		t:              t,
		token:          token,
		buildkitConfig: &agentstream.BuildKitConfig{},
	}
	h.install()

	calls := 0
	prevRecheck := requestStageRecheck
	t.Cleanup(func() { requestStageRecheck = prevRecheck })
	requestStageRecheck = func(_ context.Context, authority agentstream.RecheckAuthority, stage agentstream.RecheckStage, _ string) (*agentstream.StageResponse, error) {
		calls++
		return &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, Code: "ok", ExpiresAt: time.Now().Add(5 * time.Second)}, nil
	}

	w := doUpload(t, validTarGz(t))
	if w.Code == http.StatusAccepted {
		t.Fatalf("status = %d, want a refusal, not 202", w.Code)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cliStarted {
		t.Error("CLI deployment build must not start with a scoped token that carries no durable job id")
	}
	// archive_persist alone may have been requested (classification is
	// scoped), but queue_admission must never be, since there is no valid
	// authority to present for it.
	if calls > 1 {
		t.Errorf("recheck calls = %d, want at most 1 (archive_persist only; queue_admission must be skipped on an invalid id)", calls)
	}
}

// TestHandleCLIDeployUpload_LegacyUploadNeverCallsQueueAdmission proves an
// explicit legacy upload never calls the recheck transport at all - not at
// archive_persist (already covered elsewhere) and not at queue_admission -
// and forwards an empty classification/authority job id to the async
// build, matching the explicit no-fallback discipline every other scoped/
// legacy boundary in this codebase uses.
func TestHandleCLIDeployUpload_LegacyUploadNeverCallsQueueAdmission(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          legacyUploadToken(),
		buildkitConfig: &agentstream.BuildKitConfig{},
	}
	h.install()

	prevRecheck := requestStageRecheck
	t.Cleanup(func() { requestStageRecheck = prevRecheck })
	called := false
	requestStageRecheck = func(context.Context, agentstream.RecheckAuthority, agentstream.RecheckStage, string) (*agentstream.StageResponse, error) {
		called = true
		return &agentstream.StageResponse{Outcome: agentstream.OutcomeDeny, Code: "must not be called"}, nil
	}

	w := doUpload(t, validTarGz(t))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", w.Code, w.Body.String())
	}
	if called {
		t.Error("a legacy upload must never call the recheck transport")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.cliStarted {
		t.Fatal("expected the CLI deployment build to start")
	}
	if h.cliStartedClassification != "legacy" || h.cliStartedAuthorityJobID != "" {
		t.Errorf("legacy upload forwarded classification=%q authorityJobID=%q, want (\"legacy\", \"\")", h.cliStartedClassification, h.cliStartedAuthorityJobID)
	}
}
