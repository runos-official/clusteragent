package instructions

import (
	"context"
	"fmt"
	"regexp"
)

// Published job-kind stage names (handbook article 949, C3's stage-authority
// table). archive_persist is omitted here: it uses an upload authority, not
// a job authority, and is wired entirely inside the webhook package (story
// 338).
const (
	StageSourceFetch     = "source_fetch"
	StageQueueAdmission  = "queue_admission"
	StageBuildStart      = "build_start"
	StageDeploymentPatch = "deployment_patch"
)

// RequestJobStagePermit is set at startup (main.go) to a function that asks
// the control plane for one fresh, correlated job-authority permit over the
// authenticated stream (agentstream.RequestStageRecheck +
// agentstream.JobAuthority) and returns nil only for an exact current
// "permit" outcome.
//
// This package cannot import agentstream directly: agentstream itself
// imports this package (agent.go, progress.go dispatch instructions), so
// agentstream.RecheckStage/RecheckAuthority/StageResponse are deliberately
// not referenced here — stage is passed as one of the plain string
// constants above, and main.go's wiring converts both ways. main.go is the
// one file that may import both packages.
//
// A nil RequestJobStagePermit (unwired, e.g. a test that never calls main)
// is a programming error, not a stage refusal: callers only reach this var
// on a SCOPED job (a non-empty authorityJobID), and main.go always wires it
// before agentstream.Start(). Tests inject a fake directly.
var RequestJobStagePermit func(ctx context.Context, authorityJobID string, stage string) error

// jobAuthorityIDPattern requires a canonical lowercase UUID, matching
// agentstream's own authority.validate().
var jobAuthorityIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// requireJobStagePermit validates authorityJobID's shape and asks for one
// fresh, correlated job-authority permit for stage. Callers in this package
// invoke it ONLY for a scoped job (a non-empty authority job id); a legacy
// job (empty id) skips the recheck entirely, matching the upload path's
// explicit no-fallback discipline (datastore/upload_authority.go).
func requireJobStagePermit(ctx context.Context, authorityJobID, stage string) error {
	if !jobAuthorityIDPattern.MatchString(authorityJobID) {
		return fmt.Errorf("invalid authority job id")
	}
	if RequestJobStagePermit == nil {
		return fmt.Errorf("job stage recheck client not wired")
	}
	return RequestJobStagePermit(ctx, authorityJobID, stage)
}
