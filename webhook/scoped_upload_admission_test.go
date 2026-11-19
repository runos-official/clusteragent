package webhook

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/datastore"
)

func testBinding() *datastore.UploadAuthorityBinding {
	return &datastore.UploadAuthorityBinding{
		BindingID: "00000000-0000-4000-8000-000000000001",
		UploadID:  "00000000-0000-4000-8000-000000000002",
	}
}

// TestAdmitScopedUpload_MissingBindingFailsClosed pins the plan's "Legacy
// behavior" rule: a scoped classification with no stored binding is invalid,
// not legacy. It must refuse, not fall back to legacy authorization.
func TestAdmitScopedUpload_MissingBindingFailsClosed(t *testing.T) {
	prev := requestStageRecheck
	called := false
	requestStageRecheck = func(context.Context, agentstream.RecheckAuthority, agentstream.RecheckStage, string) (*agentstream.StageResponse, error) {
		called = true
		return nil, nil
	}
	t.Cleanup(func() { requestStageRecheck = prev })

	refusal := admitScopedUpload(context.Background(), nil)
	if refusal == nil {
		t.Fatal("expected a refusal for a missing binding")
	}
	if refusal.status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", refusal.status)
	}
	if called {
		t.Error("must not call the recheck transport with no binding")
	}
}

// TestAdmitScopedUpload_Permit confirms the only outcome that continues:
// exactly OutcomePermit. The caller must still see nil (no refusal) and the
// authority sent must come from the binding, not from caller input.
func TestAdmitScopedUpload_Permit(t *testing.T) {
	prev := requestStageRecheck
	var gotAuthority agentstream.RecheckAuthority
	var gotStage agentstream.RecheckStage
	requestStageRecheck = func(_ context.Context, authority agentstream.RecheckAuthority, stage agentstream.RecheckStage, retryID string) (*agentstream.StageResponse, error) {
		gotAuthority = authority
		gotStage = stage
		if retryID == "" {
			t.Error("retryID must not be empty")
		}
		return &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, Code: "ok"}, nil
	}
	t.Cleanup(func() { requestStageRecheck = prev })

	binding := testBinding()
	if refusal := admitScopedUpload(context.Background(), binding); refusal != nil {
		t.Fatalf("expected permit (nil refusal), got status=%d reason=%q", refusal.status, refusal.reason)
	}
	want := agentstream.UploadAuthority(binding.BindingID, binding.UploadID)
	if gotAuthority != want {
		t.Errorf("authority = %+v, want %+v", gotAuthority, want)
	}
	if gotStage != agentstream.StageArchivePersist {
		t.Errorf("stage = %q, want archive_persist", gotStage)
	}
}

// TestAdmitScopedUpload_RefusesEveryNonPermitOutcome pins every refusal
// path the plan names: deny, needs, unavailable, and a transport/validation
// error (stale reply, wrong binding, disconnect, timeout - all surface as an
// error from the injected transport). Every one must refuse: no permit, no
// panic, and a generic message (never the detailed reason).
func TestAdmitScopedUpload_RefusesEveryNonPermitOutcome(t *testing.T) {
	prev := requestStageRecheck
	t.Cleanup(func() { requestStageRecheck = prev })

	cases := []struct {
		name       string
		resp       *agentstream.StageResponse
		err        error
		wantStatus int
	}{
		{name: "deny", resp: &agentstream.StageResponse{Outcome: agentstream.OutcomeDeny, Code: "revoked"}, wantStatus: http.StatusForbidden},
		{name: "needs", resp: &agentstream.StageResponse{Outcome: agentstream.OutcomeNeeds, Code: "step_up"}, wantStatus: http.StatusForbidden},
		{name: "unavailable outcome", resp: &agentstream.StageResponse{Outcome: agentstream.OutcomeUnavailable, Code: "overloaded"}, wantStatus: http.StatusServiceUnavailable},
		{name: "transport error (disconnect/timeout/stale/wrong binding)", err: errors.New("deployment stage recheck: boom"), wantStatus: http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requestStageRecheck = func(context.Context, agentstream.RecheckAuthority, agentstream.RecheckStage, string) (*agentstream.StageResponse, error) {
				return c.resp, c.err
			}
			refusal := admitScopedUpload(context.Background(), testBinding())
			if refusal == nil {
				t.Fatal("expected a refusal")
			}
			if refusal.status != c.wantStatus {
				t.Errorf("status = %d, want %d", refusal.status, c.wantStatus)
			}
			if refusal.message == "" || refusal.message == refusal.reason {
				t.Errorf("message must be a generic string distinct from the detailed reason, got message=%q reason=%q", refusal.message, refusal.reason)
			}
		})
	}
}

// TestVerifyBuildImageHarborIdentity pins story 338 criterion 6(b)/(d): the
// comparison only runs when the token carries a locked identity, and only an
// exact match of a non-empty configured Harbor continues.
func TestVerifyBuildImageHarborIdentity(t *testing.T) {
	cases := []struct {
		name         string
		tokenOsid    string
		configOsid   string
		wantRefused  bool
		wantStatusIf int
	}{
		{name: "legacy token with no locked identity always proceeds", tokenOsid: "", configOsid: "anything-or-empty"},
		{name: "legacy token with no locked identity proceeds even with empty config", tokenOsid: "", configOsid: ""},
		{name: "match proceeds", tokenOsid: "harbor-1", configOsid: "harbor-1"},
		{name: "mismatch refuses", tokenOsid: "harbor-1", configOsid: "harbor-2", wantRefused: true, wantStatusIf: http.StatusForbidden},
		{name: "config names no Harbor refuses", tokenOsid: "harbor-1", configOsid: "", wantRefused: true, wantStatusIf: http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			refusal := verifyBuildImageHarborIdentity(c.tokenOsid, c.configOsid)
			if c.wantRefused {
				if refusal == nil {
					t.Fatal("expected a refusal")
				}
				if refusal.status != c.wantStatusIf {
					t.Errorf("status = %d, want %d", refusal.status, c.wantStatusIf)
				}
			} else if refusal != nil {
				t.Fatalf("expected no refusal, got status=%d reason=%q", refusal.status, refusal.reason)
			}
		})
	}
}
