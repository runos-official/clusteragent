package webhook

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/runos-official/clusteragent/agentstream"
)

const testAuthorityJobID = "00000000-0000-4000-8000-000000000099"

// TestRequireJobStagePermit_Permit confirms the only outcome that continues:
// exactly OutcomePermit, with a job authority built from authorityJobID (not
// an upload authority) and a fresh, non-empty retryID per call.
func TestRequireJobStagePermit_Permit(t *testing.T) {
	prev := requestStageRecheck
	var gotAuthority agentstream.RecheckAuthority
	var gotStage agentstream.RecheckStage
	var gotRetryID string
	requestStageRecheck = func(_ context.Context, authority agentstream.RecheckAuthority, stage agentstream.RecheckStage, retryID string) (*agentstream.StageResponse, error) {
		gotAuthority = authority
		gotStage = stage
		gotRetryID = retryID
		return &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, Code: "ok"}, nil
	}
	t.Cleanup(func() { requestStageRecheck = prev })

	refusal := requireJobStagePermit(context.Background(), testAuthorityJobID, agentstream.StageQueueAdmission)
	if refusal != nil {
		t.Fatalf("expected permit (nil refusal), got status=%d reason=%q", refusal.status, refusal.reason)
	}
	if gotAuthority != agentstream.JobAuthority(testAuthorityJobID) {
		t.Errorf("authority = %+v, want JobAuthority(%q)", gotAuthority, testAuthorityJobID)
	}
	if gotStage != agentstream.StageQueueAdmission {
		t.Errorf("stage = %q, want queue_admission", gotStage)
	}
	if gotRetryID == "" {
		t.Error("retryID must not be empty")
	}
}

// TestRequireJobStagePermit_RefusesEveryNonPermitOutcome proves deny, needs,
// unavailable, and a transport error all refuse (never a silent pass).
func TestRequireJobStagePermit_RefusesEveryNonPermitOutcome(t *testing.T) {
	prev := requestStageRecheck
	t.Cleanup(func() { requestStageRecheck = prev })

	cases := []struct {
		name       string
		resp       *agentstream.StageResponse
		err        error
		wantStatus int
	}{
		{name: "deny", resp: &agentstream.StageResponse{Outcome: agentstream.OutcomeDeny, Code: "denied"}, wantStatus: http.StatusForbidden},
		{name: "needs", resp: &agentstream.StageResponse{Outcome: agentstream.OutcomeNeeds, Code: "needs_condition"}, wantStatus: http.StatusForbidden},
		{name: "unavailable", resp: &agentstream.StageResponse{Outcome: agentstream.OutcomeUnavailable, Code: "outage"}, wantStatus: http.StatusServiceUnavailable},
		{name: "transport error", err: errors.New("boom"), wantStatus: http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requestStageRecheck = func(context.Context, agentstream.RecheckAuthority, agentstream.RecheckStage, string) (*agentstream.StageResponse, error) {
				return tc.resp, tc.err
			}
			refusal := requireJobStagePermit(context.Background(), testAuthorityJobID, agentstream.StageBuildStart)
			if refusal == nil {
				t.Fatalf("%s: expected a refusal", tc.name)
			}
			if refusal.status != tc.wantStatus {
				t.Errorf("%s: status = %d, want %d", tc.name, refusal.status, tc.wantStatus)
			}
		})
	}
}
