package webhook

import (
	"context"
	"testing"

	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/datastore"
)

// TestProcessBuildAndDeploy_BuildStartRefusalNeverTouchesK8s proves a
// refused build_start permit returns before any BuildKit/Kubernetes call:
// the k8sClient argument is nil, which would panic on first use, so
// reaching a clean return proves the refusal short-circuited beforehand
// (plan "Already-started effect boundary": "If a later stage refuses, mark
// the existing job failed with a generic non-secret reason. Do not start
// the refused stage.").
func TestProcessBuildAndDeploy_BuildStartRefusalNeverTouchesK8s(t *testing.T) {
	prev := requestStageRecheck
	t.Cleanup(func() { requestStageRecheck = prev })
	var gotAuthority agentstream.RecheckAuthority
	var gotStage agentstream.RecheckStage
	requestStageRecheck = func(_ context.Context, authority agentstream.RecheckAuthority, stage agentstream.RecheckStage, _ string) (*agentstream.StageResponse, error) {
		gotAuthority = authority
		gotStage = stage
		return &agentstream.StageResponse{Outcome: agentstream.OutcomeDeny, Code: "build_start_denied"}, nil
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ProcessBuildAndDeploy touched the nil k8s client: %v", r)
		}
	}()

	ProcessBuildAndDeploy(context.Background(), nil, BuildAndDeployInput{
		Workdir:                     t.TempDir(),
		OSID:                        "app-1",
		JobID:                       "job-1",
		AuthorizationClassification: datastore.AuthorizationScoped,
		AuthorityJobID:              testAuthorityJobID,
	}, &agentstream.BuildKitConfig{})

	if gotAuthority != agentstream.JobAuthority(testAuthorityJobID) {
		t.Errorf("authority = %+v, want JobAuthority(%q)", gotAuthority, testAuthorityJobID)
	}
	if gotStage != agentstream.StageBuildStart {
		t.Errorf("stage = %q, want build_start", gotStage)
	}
}

// TestProcessBuildAndDeploy_LegacyNeverCallsStageRecheck proves a legacy
// job (empty AuthorityJobID) never calls the recheck transport at all. It
// still reaches buildkitclient.Build with the nil k8s client, which panics
// - the panic itself is the proof the legacy path does NOT short-circuit
// on a stage check (unlike the scoped case above); recovering it here lets
// the test assert "the recheck was never called" without needing a live
// Kubernetes client to get further.
func TestProcessBuildAndDeploy_LegacyNeverCallsStageRecheck(t *testing.T) {
	prev := requestStageRecheck
	t.Cleanup(func() { requestStageRecheck = prev })
	called := false
	requestStageRecheck = func(context.Context, agentstream.RecheckAuthority, agentstream.RecheckStage, string) (*agentstream.StageResponse, error) {
		called = true
		return &agentstream.StageResponse{Outcome: agentstream.OutcomeDeny, Code: "must not be called"}, nil
	}

	defer func() {
		recover() // expected: the nil k8s client panics once Build() is reached
		if called {
			t.Error("a legacy job (empty AuthorityJobID) must never call the recheck transport")
		}
	}()

	ProcessBuildAndDeploy(context.Background(), nil, BuildAndDeployInput{
		Workdir: t.TempDir(),
		OSID:    "app-1",
		JobID:   "job-2",
	}, &agentstream.BuildKitConfig{})
}
