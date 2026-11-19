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

	mu          sync.Mutex
	claimed     []string
	claimErr    error
	pushed      bool
	cliStarted  bool
	buildImgRun bool
}

func (h *uploadHarness) install() {
	h.t.Helper()

	prevLoad, prevClaim, prevConfig, prevRecheck, prevPush := loadUploadableToken, claimUploadToken, readBuildKitConfig, requestStageRecheck, pushArchive
	prevCLI, prevBuildImg := startCLIDeploymentAsync, startBuildImageDeploymentAsync
	prevBegin := beginWork
	h.t.Cleanup(func() {
		loadUploadableToken, claimUploadToken, readBuildKitConfig, requestStageRecheck, pushArchive = prevLoad, prevClaim, prevConfig, prevRecheck, prevPush
		startCLIDeploymentAsync, startBuildImageDeploymentAsync = prevCLI, prevBuildImg
		beginWork = prevBegin
	})

	beginWork = func() (func(), error) { return func() {}, nil }

	loadUploadableToken = func(string) (*datastore.UploadToken, error) {
		if h.loadErr != nil {
			return nil, h.loadErr
		}
		return h.token, nil
	}
	claimUploadToken = func(token string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.claimErr != nil {
			return h.claimErr
		}
		h.claimed = append(h.claimed, token)
		return nil
	}
	readBuildKitConfig = func(context.Context) (*agentstream.BuildKitConfig, error) {
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

func (h *uploadHarness) assertNoEffects(t *testing.T) {
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
		recheckResp:    &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, Code: "ok"},
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
	h.assertNoEffects(t)
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
	h.assertNoEffects(t)
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

// TestHandleCLIDeployUpload_ConcurrentClaimRaceOnlyOnePersists pins AC5:
// "Concurrent consumption cannot bypass single-use binding." Both requests
// pass every check and receive a permit; only the first claim may succeed,
// and only that one request may push or build.
func TestHandleCLIDeployUpload_ConcurrentClaimRaceOnlyOnePersists(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          scopedUploadToken(),
		buildkitConfig: &agentstream.BuildKitConfig{},
		recheckResp:    &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, Code: "ok"},
	}
	h.install()

	var claimAttempts int
	var mu sync.Mutex
	claimUploadToken = func(string) error {
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
	h.assertNoEffects(t)
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
	h.assertNoEffects(t)
}

// TestHandleCLIDeployUpload_BuildImageHarborMatchProceeds: the configuration
// is unchanged, so the push goes to the persisted Harbor and the build runs.
func TestHandleCLIDeployUpload_BuildImageHarborMatchProceeds(t *testing.T) {
	h := &uploadHarness{
		t:              t,
		token:          buildImageToken("harbor-locked-1"),
		buildkitConfig: &agentstream.BuildKitConfig{HarborOsid: "harbor-locked-1"},
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
