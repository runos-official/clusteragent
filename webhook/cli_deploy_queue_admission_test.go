package webhook

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/runos-official/clusteragent/agentstream"
)

// TestHandleCLIDeployUpload_QueueAdmissionRefusalStopsBeforeBuildJob pins
// story 339's plan ("CLI propagation": "If queue admission refuses, do not
// create or start a build job. The already authorized archive remains the
// only completed effect."). The archive_persist recheck (admitScopedUpload)
// permits, the token is claimed and the archive pushed, but the SECOND
// recheck call (queue_admission) denies: no build starts, and - unlike
// every other post-claim refusal in this file - no build-job row is
// recorded either, matching the plan's explicit exception.
func TestHandleCLIDeployUpload_QueueAdmissionRefusalStopsBeforeBuildJob(t *testing.T) {
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
	if len(h.failedUploadJobIDs) != 0 {
		t.Errorf("queue_admission refusal must record no build-job row, got %v", h.failedUploadJobIDs)
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
