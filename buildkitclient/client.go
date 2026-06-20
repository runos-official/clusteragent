// Package buildkitclient builds app and auxiliary container images by starting
// an ephemeral per-build buildkitd pod, driving it with buildctl, and pushing
// the result (with Harbor registry-cache import/export) to the in-cluster
// Harbor. It also stages per-build auth (SSH keys, named secrets) and sweeps
// orphaned build pods at startup.
package buildkitclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/runos-official/clusteragent/datastore"

	"k8s.io/client-go/kubernetes"
)

// defaultBuildTimeout bounds a single build's wall-clock time, covering the
// concurrency-slot wait, the per-build pod start, and buildctl itself. A hung
// pod start or stuck buildctl (the moby/buildkit#6131 client-hang class)
// otherwise leaves the build blocked indefinitely and the build_kit job row
// stuck at JobStatusBusy forever, never failing, never freeing for retry. The
// default is generous so large legitimate builds are unaffected; override
// with BUILDKIT_BUILD_TIMEOUT (any time.ParseDuration value, e.g. "90m").
const defaultBuildTimeout = 45 * time.Minute

// buildTimeout returns the effective per-build timeout, honouring
// BUILDKIT_BUILD_TIMEOUT when set to a positive duration.
func buildTimeout() time.Duration {
	if v := os.Getenv("BUILDKIT_BUILD_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		log.Printf("buildkitclient: ignoring invalid BUILDKIT_BUILD_TIMEOUT=%q, using default %s", v, defaultBuildTimeout)
	}
	return defaultBuildTimeout
}

// StripRegistryProtocol removes a leading http(s):// from a Harbor URL so it
// can be used as a registry host in an image reference (BuildKit / Docker
// image names don't accept a scheme). Shared so the build path and the
// post-build ref construction strip identically.
func StripRegistryProtocol(harborURL string) string {
	stripped := strings.TrimPrefix(harborURL, "https://")
	return strings.TrimPrefix(stripped, "http://")
}

// HarborProject is the single Harbor project all RunOS-managed images live
// under. The app-less build-image primitive (obj-47) is fixed to it by
// construction (no caller can redirect a push elsewhere), matching where app
// images already go.
const HarborProject = "runos-apps"

// TargetImageRefs builds the fully-qualified push refs for an app-less
// build-image: `<registry>/runos-apps/<repo>:<tag>` for every tag. The same
// built image is pushed to each. harborRegistry must already be
// scheme-stripped (see StripRegistryProtocol).
func TargetImageRefs(harborRegistry, repo string, tags []string) []string {
	refs := make([]string, 0, len(tags))
	for _, tag := range tags {
		refs = append(refs, fmt.Sprintf("%s/%s/%s:%s", harborRegistry, HarborProject, repo, tag))
	}
	return refs
}

// cacheRefForPushRef derives the Harbor registry-cache ref for a build from
// its primary push ref: same repository, fixed `buildcache` tag
// (`<registry>/runos-apps/<name>:buildcache`). Keeping cache and image in the
// same Harbor repository lets cache blobs dedupe against pushed image layers.
// The tag separator is the last ':' only when it follows the last '/' (a
// registry host:port colon does not).
func cacheRefForPushRef(pushRef string) string {
	repo := pushRef
	if i := strings.LastIndex(pushRef, ":"); i > strings.LastIndex(pushRef, "/") {
		repo = pushRef[:i]
	}
	return repo + ":buildcache"
}

// imageOutputArg builds the buildctl `--output` value for the image
// exporter pushing to one or more refs.
//
// BuildKit parses the `--output` value as CSV (key=value attributes joined
// by commas). A multi-ref build supplies the refs as a comma-separated
// `name=` list, but those commas collide with the CSV field separator: left
// unquoted (`type=image,name=a,b,push=true`) BuildKit reads `b` as a bogus
// attribute and rejects the build. Quoting the whole `name=<list>` field
// (`type=image,"name=a,b",push=true`) keeps the list inside one CSV field.
// The args are handed to buildctl via exec (no shell), so the literal
// double-quotes reach BuildKit's CSV parser as quote characters.
//
// A single ref keeps the unquoted form byte-for-byte so the app-deploy /
// run / apps-build / CLI-deploy single-image path is unchanged.
func imageOutputArg(pushRefs []string) string {
	if len(pushRefs) == 1 {
		return fmt.Sprintf("type=image,name=%s,push=true", pushRefs[0])
	}
	return fmt.Sprintf(`type=image,"name=%s",push=true`, strings.Join(pushRefs, ","))
}

// redactURLCredentials replaces username:password in URLs with ***:***
func redactURLCredentials(urlStr string) string {
	// Find the pattern protocol://username:password@host
	if idx := strings.Index(urlStr, "://"); idx != -1 {
		afterProtocol := urlStr[idx+3:]
		if atIdx := strings.Index(afterProtocol, "@"); atIdx != -1 {
			// Found credentials
			protocol := urlStr[:idx+3]
			hostAndPath := afterProtocol[atIdx:]
			return protocol + "***:***" + hostAndPath
		}
	}
	return urlStr
}

// BuildConfig holds configuration for a BuildKit build.
//
// LocalContextPath is the cloned/extracted source root on disk and is used
// as the default for both the BuildKit context and the Dockerfile dir when
// the override fields below are empty.
//
// ContextPath / DockerfileDir / DockerfileFilename let VCS deploys point
// BuildKit at a non-root build context (monorepo apps under
// `apps/billing/...`) and at non-default Dockerfile names/locations
// (e.g. `Dockerfile.prod` or `docker/Dockerfile`). All three are absolute
// paths inside the cloned tree; resolution + path-traversal checks happen
// in the cluster-agent VCS_FETCH_SOURCE handler before BuildKit sees them.
//
// CLI deploys leave the override fields empty, so behaviour matches the
// pre-monorepo single-app shape exactly: build with context = root and
// dockerfile = `<root>/Dockerfile`.
type BuildConfig struct {
	JobID              string
	OSID               string
	Commit             string
	CommitShort        string
	HarborURL          string
	HarborUsername     string
	HarborPassword     string
	LocalContextPath   string
	ContextPath        string
	DockerfileDir      string
	DockerfileFilename string
	// BuildArgs are the effective Docker build args ([{key,value,source}])
	// merged + validated by conductor. Each is forwarded to BuildKit as
	// `--opt build-arg:KEY=VALUE`. ARGs not declared in the Dockerfile are
	// ignored by BuildKit (no-op, safe), matching `docker build` semantics.
	// Empty for deploys with no build args.
	BuildArgs []datastore.BuildArg
	// Repo (credential-stripped clone URL) and Branch annotate the per-build
	// BuildKit pod for manual inspection. Set for VCS deploys; empty for CLI
	// deploys and app-less build-image builds.
	Repo   string
	Branch string
	// TargetImages, when non-empty, are the fully-qualified image refs
	// (`<registry>/<project>/<repo>:<tag>`) this build pushes to. A single
	// build pushes the SAME image bytes to every ref via BuildKit's
	// comma-separated image-exporter `name=` list. Used by the app-less
	// build-image primitive (obj-47) to push one build to one or more
	// arbitrary tags. When empty, the build falls back to the historical
	// single app-image ref derived from OSID + Commit, so app deploy / run
	// / apps-build / CLI-deploy behaviour is unchanged.
	TargetImages []string
}

// Build executes a Docker build using BuildKit via buildctl CLI against an
// ephemeral per-build buildkitd pod it creates (and deletes) itself.
func Build(ctx context.Context, clientset kubernetes.Interface, config BuildConfig) error {
	// Log build start. The job row stays at its created status (pending)
	// until a concurrency slot is acquired below, so consumers (console
	// builds UI, CLI/MCP) see queued builds as pending rather than busy.
	log.Printf("Starting BuildKit build: job_id=%s, osid=%s, commit=%s", config.JobID, config.OSID, config.CommitShort)
	datastore.InsertBuildKitLog(config.JobID, fmt.Sprintf("Starting build for %s:%s", config.OSID, config.CommitShort))

	// Bound the build so a hung pod start or stuck buildctl can't hang it
	// forever. On expiry exec.CommandContext kills buildctl, cmd.Run returns
	// an error, and the failure path below flips the job to JobStatusFailed
	// (retryable) instead of leaving it stuck at JobStatusBusy. Callers pass
	// an unbounded context.Background(), so this is the single place the
	// deadline is applied for every build path (VCS, CLI, app-less). The
	// deadline covers slot wait + pod start + the build itself.
	timeout := buildTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Cluster-level build settings (concurrency cap + per-pod resources),
	// read fresh per build so console/CLI edits apply to the next build.
	settings := ReadBuildSettings(ctx, clientset)

	// Bound concurrent build pods. Builds beyond the cap wait here; the job
	// row is already busy, so the wait is made visible in the build log.
	if active := activeBuildCount(); active >= settings.MaxConcurrentBuilds {
		datastore.InsertBuildKitLog(config.JobID, fmt.Sprintf("Waiting for a build slot (%d builds running, limit %d)", active, settings.MaxConcurrentBuilds))
	}
	if err := acquireBuildSlot(ctx, settings.MaxConcurrentBuilds); err != nil {
		datastore.UpdateBuildKitJobStatus(config.JobID, datastore.JobStatusFailed)
		datastore.InsertBuildKitLog(config.JobID, fmt.Sprintf("ERROR: %v", err))
		return err
	}
	defer releaseBuildSlot()

	// Slot held: the build is genuinely in progress now (pod start + buildctl).
	if err := datastore.UpdateBuildKitJobStatus(config.JobID, datastore.JobStatusBusy); err != nil {
		return fmt.Errorf("failed to update job status: %w", err)
	}

	// Resolve the push target(s).
	// Strip protocol from Harbor URL for image tag (BuildKit doesn't accept https:// in image names)
	harborRegistry := StripRegistryProtocol(config.HarborURL)
	// pushRefs holds every ref this build pushes to. The app-less
	// build-image primitive (obj-47) supplies explicit TargetImages (one or
	// more arbitrary tags); everything else falls back to the historical
	// single app-image ref so behaviour is unchanged.
	pushRefs := config.TargetImages
	if len(pushRefs) == 0 {
		pushRefs = []string{fmt.Sprintf("%s/runos-apps/%s:%s", harborRegistry, config.OSID, config.Commit)}
	}

	// Start the per-build buildkitd pod, labelled/annotated with the build's
	// identity (osid, commit, repo/branch, target images) for manual
	// inspection (kubectl get pods -n buildkit --show-labels / describe).
	meta := BuilderMeta{
		JobID:  config.JobID,
		OSID:   config.OSID,
		Commit: config.CommitShort,
		Repo:   config.Repo,
		Branch: config.Branch,
		Images: pushRefs,
	}
	datastore.InsertBuildKitLog(config.JobID, "Starting ephemeral BuildKit pod")
	buildkitAddr, stopBuilder, err := StartEphemeralBuilder(ctx, clientset, meta, settings)
	if err != nil {
		datastore.UpdateBuildKitJobStatus(config.JobID, datastore.JobStatusFailed)
		datastore.InsertBuildKitLog(config.JobID, fmt.Sprintf("ERROR: failed to start BuildKit pod: %v", err))
		return fmt.Errorf("failed to start build pod: %w", err)
	}
	defer stopBuilder()
	datastore.InsertBuildKitLog(config.JobID, fmt.Sprintf("BuildKit pod ready at %s", buildkitAddr))
	// BuildKit's image exporter accepts a comma-separated name list, so a
	// single build pushes the same image bytes to every ref in one shot. The
	// `--output` value is itself CSV, so a multi-ref name list must be quoted
	// (see imageOutputArg).
	outputArg := imageOutputArg(pushRefs)
	imageTag := strings.Join(pushRefs, ", ")
	log.Printf("Building image: %s", imageTag)
	datastore.InsertBuildKitLog(config.JobID, fmt.Sprintf("Building image: %s", imageTag))

	// Create temporary directory for Docker config
	tmpDir, err := os.MkdirTemp("", "buildkit-docker-config-*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(tmpDir) // Clean up after build

	// Create Docker config.json with Harbor credentials using external registry URL (no protocol)
	dockerConfig := map[string]any{
		"auths": map[string]any{
			harborRegistry: map[string]string{
				"username": config.HarborUsername,
				"password": config.HarborPassword,
				"auth":     base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", config.HarborUsername, config.HarborPassword))),
			},
		},
	}

	configJSON, err := json.Marshal(dockerConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal docker config: %w", err)
	}

	configPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(configPath, configJSON, 0600); err != nil {
		return fmt.Errorf("failed to write docker config: %w", err)
	}

	log.Printf("Created Docker config at %s for registry %s", configPath, harborRegistry)
	datastore.InsertBuildKitLog(config.JobID, fmt.Sprintf("Created Harbor credentials for %s", harborRegistry))

	if config.LocalContextPath == "" {
		return fmt.Errorf("LocalContextPath is required")
	}

	// Resolve context + Dockerfile paths from the optional overrides set by
	// VCS_FETCH_SOURCE; CLI deploys (and any caller that leaves them empty)
	// fall back to the LocalContextPath + Dockerfile defaults so behaviour
	// is identical to the pre-monorepo shape.
	contextPath := config.ContextPath
	if contextPath == "" {
		contextPath = config.LocalContextPath
	}
	dockerfileDir := config.DockerfileDir
	if dockerfileDir == "" {
		dockerfileDir = contextPath
	}
	dockerfileFilename := config.DockerfileFilename
	if dockerfileFilename == "" {
		dockerfileFilename = "Dockerfile"
	}

	args := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", fmt.Sprintf("context=%s", contextPath),
		"--local", fmt.Sprintf("dockerfile=%s", dockerfileDir),
	}

	args = append(args,
		"--opt", fmt.Sprintf("filename=%s", dockerfileFilename),
		"--output", outputArg,
	)

	// Build-time auth (SSH key / named secrets), staged from the app's
	// build-auth Secrets and forwarded over the BuildKit session. Missing
	// Secrets mean no auth; only a real read error fails the build.
	auth, authCleanup, err := readBuildAuth(ctx, clientset, config.OSID)
	defer authCleanup()
	if err != nil {
		datastore.UpdateBuildKitJobStatus(config.JobID, datastore.JobStatusFailed)
		datastore.InsertBuildKitLog(config.JobID, fmt.Sprintf("ERROR: failed to read build auth: %v", err))
		return fmt.Errorf("failed to read build auth: %w", err)
	}
	if summary := describeBuildAuth(auth); summary != "" {
		datastore.InsertBuildKitLog(config.JobID, summary)
	}
	args = append(args, buildAuthArgs(auth)...)

	// Harbor registry cache: the per-build daemon starts with an empty local
	// cache, so layer reuse comes from importing the previous build's cache
	// from Harbor and exporting this build's cache back. mode=max also caches
	// intermediate (multi-stage) layers; image-manifest+oci-mediatypes emit
	// the OCI-compatible cache manifest Harbor accepts (the default BuildKit
	// cache media type is rejected by Harbor); zstd keeps export/import fast.
	// A missing cache ref (first build) is a non-fatal BuildKit warning.
	cacheRef := cacheRefForPushRef(pushRefs[0])
	args = append(args,
		"--export-cache", fmt.Sprintf("type=registry,ref=%s,mode=max,image-manifest=true,oci-mediatypes=true,compression=zstd", cacheRef),
		"--import-cache", fmt.Sprintf("type=registry,ref=%s", cacheRef),
	)

	// Forward each effective build arg to BuildKit. BuildKit ignores ARGs
	// that the Dockerfile doesn't declare (no-op, safe). Values are plaintext
	// (no secret-sourced args in this objective); only key/value/source land
	// in the build record.
	for _, ba := range config.BuildArgs {
		args = append(args, "--opt", fmt.Sprintf("build-arg:%s=%s", ba.Key, ba.Value))
	}

	// Set environment variables for BuildKit host and Docker config
	// buildctl CLI will read DOCKER_CONFIG to find registry credentials
	cmd := exec.CommandContext(ctx, "buildctl", args...)
	cmd.Env = os.Environ() // Inherit parent environment
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("BUILDKIT_HOST=%s", buildkitAddr),
		fmt.Sprintf("DOCKER_CONFIG=%s", tmpDir), // Point to our temporary config
	)

	// Capture output
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Redact credentials from logs
	argsForLog := make([]string, len(args))
	copy(argsForLog, args)
	for i, arg := range argsForLog {
		if strings.Contains(arg, "@") && strings.Contains(arg, "://") {
			// Redact credentials in URLs (e.g., http://user:pass@host -> http://***:***@host)
			argsForLog[i] = redactURLCredentials(arg)
		}
	}

	log.Printf("Executing buildctl: %s", strings.Join(argsForLog, " "))
	datastore.InsertBuildKitLog(config.JobID, fmt.Sprintf("Executing: buildctl %s", strings.Join(argsForLog, " ")))

	// Run build
	err = cmd.Run()
	log.Printf("BuildKit command completed for job %s, error: %v", config.JobID, err)

	// Log all output (redact credentials)
	if stdout.Len() > 0 {
		for _, line := range strings.Split(stdout.String(), "\n") {
			if line != "" {
				redactedLine := redactURLCredentials(line)
				log.Printf("[Build %s] STDOUT: %s", config.JobID, redactedLine)
				datastore.InsertBuildKitLog(config.JobID, redactedLine)
			}
		}
	}
	if stderr.Len() > 0 {
		for _, line := range strings.Split(stderr.String(), "\n") {
			if line != "" {
				redactedLine := redactURLCredentials(line)
				log.Printf("[Build %s] STDERR: %s", config.JobID, redactedLine)
				datastore.InsertBuildKitLog(config.JobID, redactedLine)
			}
		}
	}

	if err != nil {
		// Capture the build pod's state BEFORE the deferred cleanup deletes
		// it: a daemon killed externally (OOM, SIGKILL) shows up here as the
		// container's terminated exitCode/reason, turning a bare buildctl
		// "EOF" into a diagnosable line instead of destroyed evidence.
		state := builderStateSummary(clientset, config.JobID)
		log.Printf("Build pod state at failure for job %s: %s", config.JobID, state)
		datastore.InsertBuildKitLog(config.JobID, fmt.Sprintf("Build pod state at failure: %s", state))

		// Distinguish a timeout from an ordinary build failure so the log
		// makes the cause clear; both flip the job to JobStatusFailed.
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("Build timed out for job %s after %s", config.JobID, timeout)
			datastore.InsertBuildKitLog(config.JobID, fmt.Sprintf("ERROR: build timed out after %s; marking failed", timeout))
		}
		log.Printf("Build failed for job %s: %v", config.JobID, err)
		log.Printf("Build stdout: %s", stdout.String())
		log.Printf("Build stderr: %s", stderr.String())
		datastore.UpdateBuildKitJobStatus(config.JobID, datastore.JobStatusFailed)
		datastore.InsertBuildKitLog(config.JobID, fmt.Sprintf("ERROR: Build failed: %v", err))
		return fmt.Errorf("build failed: %w", err)
	}

	// Update job status to success
	datastore.UpdateBuildKitJobStatus(config.JobID, datastore.JobStatusSuccess)
	datastore.InsertBuildKitLog(config.JobID, fmt.Sprintf("Build completed successfully. Image: %s", imageTag))

	log.Printf("Build succeeded: job_id=%s, image=%s", config.JobID, imageTag)
	return nil
}
