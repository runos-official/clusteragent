package instructions

import (
	"context"
	"fmt"
	"testing"

	"github.com/runos-official/clusteragent/commons"
)

// TestVcsFetchSource_SourceFetchRefusalNeverClones proves a refused
// source_fetch permit short-circuits before any git invocation: the
// request's RepoCloneURL is deliberately unusable (an empty scheme), which
// would fail git with a git-specific error if ever reached. Getting the
// stage-refusal message instead proves the check ran first.
func TestVcsFetchSource_SourceFetchRefusalNeverClones(t *testing.T) {
	prevPermit := RequestJobStagePermit
	t.Cleanup(func() { RequestJobStagePermit = prevPermit })
	var gotStage string
	RequestJobStagePermit = func(_ context.Context, authorityJobID, stage string) error {
		gotStage = stage
		return fmt.Errorf("denied")
	}

	const conductorJobID = "00000000-0000-4000-8000-000000000005"
	payload, err := commons.JsonB64Encode(VcsFetchSourceRequest{
		OSID:           "app-source-fetch-denied",
		SHA:            "0123456",
		RepoCloneURL:   "not-a-real-url",
		ConductorJobID: conductorJobID,
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	replyType, reply, err := VcsFetchSource(payload)
	if err != nil {
		t.Fatalf("VcsFetchSource: %v", err)
	}
	if replyType != "VCS_FETCH_SOURCE_RESPONSE" {
		t.Fatalf("replyType = %q", replyType)
	}
	var resp VcsFetchSourceResponse
	if err := commons.JsonB64Decode(reply, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Success {
		t.Fatal("expected a refusal for a denied source_fetch permit")
	}
	if resp.Message != "source fetch refused" {
		t.Errorf("message = %q, want the generic stage refusal (proves the check ran before git)", resp.Message)
	}
	if gotStage != StageSourceFetch {
		t.Errorf("stage = %q, want %q", gotStage, StageSourceFetch)
	}
}

// TestVcsFetchSource_LegacyNeverCallsStagePermit proves an explicit legacy
// VCS deploy (empty ConductorJobID) never calls the recheck transport, and
// proceeds straight to git (failing there instead, with a git-specific
// message, since RepoCloneURL is unusable here too).
func TestVcsFetchSource_LegacyNeverCallsStagePermit(t *testing.T) {
	prevPermit := RequestJobStagePermit
	t.Cleanup(func() { RequestJobStagePermit = prevPermit })
	called := false
	RequestJobStagePermit = func(context.Context, string, string) error {
		called = true
		return fmt.Errorf("must not be called")
	}

	payload, err := commons.JsonB64Encode(VcsFetchSourceRequest{
		OSID:         "app-source-fetch-legacy",
		SHA:          "0123456",
		RepoCloneURL: "not-a-real-url",
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	_, reply, err := VcsFetchSource(payload)
	if err != nil {
		t.Fatalf("VcsFetchSource: %v", err)
	}
	var resp VcsFetchSourceResponse
	if err := commons.JsonB64Decode(reply, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Success {
		t.Fatal("expected a failure (the clone itself fails for this unusable URL)")
	}
	if resp.Message == "source fetch refused" {
		t.Error("a legacy deploy must not see the stage-refusal message")
	}
	if called {
		t.Error("a legacy VCS deploy must never call the recheck transport")
	}
}
