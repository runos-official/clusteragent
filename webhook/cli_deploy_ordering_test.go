package webhook

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/datastore"
	"github.com/runos-official/clusteragent/harborclient"
)

// validTarGz returns a minimal valid gzipped tarball with one small file, so
// it clears checkTarballLimits and the subsequent gzip/tar reads done by the
// handler under test. Ordering tests need admission logic to actually run,
// not fail earlier on an invalid body.
func validTarGz(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	body := []byte("FROM alpine")
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Typeflag: tar.TypeReg, Size: int64(len(body)), Mode: 0o644}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// uploadHarness overrides every injected collaborator HandleCLIDeployUpload
// uses past the drain check, and records what actually ran. Story 338's
// whole point is ordering: the claim, the archive push, and the build start
// must never happen before every available check (and, for a scoped token,
// the fresh recheck) has passed.
type uploadHarness struct {
	t *testing.T

	token          *datastore.UploadToken
	loadErr        error
	buildkitConfig *agentstream.BuildKitConfig
	configErr      error
	recheckResp    *agentstream.StageResponse
	recheckErr     error
	pushErr        error

	mu                          sync.Mutex
	claimed                     []string
	claimedPermitDeadline       []*time.Time
	claimErr                    error
	pushed                      bool
	cliStarted                  bool
	buildImgRun                 bool
	failedUploadJobIDs          []string
	skipFailedUploadIfUnclaimed bool
	skippedFailedUploadJobIDs   []string
	configReads                 int
	configVerifiedReads         int
}

func (h *uploadHarness) install() {
	h.t.Helper()

	prevLoad, prevClaim, prevConfig, prevRecheck, prevPush := loadUploadableToken, claimUploadToken, readBuildKitConfig, requestStageRecheck, pushArchive
	prevConfigVerified := readBuildKitConfigVerified
	prevCLI, prevBuildImg := startCLIDeploymentAsync, startBuildImageDeploymentAsync
	prevBegin := beginWork
	prevRecordFailed := recordFailedUpload
	prevRecordFailedIfUnclaimed := recordFailedUploadIfUnclaimed
	h.t.Cleanup(func() {
		loadUploadableToken, claimUploadToken, readBuildKitConfig, requestStageRecheck, pushArchive = prevLoad, prevClaim, prevConfig, prevRecheck, prevPush
		readBuildKitConfigVerified = prevConfigVerified
		startCLIDeploymentAsync, startBuildImageDeploymentAsync = prevCLI, prevBuildImg
		beginWork = prevBegin
		recordFailedUpload = prevRecordFailed
		recordFailedUploadIfUnclaimed = prevRecordFailedIfUnclaimed
	})

	// Review findings 1075/1086/1133: a RETRYABLE pre-claim refusal must
	// never reserve the token's (fixed, retryable) uploadID as a
	// build-job row, but a TERMINAL one must record exactly one. Track
	// every call so tests can assert either shape.
	recordFailedUpload = func(jobID, _, _ string) error {
		h.mu.Lock()
		h.failedUploadJobIDs = append(h.failedUploadJobIDs, jobID)
		h.mu.Unlock()
		return nil
	}

	// Review finding 1267: the pre-claim-safe recorder. Defaults to the
	// same tracking as recordFailedUpload and "not claimed by someone
	// else", so every existing test's assumptions keep holding; a test for
	// finding 1267 itself overrides h.unclaimedCheck to simulate a
	// concurrent winner.
	recordFailedUploadIfUnclaimed = func(_, jobID, _, _ string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.skipFailedUploadIfUnclaimed {
			h.skippedFailedUploadJobIDs = append(h.skippedFailedUploadJobIDs, jobID)
			return nil
		}
		h.failedUploadJobIDs = append(h.failedUploadJobIDs, jobID)
		return nil
	}

	beginWork = func() (func(), error) { return func() {}, nil }

	loadUploadableToken = func(string) (*datastore.UploadToken, error) {
		if h.loadErr != nil {
			return nil, h.loadErr
		}
		return h.token, nil
	}
	claimUploadToken = func(token string, permitDeadline *time.Time) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.claimErr != nil {
			return h.claimErr
		}
		h.claimed = append(h.claimed, token)
		h.claimedPermitDeadline = append(h.claimedPermitDeadline, permitDeadline)
		return nil
	}
	readBuildKitConfig = func(context.Context) (*agentstream.BuildKitConfig, error) {
		h.mu.Lock()
		h.configReads++
		h.mu.Unlock()
		if h.configErr != nil {
			return nil, h.configErr
		}
		return h.buildkitConfig, nil
	}
	readBuildKitConfigVerified = func(context.Context) (*agentstream.BuildKitConfig, error) {
		h.mu.Lock()
		h.configVerifiedReads++
		h.mu.Unlock()
		if h.configErr != nil {
			return nil, h.configErr
		}
		return h.buildkitConfig, nil
	}
	requestStageRecheck = func(context.Context, agentstream.RecheckAuthority, agentstream.RecheckStage, string) (*agentstream.StageResponse, error) {
		return h.recheckResp, h.recheckErr
	}
	pushArchive = func(context.Context, harborclient.Config, string, string, []byte) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.pushed = true
		return h.pushErr
	}
	startCLIDeploymentAsync = func(_ []byte, _, _, _ string, _ []datastore.BuildArg, _ *agentstream.BuildKitConfig, end func()) {
		h.mu.Lock()
		h.cliStarted = true
		h.mu.Unlock()
		end()
	}
	startBuildImageDeploymentAsync = func(_ []byte, _, _, _ string, _ []string, _ string, _ []datastore.BuildArg, _ *agentstream.BuildKitConfig, end func()) {
		h.mu.Lock()
		h.buildImgRun = true
		h.mu.Unlock()
		end()
	}
}

func (h *uploadHarness) assertNoClaimPushOrBuild(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claimed) != 0 {
		t.Errorf("token was claimed on a refused request: %v", h.claimed)
	}
	if h.pushed {
		t.Error("archive was pushed on a refused request")
	}
	if h.cliStarted {
		t.Error("CLI deployment build was started on a refused request")
	}
	if h.buildImgRun {
		t.Error("build-image build was started on a refused request")
	}
}

// assertNoEffects is for a RETRYABLE refusal (review findings 1075/1086): no
// claim/push/build, and no build-job row - a row here would poison a later
// legitimate retry with the same token/uploadID.
func (h *uploadHarness) assertNoEffects(t *testing.T) {
	t.Helper()
	h.assertNoClaimPushOrBuild(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.failedUploadJobIDs) != 0 {
		t.Errorf("a retryable refusal must not reserve a build-job row under the token's uploadID, got %v", h.failedUploadJobIDs)
	}
}

// assertTerminalRefusal is for a TERMINAL refusal (review finding 1133): no
// claim/push/build, but exactly one recorded build-job row under the
// token's uploadID, so Conductor's deploy job gets a pollable reason
// instead of waiting out its whole poll budget (FCR 744).
func (h *uploadHarness) assertTerminalRefusal(t *testing.T) {
	t.Helper()
	h.assertNoClaimPushOrBuild(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.failedUploadJobIDs) != 1 {
		t.Errorf("a terminal refusal must record exactly one build-job row, got %v", h.failedUploadJobIDs)
	}
}

func doUpload(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/cli-deploy/tok-fixture", bytes.NewReader(body))
	w := httptest.NewRecorder()
	HandleCLIDeployUpload(w, req)
	return w
}

func scopedUploadToken() *datastore.UploadToken {
	return &datastore.UploadToken{
		DeployConfig:                "app-1:00000000-0000-4000-8000-000000000002",
		ExpiresAt:                   time.Now().Add(5 * time.Minute),
		Purpose:                     datastore.PurposeUpload,
		AuthorizationClassification: datastore.AuthorizationScoped,
		AuthorizationBinding: &datastore.UploadAuthorityBinding{
			BindingID: "00000000-0000-4000-8000-000000000001",
			UploadID:  "00000000-0000-4000-8000-000000000002",
		},
	}
}

func legacyUploadToken() *datastore.UploadToken {
	return &datastore.UploadToken{
		DeployConfig:                "app-1:00000000-0000-4000-8000-000000000002",
		ExpiresAt:                   time.Now().Add(5 * time.Minute),
		Purpose:                     datastore.PurposeUpload,
		AuthorizationClassification: datastore.AuthorizationLegacy,
	}
}

func buildImageToken(harborOsid string) *datastore.UploadToken {
	return &datastore.UploadToken{
		DeployConfig:                "repo-1:00000000-0000-4000-8000-000000000002",
		ExpiresAt:                   time.Now().Add(5 * time.Minute),
		Purpose:                     datastore.PurposeBuildImage,
		AuthorizationClassification: datastore.AuthorizationLegacy,
		BuildTarget:                 &datastore.BuildTargetSpec{Repo: "repo-1", Tags: []string{"latest"}, HarborOsid: harborOsid},
	}
}

// TestHandleCLIDeployUpload_ScopedPermitClaimsAndProceeds: the happy path. A
// scoped token with a permit response is claimed exactly once, the archive
// is pushed, and the build starts.
func TestHandleCLIDeployUpload_ScopedPermitClaimsAndProceeds(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          scopedUploadToken(),
		buildkitConfig: &agentstream.BuildKitConfig{},
		recheckResp:    &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, Code: "ok", ExpiresAt: time.Now().Add(5 * time.Second), Lifetime: agentstream.NewTestLifetime(true)},
	}
	h.install()

	w := doUpload(t, validTarGz(t))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", w.Code, w.Body.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claimed) != 1 {
		t.Fatalf("claimed = %v, want exactly one claim", h.claimed)
	}
	if !h.pushed {
		t.Error("expected the archive to be pushed")
	}
	if !h.cliStarted {
		t.Error("expected the CLI deployment build to start")
	}
}

// TestHandleCLIDeployUpload_PermitExpiresDuringClaimRefusesBeforePersistence
// pins review finding 1128's "Check the permit again at the start of
// archive persistence": the permit is valid when checked before the claim,
// but the claim itself (a datastore round trip) takes long enough for the
// permit's own short deadline to pass. The token's own expiry is
// unaffected (it is still claimed, correctly, since the permit's deadline
// is a separate and much shorter window), but the re-check right before
// the push must still catch the now-expired permit and refuse: no archive
// push, no build start. Because the token is already claimed by then, this
// is unconditionally terminal (one recorded row), unlike the pre-claim
// permit check.
func TestHandleCLIDeployUpload_PermitExpiresDuringClaimRefusesBeforePersistence(t *testing.T) {
	deadline := time.Now().Add(2 * time.Second)
	h := &uploadHarness{
		t:              t,
		token:          scopedUploadToken(),
		buildkitConfig: &agentstream.BuildKitConfig{},
		recheckResp:    &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, Code: "ok", ExpiresAt: deadline, Lifetime: agentstream.NewTestLifetime(true)},
	}
	h.install()

	prevClock := permitClock
	t.Cleanup(func() { permitClock = prevClock })
	var clockAdvanced bool
	permitClock = func() time.Time {
		if clockAdvanced {
			return deadline.Add(1 * time.Second) // past the permit's deadline
		}
		return deadline.Add(-1 * time.Second) // still valid, for the pre-claim check
	}

	prevClaim := claimUploadToken
	t.Cleanup(func() { claimUploadToken = prevClaim })
	claimUploadToken = func(token string, _ *time.Time) error {
		clockAdvanced = true // simulate the permit's deadline passing during the claim round trip
		h.mu.Lock()
		h.claimed = append(h.claimed, token)
		h.mu.Unlock()
		return nil
	}

	w := doUpload(t, validTarGz(t))
	if w.Code == http.StatusAccepted {
		t.Fatalf("expected a refusal once the permit expires before persistence, got 202")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claimed) != 1 {
		t.Errorf("claimed = %v, want exactly one claim (the token itself was still valid)", h.claimed)
	}
	if h.pushed {
		t.Error("archive must not be pushed once the permit expired before persistence")
	}
	if h.cliStarted {
		t.Error("build must not start once the permit expired before persistence")
	}
	if len(h.failedUploadJobIDs) != 1 {
		t.Errorf("expected exactly one recorded row (the token is already claimed, so this is terminal), got %v", h.failedUploadJobIDs)
	}
}

// TestHandleCLIDeployUpload_StreamReconnectsDuringClaimRefusesBeforePersistence
// pins review finding 1313 (repeat of 1270/1240/1169): a permit received on
// one control-stream connection must not authorize persistence once that
// connection has disconnected and a replacement has connected, even though
// the permit's own expiry is still in the future. The permit payload
// carries no connection identity by itself; permitStillValid checks the
// Lifetime that delivered the permit directly.
func TestHandleCLIDeployUpload_StreamReconnectsDuringClaimRefusesBeforePersistence(t *testing.T) {
	deadline := time.Now().Add(1 * time.Hour) // plenty of time left on the permit itself
	lt := agentstream.NewTestLifetime(true)
	h := &uploadHarness{
		t:              t,
		token:          scopedUploadToken(),
		buildkitConfig: &agentstream.BuildKitConfig{},
		recheckResp:    &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, Code: "ok", ExpiresAt: deadline, Lifetime: lt},
	}
	h.install()

	prevClaim := claimUploadToken
	t.Cleanup(func() { claimUploadToken = prevClaim })
	claimUploadToken = func(token string, _ *time.Time) error {
		agentstream.EndTestLifetime(lt) // simulate a disconnect and reconnect during the claim round trip
		h.mu.Lock()
		h.claimed = append(h.claimed, token)
		h.mu.Unlock()
		return nil
	}

	w := doUpload(t, validTarGz(t))
	if w.Code == http.StatusAccepted {
		t.Fatalf("expected a refusal once the stream has reconnected before persistence, got 202")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claimed) != 1 {
		t.Errorf("claimed = %v, want exactly one claim (the token itself was still valid)", h.claimed)
	}
	if h.pushed {
		t.Error("archive must not be pushed once the stream reconnected before persistence")
	}
	if h.cliStarted {
		t.Error("build must not start once the stream reconnected before persistence")
	}
}

// TestHandleCLIDeployUpload_LiveLifetimeProceeds proves the legitimate case
// for review finding 1313 (repeat of 1270/1240/1169): when the control
// stream never disconnects, the delivering Lifetime stays live, and the
// upload proceeds exactly as it would have before this check existed.
// Replaces TestHandleCLIDeployUpload_SameStreamGenerationProceeds
// (generation counter replaced by Lifetime).
func TestHandleCLIDeployUpload_LiveLifetimeProceeds(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          scopedUploadToken(),
		buildkitConfig: &agentstream.BuildKitConfig{},
		recheckResp:    &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, Code: "ok", ExpiresAt: time.Now().Add(5 * time.Second), Lifetime: agentstream.NewTestLifetime(true)},
	}
	h.install()

	w := doUpload(t, validTarGz(t))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", w.Code, w.Body.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claimed) != 1 || !h.pushed || !h.cliStarted {
		t.Fatalf("expected the same-generation upload to proceed: claimed=%v pushed=%v cliStarted=%v", h.claimed, h.pushed, h.cliStarted)
	}
}

// TestHandleCLIDeployUpload_ScopedDenyRefusesWithoutAnyEffect pins AC4: a
// denial must stop before persistence, with no claim, no push, no build.
func TestHandleCLIDeployUpload_ScopedDenyRefusesWithoutAnyEffect(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          scopedUploadToken(),
		buildkitConfig: &agentstream.BuildKitConfig{},
		recheckResp:    &agentstream.StageResponse{Outcome: agentstream.OutcomeDeny, Code: "revoked"},
	}
	h.install()

	w := doUpload(t, validTarGz(t))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %q)", w.Code, w.Body.String())
	}
	// A denial is terminal (review finding 1133): retrying the same request
	// will not change the outcome, so Conductor's deploy job needs a
	// pollable reason instead of waiting out its whole poll budget.
	h.assertTerminalRefusal(t)
}

// TestHandleCLIDeployUpload_ScopedOutageRefusesWithoutAnyEffect pins AC4's
// "disconnect, timeout and recheck outage stop before persistence".
func TestHandleCLIDeployUpload_ScopedOutageRefusesWithoutAnyEffect(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          scopedUploadToken(),
		buildkitConfig: &agentstream.BuildKitConfig{},
		recheckErr:     context.DeadlineExceeded,
	}
	h.install()

	w := doUpload(t, validTarGz(t))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %q)", w.Code, w.Body.String())
	}
	h.assertNoEffects(t)
}

// TestHandleCLIDeployUpload_ScopedMissingBindingFailsClosed: if the loader
// ever hands back a scoped token with no binding (an invariant it should
// never violate), the handler must still fail closed rather than treat it
// as legacy.
func TestHandleCLIDeployUpload_ScopedMissingBindingFailsClosed(t *testing.T) {
	tok := scopedUploadToken()
	tok.AuthorizationBinding = nil
	h := &uploadHarness{
		t:              t,
		token:          tok,
		buildkitConfig: &agentstream.BuildKitConfig{},
	}
	h.install()

	w := doUpload(t, validTarGz(t))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %q)", w.Code, w.Body.String())
	}
	h.assertNoEffects(t)
}

// TestHandleCLIDeployUpload_LegacyUploadSkipsRecheck pins "Preserve explicit
// legacy token compatibility without scoped-to-legacy fallback": a legacy
// token must never call the recheck transport at all, and must still
// proceed to claim, push, and build.
func TestHandleCLIDeployUpload_LegacyUploadSkipsRecheck(t *testing.T) {
	recheckCalled := false
	h := &uploadHarness{
		t:              t,
		token:          legacyUploadToken(),
		buildkitConfig: &agentstream.BuildKitConfig{},
	}
	h.install()
	prev := requestStageRecheck
	requestStageRecheck = func(context.Context, agentstream.RecheckAuthority, agentstream.RecheckStage, string) (*agentstream.StageResponse, error) {
		recheckCalled = true
		return &agentstream.StageResponse{Outcome: agentstream.OutcomePermit}, nil
	}
	t.Cleanup(func() { requestStageRecheck = prev })

	w := doUpload(t, validTarGz(t))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", w.Code, w.Body.String())
	}
	if recheckCalled {
		t.Error("a legacy token must never call the recheck transport")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claimed) != 1 {
		t.Errorf("claimed = %v, want exactly one claim", h.claimed)
	}
	if !h.pushed || !h.cliStarted {
		t.Error("expected the legacy upload to proceed to push and build")
	}
}

// TestHandleCLIDeployUpload_TarballLimitFailureDoesNotClaimToken is the
// regression test for the bug the plan names: "The early upload claim burns
// a valid token when later body, tar, or configuration checks fail." A
// tarball that fails the extraction limits must leave the token unclaimed.
func TestHandleCLIDeployUpload_TarballLimitFailureDoesNotClaimToken(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          legacyUploadToken(),
		buildkitConfig: &agentstream.BuildKitConfig{},
	}
	h.install()

	origMaxFile := MaxFileSize
	MaxFileSize = 1 // force checkTarballLimits to refuse any non-trivial tarball
	t.Cleanup(func() { MaxFileSize = origMaxFile })

	w := doUpload(t, validTarGz(t))
	if w.Code == http.StatusAccepted {
		t.Fatalf("expected a refusal, got 202")
	}
	// Terminal (review finding 1133): the same tarball fails the same way
	// on every retry, so Conductor's deploy job needs a pollable reason.
	h.assertTerminalRefusal(t)
}

// TestHandleCLIDeployUpload_ConfigReadFailureDoesNotClaimToken: the same
// ordering guarantee for a buildkit-configuration read failure.
func TestHandleCLIDeployUpload_ConfigReadFailureDoesNotClaimToken(t *testing.T) {
	h := &uploadHarness{
		t:         t,
		token:     legacyUploadToken(),
		configErr: context.DeadlineExceeded,
	}
	h.install()

	w := doUpload(t, validTarGz(t))
	if w.Code == http.StatusAccepted {
		t.Fatalf("expected a refusal, got 202")
	}
	h.assertNoEffects(t)
}

// TestHandleCLIDeployUpload_InvalidDockerfileTargetDoesNotClaimToken pins
// review finding 1078: a configured Dockerfile target that does not exist
// in the tarball must refuse before claim, archive persist, or build
// start - not only later, inside the asynchronous build.
func TestHandleCLIDeployUpload_InvalidDockerfileTargetDoesNotClaimToken(t *testing.T) {
	tok := legacyUploadToken()
	tok.Dockerfile = "apps/api/Dockerfile" // validTarGz(t) only has a root "Dockerfile"
	h := &uploadHarness{
		t:              t,
		token:          tok,
		buildkitConfig: &agentstream.BuildKitConfig{},
	}
	h.install()

	w := doUpload(t, validTarGz(t))
	if w.Code == http.StatusAccepted {
		t.Fatalf("expected a refusal, got 202")
	}
	// Terminal (review finding 1133): the same tarball/Dockerfile pairing
	// fails the same way on every retry.
	h.assertTerminalRefusal(t)
}

// TestHandleCLIDeployUpload_ConfiguredDockerfileTargetProceeds proves a
// legitimate, present Dockerfile target still passes the new preflight
// check (rework guidance: every new guard needs a legitimate-request test).
func TestHandleCLIDeployUpload_ConfiguredDockerfileTargetProceeds(t *testing.T) {
	tok := legacyUploadToken()
	tok.Dockerfile = "Dockerfile" // matches validTarGz(t)'s root entry
	h := &uploadHarness{
		t:              t,
		token:          tok,
		buildkitConfig: &agentstream.BuildKitConfig{},
	}
	h.install()

	w := doUpload(t, validTarGz(t))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", w.Code, w.Body.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claimed) != 1 || !h.pushed || !h.cliStarted {
		t.Fatalf("expected the upload to proceed: claimed=%v pushed=%v cliStarted=%v", h.claimed, h.pushed, h.cliStarted)
	}
}

// TestHandleCLIDeployUpload_RefusalThenRetryWithSameTokenSucceeds is the
// direct regression for review findings 1075/1086: before the fix, a
// pre-claim refusal (here, a transient config-read error) called failUpload,
// which reserved a build-job row under the token's fixed uploadID via a
// unique index. The token was never claimed, so a legitimate second request
// with the SAME token must still succeed end to end - claim, push, and
// build - which it cannot do if the first refusal already poisoned that
// uploadID's job row.
func TestHandleCLIDeployUpload_RefusalThenRetryWithSameTokenSucceeds(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          legacyUploadToken(),
		buildkitConfig: &agentstream.BuildKitConfig{},
		configErr:      context.DeadlineExceeded,
	}
	h.install()

	// First attempt: transient config-read failure, refused before claim.
	w1 := doUpload(t, validTarGz(t))
	if w1.Code == http.StatusAccepted {
		t.Fatalf("expected the first attempt to be refused, got 202")
	}
	h.assertNoEffects(t)

	// Second attempt, same token: the transient failure is gone now.
	h.configErr = nil
	w2 := doUpload(t, validTarGz(t))
	if w2.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 on the retry (body %q)", w2.Code, w2.Body.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claimed) != 1 {
		t.Errorf("claimed = %v, want exactly one claim on the retry", h.claimed)
	}
	if !h.pushed || !h.cliStarted {
		t.Error("expected the retry to push the archive and start the build")
	}
	if len(h.failedUploadJobIDs) != 0 {
		t.Errorf("the refused first attempt must not have reserved a build-job row, got %v", h.failedUploadJobIDs)
	}
}

// TestHandleCLIDeployUpload_TerminalRefusalThenFixedRetrySucceeds pins
// review finding 1133's own closure test: a TERMINAL refusal (here, a
// tarball over the per-file cap) records a build-job row under the token's
// uploadID. A second attempt with the SAME token and uploadID, now with a
// tarball that passes, must still claim, push, and start its build - the
// recorded row from the first attempt must not block it. The datastore
// package's own test proves CreateBuildKitJob's upsert handles the real
// unique-index collision; this test proves the handler drives that retry
// correctly end to end.
func TestHandleCLIDeployUpload_TerminalRefusalThenFixedRetrySucceeds(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          legacyUploadToken(),
		buildkitConfig: &agentstream.BuildKitConfig{},
	}
	h.install()

	origMaxFile := MaxFileSize
	MaxFileSize = 1
	w1 := doUpload(t, validTarGz(t))
	MaxFileSize = origMaxFile
	if w1.Code == http.StatusAccepted {
		t.Fatalf("expected the first attempt to be refused, got 202")
	}
	h.assertTerminalRefusal(t)

	w2 := doUpload(t, validTarGz(t))
	if w2.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 on the fixed retry (body %q)", w2.Code, w2.Body.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claimed) != 1 {
		t.Errorf("claimed = %v, want exactly one claim on the retry", h.claimed)
	}
	if !h.pushed || !h.cliStarted {
		t.Error("expected the fixed retry to push the archive and start the build")
	}
}

// TestHandleCLIDeployUpload_TerminalRefusalNeverOverwritesAConcurrentWinner
// pins review finding 1267: a terminal PRE-claim refusal must route
// through the ownership-safe recorder (recordFailedUploadIfUnclaimed), not
// the unconditional one, so a request whose token a concurrent, faster
// request already claimed never overwrites that winner's build row. The
// mock here stands in for "a concurrent winner already claimed this
// token": it is the ownership-safe path's own job (proven for real against
// PostgreSQL by
// TestRecordFailedUploadIfUnclaimed_PostgresLosesRaceToRealClaim in the
// datastore package) to decide whether to skip; this test only pins that
// the HANDLER calls that path, with the right token, for every terminal
// pre-claim refusal.
func TestHandleCLIDeployUpload_TerminalRefusalNeverOverwritesAConcurrentWinner(t *testing.T) {
	h := &uploadHarness{
		t:                           t,
		token:                       legacyUploadToken(),
		buildkitConfig:              &agentstream.BuildKitConfig{},
		skipFailedUploadIfUnclaimed: true,
	}
	h.install()

	origMaxFile := MaxFileSize
	MaxFileSize = 1
	w := doUpload(t, validTarGz(t))
	MaxFileSize = origMaxFile
	if w.Code == http.StatusAccepted {
		t.Fatalf("expected the refusal, got 202")
	}

	h.assertNoClaimPushOrBuild(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.failedUploadJobIDs) != 0 {
		t.Errorf("the unconditional recorder must not be used for a pre-claim refusal, got %v", h.failedUploadJobIDs)
	}
	if len(h.skippedFailedUploadJobIDs) != 1 {
		t.Fatalf("expected the ownership-safe recorder to run exactly once, got %v", h.skippedFailedUploadJobIDs)
	}
	if h.skippedFailedUploadJobIDs[0] != "00000000-0000-4000-8000-000000000002" {
		t.Errorf("recorded uploadID = %q, want the token's own uploadID", h.skippedFailedUploadJobIDs[0])
	}
}

// TestHandleCLIDeployUpload_ConcurrentClaimRaceOnlyOnePersists pins AC5:
// "Concurrent consumption cannot bypass single-use binding." Both requests
// pass every check and receive a permit; only the first claim may succeed,
// and only that one request may push or build.
func TestHandleCLIDeployUpload_ConcurrentClaimRaceOnlyOnePersists(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          scopedUploadToken(),
		buildkitConfig: &agentstream.BuildKitConfig{},
		recheckResp:    &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, Code: "ok", ExpiresAt: time.Now().Add(5 * time.Second), Lifetime: agentstream.NewTestLifetime(true)},
	}
	h.install()

	var claimAttempts int
	var mu sync.Mutex
	claimUploadToken = func(string, *time.Time) error {
		mu.Lock()
		defer mu.Unlock()
		claimAttempts++
		if claimAttempts > 1 {
			return http.ErrBodyNotAllowed // any non-nil error stands in for "already used"
		}
		return nil
	}

	body := validTarGz(t)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := doUpload(t, body)
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()

	accepted := 0
	for _, c := range codes {
		if c == http.StatusAccepted {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("exactly one request should be accepted, got codes=%v", codes)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.pushed {
		t.Error("the winning request should have pushed the archive")
	}
}

// TestHandleCLIDeployUpload_BuildImageHarborMismatchRefusesNoBuildNoPush
// pins criterion 6(b): a Harbor retarget between mint and upload must
// refuse the upload, start no build, and push nothing.
func TestHandleCLIDeployUpload_BuildImageHarborMismatchRefusesNoBuildNoPush(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          buildImageToken("harbor-locked-1"),
		buildkitConfig: &agentstream.BuildKitConfig{HarborOsid: "harbor-retargeted-2"},
	}
	h.install()

	w := doUpload(t, validTarGz(t))
	if w.Code == http.StatusAccepted {
		t.Fatalf("expected a refusal, got 202")
	}
	// Terminal (review finding 1133): a Harbor mismatch fails the same way
	// on every retry against this configuration.
	h.assertTerminalRefusal(t)
}

// TestHandleCLIDeployUpload_BuildImageHarborConfigMissingRefuses pins
// criterion 6(b)'s "or when the configuration names no Harbor".
func TestHandleCLIDeployUpload_BuildImageHarborConfigMissingRefuses(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          buildImageToken("harbor-locked-1"),
		buildkitConfig: &agentstream.BuildKitConfig{HarborOsid: ""},
	}
	h.install()

	w := doUpload(t, validTarGz(t))
	if w.Code == http.StatusAccepted {
		t.Fatalf("expected a refusal, got 202")
	}
	// Terminal (review finding 1133): a missing Harbor identity fails the
	// same way on every retry against this configuration.
	h.assertTerminalRefusal(t)
}

// TestHandleCLIDeployUpload_BuildImageHarborEndpointMissingRefuses pins
// criterion 6(b2) (review finding 1268): a matching, non-empty Harbor
// identity is not enough. The configuration must also name a usable
// registry endpoint before a bound build-image upload may claim and
// build, or BuildKit can start a pod against an unusable image reference.
func TestHandleCLIDeployUpload_BuildImageHarborEndpointMissingRefuses(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          buildImageToken("harbor-locked-1"),
		buildkitConfig: &agentstream.BuildKitConfig{HarborOsid: "harbor-locked-1", HarborURL: ""},
	}
	h.install()

	w := doUpload(t, validTarGz(t))
	if w.Code == http.StatusAccepted {
		t.Fatalf("expected a refusal, got 202")
	}
	// Terminal (review finding 1133): an incomplete configuration fails
	// the same way on every retry against it.
	h.assertTerminalRefusal(t)
}

// TestHandleCLIDeployUpload_BuildImageHarborMatchProceeds: the configuration
// is unchanged, so the push goes to the persisted Harbor and the build runs.
func TestHandleCLIDeployUpload_BuildImageHarborMatchProceeds(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          buildImageToken("harbor-locked-1"),
		buildkitConfig: &agentstream.BuildKitConfig{HarborOsid: "harbor-locked-1", HarborURL: "harbor.example.test"},
	}
	h.install()

	w := doUpload(t, validTarGz(t))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", w.Code, w.Body.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claimed) != 1 {
		t.Errorf("claimed = %v, want exactly one claim", h.claimed)
	}
	if !h.buildImgRun {
		t.Error("expected the build-image build to start")
	}
	if h.pushed {
		t.Error("the build-image path does not push an app archive")
	}
}

// TestHandleCLIDeployUpload_BuildImageLegacyNoHarborOsidBehavesAsBefore
// pins criterion 6(d): a build_image request that predates harborOsid
// (empty persisted value) keeps today's behavior and never compares,
// regardless of what the configuration currently names.
func TestHandleCLIDeployUpload_BuildImageLegacyNoHarborOsidBehavesAsBefore(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          buildImageToken(""),
		buildkitConfig: &agentstream.BuildKitConfig{HarborOsid: "harbor-anything"},
	}
	h.install()

	w := doUpload(t, validTarGz(t))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", w.Code, w.Body.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.buildImgRun {
		t.Error("expected the legacy build-image request to proceed")
	}
}
