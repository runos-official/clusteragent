package instructions

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"

	"github.com/google/uuid"
	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/datastore"
	"github.com/runos-official/clusteragent/drain"
)

// VcsBuildRequest is the input for VCS_BUILD.
//
// JobID is the conductor-supplied identifier for the deploy attempt (today
// conductor sends the SHA, mirroring the original CLI-deploy convention).
// The cluster agent does NOT use it as the BuildKit row key directly: a
// second deploy of the same SHA would collide on the unique constraint.
// Instead the cluster agent appends a short uuid suffix and returns the
// resulting unique row id in VcsBuildResponse.JobID, so conductor can poll
// listBuildLogs against the actual row.
//
// SHA is what the resulting image is tagged with
// (`<harbor>/runos-apps/<osid>:<sha>`), so the Harbor presence check in the
// next deploy of the same SHA can short-circuit the build. The image tag
// is decoupled from the row id specifically to make retries idempotent.
//
// RepoCloneURL and Branch are only used for build-job-row metadata (so the
// console builds UI shows the right repo/branch alongside the build).
// Embedded credentials in the URL are stripped before storing.
type VcsBuildRequest struct {
	OSID         string               `json:"osid"`
	SHA          string               `json:"sha"`
	JobID        string               `json:"jobId"`
	RepoCloneURL string               `json:"repoCloneUrl,omitempty"`
	Branch       string               `json:"branch,omitempty"`
	BuildArgs    []datastore.BuildArg `json:"buildArgs,omitempty"` // Effective Docker build args ([{key,value,source}]) merged + validated by conductor. Omitted (back-compat) when the deploy has none.
	// BuildOnly requests the standalone `apps build` path: build + push the
	// SHA-keyed image to Harbor and stop, WITHOUT patching the live
	// deployment. Default (false / omitted) keeps the historical behaviour
	// where a VCS build also rolls the deployment to the new image, so
	// run/deploy build-on-demand is unchanged. Conductor's app.build
	// orchestration sets this true; deploy leaves it false.
	BuildOnly bool `json:"buildOnly,omitempty"`
	// ConductorJobID (story 339) is Conductor's durable job-authority id
	// (handbook 949 C3's "job: jobId" stage authority), a SIBLING of JobID
	// (which keeps its existing, unrelated meaning above) — never a
	// repurposing of it. Must equal the value VCS_FETCH_SOURCE bound to
	// this osid+sha's cache entry; empty means an explicit legacy VCS
	// deploy. Omitted by a caller that predates this field.
	ConductorJobID string `json:"conductorJobId,omitempty"`
}

// authorizationClassification reports this request's classification purely
// from ConductorJobID's presence, matching VCS_FETCH_SOURCE's own rule
// (vcs_fetch_source.go): empty id means legacy, non-empty means scoped.
func (r VcsBuildRequest) authorizationClassification() string {
	if r.ConductorJobID == "" {
		return ""
	}
	return datastore.AuthorizationScoped
}

// VcsBuildResponse acknowledges that the cluster agent has accepted the
// build request. Actual progress lands in the build log table; conductor
// polls listBuildLogs to stream them on to the user.
type VcsBuildResponse struct {
	Success bool   `json:"success"`
	JobID   string `json:"jobId,omitempty"`
	Message string `json:"message,omitempty"`
}

// VcsBuildExecutorInput is what the registered executor receives.
//
// The executor (`webhook.RunVcsBuild`) owns the K8s/BuildKit setup and
// runs the build asynchronously. We hand it the cached workdir path, the
// resolved BuildKit paths (context dir, Dockerfile dir + filename), and
// the same identifiers the build job row carries, plus a cleanup hook
// for the workdir cache so the executor can decide when to release.
//
// JobID is the unique build_kit row id (sha + uuid suffix); SHA is what
// the resulting image is tagged with. Keeping them distinct lets retries
// of the same SHA produce fresh build rows without violating the unique
// constraint, while the image tag stays SHA-keyed so the Harbor presence
// check on the next deploy can short-circuit the build.
//
// ContextPath / DockerfileDir / DockerfileFilename are populated by
// VCS_FETCH_SOURCE from the committed runos.yaml and may be empty for
// legacy entries — the executor is responsible for falling back to
// (Workdir, Workdir, "Dockerfile") when they are.
type VcsBuildExecutorInput struct {
	OSID               string
	SHA                string
	JobID              string
	Workdir            string
	ContextPath        string
	DockerfileDir      string
	DockerfileFilename string
	// BuildArgs are the effective Docker build args ([{key,value,source}])
	// forwarded from the VcsBuildRequest; the executor passes them through to
	// ProcessBuildAndDeploy, which persists them and hands them to BuildKit.
	BuildArgs []datastore.BuildArg
	// BuildOnly forwards VcsBuildRequest.BuildOnly: when true the executor
	// stops after a successful Harbor push and skips the deployment patch.
	BuildOnly bool
	// Repo (credential-stripped clone URL) and Branch annotate the per-build
	// BuildKit pod for manual inspection; empty for non-VCS builds.
	Repo   string
	Branch string
	// AuthorizationClassification and AuthorityJobID (story 339) carry the
	// job-authority attribution this build's row was created with, so the
	// executor can request fresh build_start/deployment_patch permits for
	// the SAME durable job id used at source_fetch/queue_admission. Empty
	// classification means legacy (no stage recheck).
	AuthorizationClassification string
	AuthorityJobID              string
	// Cleanup is called by the executor when the build finishes (success
	// or failure). Removes the workdir from disk and the cache entry.
	Cleanup func()
}

// VcsBuildExecutor is set at startup by main.go (which imports both this
// package and the webhook package). Keeping the executor function injected
// rather than imported breaks the cycle between agentstream/instructions
// and webhook (which depends on agentstream).
var VcsBuildExecutor func(VcsBuildExecutorInput)

// beginWork registers one build or one-shot job with the drain tracker, so a
// restart of the agent waits for it. Tests replace it to model a draining agent.
var beginWork = drain.Begin

// createBuildKitJob and createScopedBuildKitJob are injected (story 339) so
// tests can exercise VcsBuild's admission/propagation ordering without a
// live datastore connection, matching this file's sibling
// create_cli_deploy_token.go's createUploadToken/createScopedUploadToken
// convention.
var (
	createBuildKitJob       = datastore.CreateBuildKitJob
	createScopedBuildKitJob = datastore.CreateScopedBuildKitJob
)

// VcsBuild handles the VCS_BUILD instruction.
//
// Looks up the workdir populated by an earlier VCS_FETCH_SOURCE call,
// creates the BuildKit job row with proper VCS metadata (so the row is
// distinguishable from a CLI deploy in the console UI), and hands off
// to the registered executor for the actual build. Returns immediately
// so the gRPC call doesn't block for minutes.
func VcsBuild(jsonB64 string) (string, string, error) {
	const replyType = "VCS_BUILD_RESPONSE"

	var req VcsBuildRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		log.Printf("VCS_BUILD: decode error: %v", err)
		return "", "", err
	}

	if req.OSID == "" || req.SHA == "" || req.JobID == "" {
		return replyVcsBuild(replyType, VcsBuildResponse{
			Success: false,
			Message: "osid, sha, and jobId are required",
		})
	}

	// Refuse before anything is written: a build row for a refused build would
	// never get a build. end is called here on every early return, and by the
	// build goroutine once the build is over.
	end, err := beginWork()
	if err != nil {
		return replyVcsBuild(replyType, VcsBuildResponse{Success: false, Message: err.Error()})
	}
	started := false
	defer func() {
		if !started {
			end()
		}
	}()

	if VcsBuildExecutor == nil {
		return replyVcsBuild(replyType, VcsBuildResponse{
			Success: false,
			Message: "VCS build executor not registered (cluster agent misconfigured)",
		})
	}

	paths, ok := vcsWorkdirCacheGet(req.OSID, req.SHA)
	if !ok {
		return replyVcsBuild(replyType, VcsBuildResponse{
			Success: false,
			Message: "no fetched workdir for osid+sha; caller must run VCS_FETCH_SOURCE first (or the cache TTL expired)",
		})
	}

	// Story 339: the cache entry's own job-authority attribution (bound by
	// VCS_FETCH_SOURCE) must match this request's exactly. A cache entry
	// from another binding, app, job, or source identity cannot satisfy
	// this request (plan "VCS propagation") - never fall back to treating
	// either side's mismatch as legacy.
	if paths.AuthorizationClassification != req.authorizationClassification() {
		return replyVcsBuild(replyType, VcsBuildResponse{
			Success: false,
			Message: "fetched source does not match this build's authorization classification",
		})
	}
	if paths.ConductorJobID != req.ConductorJobID {
		return replyVcsBuild(replyType, VcsBuildResponse{
			Success: false,
			Message: "fetched source does not match this build's job authority",
		})
	}

	// Request queue admission for the bound job BEFORE the build row is
	// created (plan "VCS propagation": "Request queue admission before the
	// build row is created."). A refusal creates no job row and starts no
	// goroutine; the already-authorized source fetch remains the only
	// completed effect for this stage.
	if req.ConductorJobID != "" {
		if err := requireJobStagePermit(context.Background(), req.ConductorJobID, StageQueueAdmission); err != nil {
			log.Printf("VCS_BUILD: queue_admission stage recheck refused for osid=%s sha=%s: %v", req.OSID, req.SHA, err)
			return replyVcsBuild(replyType, VcsBuildResponse{
				Success: false,
				Message: "queue admission refused",
			})
		}
	}

	// Append a short uuid suffix so retries of the same SHA produce a fresh
	// build_kit row (the table keys by job_id, sha-only would collide).
	// We keep the SHA prefix because the console builds UI uses the row id
	// for display and the SHA prefix stays human-recognisable. A retry
	// therefore always gets a fresh row AND, since every call above re-runs
	// for the new request, a fresh queue_admission decision: a permit for
	// an earlier attempt's job identity never authorizes this one.
	buildJobID := fmt.Sprintf("%s-%s", req.JobID, uuid.New().String()[:8])

	cleanRepoURL := stripURLCredentials(req.RepoCloneURL)
	var createErr error
	if req.ConductorJobID != "" {
		createErr = createScopedBuildKitJob(buildJobID, req.ConductorJobID, req.OSID, cleanRepoURL, req.Branch, req.SHA)
	} else {
		createErr = createBuildKitJob(buildJobID, req.OSID, cleanRepoURL, req.Branch, req.SHA)
	}
	if createErr != nil {
		return replyVcsBuild(replyType, VcsBuildResponse{
			Success: false,
			Message: fmt.Sprintf("create build job row: %v", createErr),
		})
	}
	log.Printf("VCS_BUILD: queued build job_id=%s osid=%s sha=%s context=%s dockerfile=%s/%s",
		buildJobID, req.OSID, req.SHA, paths.ContextPath, paths.DockerfileDir, paths.DockerfileFilename)

	osid, sha := req.OSID, req.SHA
	workdir := paths.Workdir
	started = true
	go func() {
		defer end()
		VcsBuildExecutor(VcsBuildExecutorInput{
			OSID:                        osid,
			SHA:                         sha,
			JobID:                       buildJobID,
			Workdir:                     workdir,
			ContextPath:                 paths.ContextPath,
			DockerfileDir:               paths.DockerfileDir,
			DockerfileFilename:          paths.DockerfileFilename,
			BuildArgs:                   req.BuildArgs,
			BuildOnly:                   req.BuildOnly,
			Repo:                        cleanRepoURL,
			Branch:                      req.Branch,
			AuthorizationClassification: req.authorizationClassification(),
			AuthorityJobID:              req.ConductorJobID,
			// Cleanup deletes the specific workdir this build used. We can't
			// chase the cache (vcsWorkdirCacheDelete) because workdirs are now
			// unique per VCS_FETCH_SOURCE call: a concurrent fetch for the
			// same {osid, sha} may have overwritten the cache entry between
			// fetch return and build cleanup, and we don't want to wipe its
			// fresh tree. We only clear the cache entry if it still points
			// at our workdir.
			Cleanup: func() {
				if err := os.RemoveAll(workdir); err != nil {
					log.Printf("VCS_BUILD: cleanup of workdir %s: %v", workdir, err)
				}
				vcsWorkdirCacheDeleteIfPath(osid, sha, workdir)
			},
		})
	}()

	// Return the unique row id so conductor polls listBuildLogs against the
	// actual row, not against the SHA-only id it sent.
	return replyVcsBuild(replyType, VcsBuildResponse{
		Success: true,
		JobID:   buildJobID,
	})
}

// stripURLCredentials removes user-info from an HTTPS URL so the URL stored
// on a build job row never contains tokens or passwords. Returns the input
// unchanged if it doesn't parse — the caller already validated the URL was
// usable for the prior git clone, so a parse failure here is a defensive
// fallback rather than a normal path.
func stripURLCredentials(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parsed.User = nil
	return parsed.String()
}

func replyVcsBuild(replyType string, payload VcsBuildResponse) (string, string, error) {
	encoded, err := commons.JsonB64Encode(payload)
	if err != nil {
		return "", "", err
	}
	return replyType, encoded, nil
}
