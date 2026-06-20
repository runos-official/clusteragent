package webhook

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/buildkitclient"
	"github.com/runos-official/clusteragent/datastore"
	"github.com/runos-official/clusteragent/harborclient"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	MaxUploadSize = 100 * 1024 * 1024 // 100MB max tarball size
	MaxFileSize   = 500 * 1024 * 1024 // 500MB max per extracted file
)

// Decompression-bomb caps. These are package-level vars (not consts) only so
// tests can lower them to exercise the guards without materialising gigabytes;
// production never mutates them.
var (
	// MaxTotalExtractedBytes caps the TOTAL decompressed bytes written across
	// all entries. The per-file MaxFileSize alone doesn't bound the aggregate:
	// a gzip bomb of many sub-MaxFileSize entries (or one entry repeated) could
	// still exhaust disk. 2 GiB is generously above any real build context.
	MaxTotalExtractedBytes int64 = 2 * 1024 * 1024 * 1024 // 2 GiB total decompressed
	// MaxEntryCount caps the number of tar entries processed, defending against
	// a many-tiny-files exhaustion (inode/CPU) attack where each entry is small
	// enough to pass MaxFileSize but the count is unbounded.
	MaxEntryCount = 50000
)

// HandleCLIDeployUpload processes CLI deployment tarball uploads with presigned tokens
func HandleCLIDeployUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract token from URL path: /cli-deploy/{token}
	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/cli-deploy/"), "/")
	if len(pathParts) == 0 || pathParts[0] == "" {
		http.Error(w, "Token required", http.StatusBadRequest)
		return
	}
	token := pathParts[0]

	// Validate token. The endpoint serves both app-deploy uploads and
	// app-less build-image uploads (obj-47); GetUploadableToken matches
	// either purpose and we branch on it below.
	uploadToken, err := datastore.GetUploadableToken(token)
	if err != nil {
		log.Printf("Token lookup failed: %v", err)
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	// Check expiry
	if time.Now().After(uploadToken.ExpiresAt) {
		log.Printf("Token expired: %s", token[:8])
		http.Error(w, "Token expired", http.StatusUnauthorized)
		return
	}

	// Check if already used
	if uploadToken.Used {
		log.Printf("Token already used: %s", token[:8])
		http.Error(w, "Token already used", http.StatusUnauthorized)
		return
	}

	// Mark token as used IMMEDIATELY (before processing) to prevent race conditions
	if err := datastore.MarkUploadTokenUsed(token); err != nil {
		log.Printf("Failed to mark token as used: %v", err)
		http.Error(w, "Token validation failed", http.StatusInternalServerError)
		return
	}

	// Limit request body size
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadSize)

	// Read tarball from request body
	tarballData, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Failed to read upload body: %v", err)
		http.Error(w, "Failed to read upload", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// DeployConfig is "{osid}:{uploadId}". For app deploys osid is the app id;
	// for build_image tokens (obj-47) osid is a synthetic app-less identity
	// (the target repo name) so the console builds UI stays recognisable.
	// Older app-deploy conductors stored just "{osid}"; in that case we mint
	// the uploadId here as a fallback so the legacy flow keeps working.
	osid, uploadID, hasUploadID := strings.Cut(uploadToken.DeployConfig, ":")
	if !hasUploadID || uploadID == "" {
		uploadID = uuid.New().String()
		log.Printf("Upload token had no pre-assigned uploadId; minted %s (legacy conductor path)", uploadID)
	}

	// Load BuildKit config (Harbor credentials) used by both flows.
	ctx := r.Context()
	k8sClient, err := agentstream.NewK8sClient()
	if err != nil {
		log.Printf("Failed to create K8s client: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	buildkitConfig, err := k8sClient.GetBuildKitConfig(ctx)
	if err != nil {
		log.Printf("Failed to get BuildKit config: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// App-less build-image flow (obj-47): the tarball is a generic build
	// context to be built and pushed to explicit Harbor target ref(s). No
	// app, no deploy, no source-archive persistence (there is no pull-token
	// use case for an auxiliary image).
	if uploadToken.Purpose == datastore.PurposeBuildImage {
		if uploadToken.BuildTarget == nil || uploadToken.BuildTarget.Repo == "" || len(uploadToken.BuildTarget.Tags) == 0 {
			log.Printf("build_image token %s... missing repo/tags", token[:8])
			http.Error(w, "Invalid build target", http.StatusBadRequest)
			return
		}
		repo := uploadToken.BuildTarget.Repo
		tags := uploadToken.BuildTarget.Tags
		log.Printf("Received build-image upload: %d bytes for repo=%s, tags=%v, uploadID=%s, token=%s...", len(tarballData), repo, tags, uploadID, token[:8])
		go processBuildImageDeployment(tarballData, osid, uploadID, repo, tags, uploadToken.Dockerfile, uploadToken.BuildArgs, buildkitConfig)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("Upload received, processing build-image"))
		return
	}

	log.Printf("Received CLI deploy upload: %d bytes for osid=%s, uploadID=%s, token=%s...", len(tarballData), osid, uploadID, token[:8])

	// Persist the source archive in Harbor before kicking off the build.
	pushCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := harborclient.PushArchive(pushCtx, harborclient.Config{
		URL:      buildkitConfig.HarborURL,
		Username: buildkitConfig.HarborUsername,
		Password: buildkitConfig.HarborPassword,
	}, osid, uploadID, tarballData); err != nil {
		log.Printf("Failed to push archive to Harbor (osid=%s, uploadID=%s): %v", osid, uploadID, err)
		http.Error(w, "Failed to store archive", http.StatusInternalServerError)
		return
	}
	log.Printf("Pushed CLI archive to Harbor: osid=%s uploadID=%s", osid, uploadID)

	// Process deployment asynchronously (buildkit treats uploadID as its jobID).
	// uploadToken.Dockerfile carries the path inside the tarball (iter-27 I27-Y);
	// empty defaults to "Dockerfile" at the tarball root. uploadToken.BuildArgs
	// carries the effective build args captured when the token was minted.
	go processCLIDeployment(tarballData, osid, uploadID, uploadToken.Dockerfile, uploadToken.BuildArgs, buildkitConfig)

	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte("Upload received, processing CLI deployment"))
}

// processCLIDeployment handles the full deployment workflow for CLI uploaded tarballs.
//
// Front: create a build-job row (CLI semantics, empty repo/branch, jobID as
// commit), extract the tarball into a temp dir.
// Tail: delegated to processBuildAndDeploy, which is shared with the VCS deploy
// path (the only difference between CLI and VCS at the build+push+patch layer
// is what populated the workdir).
//
// dockerfile is the relative path to the Dockerfile inside the tarball
// (iter-27 I27-Y). Empty means "Dockerfile at the tarball root", which
// preserves the pre-monorepo single-app shape. For a monorepo CLI deploy
// the conductor sends e.g. "apps/api/Dockerfile" via the upload-token
// request; we split it into DockerfileDir + DockerfileFilename so
// BuildKit's `--local dockerfile=` + `--opt filename=` get the right
// values (matching what the VCS path already does for monorepo apps).
func processCLIDeployment(tarballData []byte, osid, jobID, dockerfile string, buildArgs []datastore.BuildArg, buildkitConfig *agentstream.BuildKitConfig) {
	ctx := context.Background()

	// Create K8s client (separate from the synchronous one; goroutine outlives the request)
	k8sClient, err := agentstream.NewK8sClient()
	if err != nil {
		log.Printf("Failed to create K8s client: %v", err)
		return
	}

	// Create build job in database with empty repo/branch, jobID as commit
	if err := datastore.CreateBuildKitJob(jobID, osid, "", "", jobID); err != nil {
		log.Printf("Failed to create build job: %v", err)
		return
	}
	log.Printf("Created CLI deploy build job: job_id=%s, osid=%s", jobID, osid)

	// Create temp directory for extraction
	tmpDir, err := os.MkdirTemp("", "cli-deploy-build-*")
	if err != nil {
		log.Printf("Failed to create temp directory: %v", err)
		datastore.UpdateBuildKitJobStatus(jobID, datastore.JobStatusFailed)
		datastore.InsertBuildKitLog(jobID, fmt.Sprintf("ERROR: Failed to create temp directory: %v", err))
		return
	}
	defer os.RemoveAll(tmpDir) // Clean up after build

	// Extract gzipped tarball
	if err := extractTarball(tarballData, tmpDir); err != nil {
		log.Printf("Failed to extract tarball: %v", err)
		datastore.UpdateBuildKitJobStatus(jobID, datastore.JobStatusFailed)
		datastore.InsertBuildKitLog(jobID, fmt.Sprintf("ERROR: Failed to extract tarball: %v", err))
		return
	}

	log.Printf("Extracted tarball to %s", tmpDir)
	datastore.InsertBuildKitLog(jobID, "Tarball extracted successfully")

	// Iter-27 I27-Y: split the relative dockerfile path inside the tarball
	// into DockerfileDir (absolute) + DockerfileFilename (basename) so
	// BuildKit's `--local dockerfile=` + `--opt filename=` get the right
	// values for monorepo CLI deploys (e.g. "apps/api/Dockerfile" lands as
	// dir=<tmpDir>/apps/api, filename=Dockerfile). Empty dockerfile keeps
	// the pre-monorepo single-app shape (BuildKit's defaults).
	//
	// ContextPath stays empty: CLI deploys always build with the tarball
	// root as the build context. Only the dockerfile location is variable;
	// the source-dir analogue for VCS deploys (build at a subdir of the
	// repo) is enforced CLI-side by the tarball walker, so by the time the
	// tarball lands here, all paths are already relative-to-context.
	dockerfileDir, dockerfileFilename, dfErr := splitTarballDockerfile(tmpDir, dockerfile)
	if dfErr != nil {
		log.Printf("Invalid dockerfile %q for job %s: %v", dockerfile, jobID, dfErr)
		datastore.UpdateBuildKitJobStatus(jobID, datastore.JobStatusFailed)
		datastore.InsertBuildKitLog(jobID, fmt.Sprintf("ERROR: invalid dockerfile %q: %v", dockerfile, dfErr))
		return
	}

	ProcessBuildAndDeploy(ctx, k8sClient, BuildAndDeployInput{
		Workdir:            tmpDir,
		OSID:               osid,
		JobID:              jobID,
		DockerfileDir:      dockerfileDir,
		DockerfileFilename: dockerfileFilename,
		BuildArgs:          buildArgs,
	}, buildkitConfig)
}

// processBuildImageDeployment handles the app-less build-and-push flow
// (obj-47): build a generic uploaded context and push it to explicit Harbor
// target ref(s) under the fixed `runos-apps` project, WITHOUT any app deploy.
//
// It mirrors processCLIDeployment's front (build-job row, tarball extract,
// dockerfile split) but: the job identity is app-less (osid is the target
// repo name, a synthetic identity that keeps the console builds UI
// recognisable), the build is build-only (no deployment patch), and the
// push goes to runos-apps/<repo>:<tag> for every supplied tag from a single
// build. jobID is the conductor-supplied uploadID so conductor can poll
// LIST_BUILD_JOBS/LIST_BUILD_LOGS by it.
func processBuildImageDeployment(tarballData []byte, osid, jobID, repo string, tags []string, dockerfile string, buildArgs []datastore.BuildArg, buildkitConfig *agentstream.BuildKitConfig) {
	ctx := context.Background()

	k8sClient, err := agentstream.NewK8sClient()
	if err != nil {
		log.Printf("Failed to create K8s client: %v", err)
		return
	}

	// App-less build-job row: empty repo/branch (no VCS), jobID as commit.
	if err := datastore.CreateBuildKitJob(jobID, osid, "", "", jobID); err != nil {
		log.Printf("Failed to create build job: %v", err)
		return
	}
	log.Printf("Created build-image build job: job_id=%s, repo=%s, tags=%v", jobID, repo, tags)

	tmpDir, err := os.MkdirTemp("", "build-image-*")
	if err != nil {
		log.Printf("Failed to create temp directory: %v", err)
		datastore.UpdateBuildKitJobStatus(jobID, datastore.JobStatusFailed)
		datastore.InsertBuildKitLog(jobID, fmt.Sprintf("ERROR: Failed to create temp directory: %v", err))
		return
	}
	defer os.RemoveAll(tmpDir)

	if err := extractTarball(tarballData, tmpDir); err != nil {
		log.Printf("Failed to extract tarball: %v", err)
		datastore.UpdateBuildKitJobStatus(jobID, datastore.JobStatusFailed)
		datastore.InsertBuildKitLog(jobID, fmt.Sprintf("ERROR: Failed to extract tarball: %v", err))
		return
	}
	log.Printf("Extracted build-image context to %s", tmpDir)
	datastore.InsertBuildKitLog(jobID, "Build context extracted successfully")

	// dockerfile is relative to the uploaded context root (default
	// "Dockerfile"); split into dir + filename for BuildKit. The same
	// path-traversal guard the app-deploy path uses applies.
	dockerfileDir, dockerfileFilename, dfErr := splitTarballDockerfile(tmpDir, dockerfile)
	if dfErr != nil {
		log.Printf("Invalid dockerfile %q for job %s: %v", dockerfile, jobID, dfErr)
		datastore.UpdateBuildKitJobStatus(jobID, datastore.JobStatusFailed)
		datastore.InsertBuildKitLog(jobID, fmt.Sprintf("ERROR: invalid dockerfile %q: %v", dockerfile, dfErr))
		return
	}

	ProcessBuildAndDeploy(ctx, k8sClient, BuildAndDeployInput{
		Workdir:            tmpDir,
		OSID:               osid,
		JobID:              jobID,
		DockerfileDir:      dockerfileDir,
		DockerfileFilename: dockerfileFilename,
		BuildArgs:          buildArgs,
		BuildOnly:          true,
		TargetRepo:         repo,
		TargetTags:         tags,
	}, buildkitConfig)
}

// splitTarballDockerfile resolves a relative dockerfile path (as sent by
// the CLI in the `runos.yaml`'s `dockerfile:` field, e.g.
// "apps/api/Dockerfile") into an absolute DockerfileDir + bare filename
// suitable for BuildKit's `--local dockerfile=DIR` + `--opt filename=NAME`.
//
// Empty dockerfile returns empty strings (BuildKit's defaults: context
// root + "Dockerfile"), preserving the pre-monorepo single-app shape.
//
// Path-traversal: the resolved path must stay inside tarballRoot. The
// CLI's pre-flight `ResolveDockerfilePath` already gates this, but
// validating here too keeps the cluster-agent's trust boundary clear
// (the tarball could come from a malicious upload token recipient).
func splitTarballDockerfile(tarballRoot, dockerfile string) (string, string, error) {
	trimmed := strings.TrimSpace(dockerfile)
	if trimmed == "" {
		return "", "", nil
	}
	if filepath.IsAbs(trimmed) {
		return "", "", fmt.Errorf("dockerfile must be relative to the tarball root, got absolute path %q", trimmed)
	}

	absRoot, err := filepath.Abs(tarballRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolving tarball root: %w", err)
	}
	joined := filepath.Join(absRoot, trimmed)
	resolved, err := filepath.Abs(joined)
	if err != nil {
		return "", "", fmt.Errorf("resolving dockerfile path: %w", err)
	}

	// Path-traversal guard: a malicious `../etc/passwd` style path is
	// rejected even if it happens to exist on disk.
	rel, err := filepath.Rel(absRoot, resolved)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", "", fmt.Errorf("dockerfile %q escapes tarball root", trimmed)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", "", fmt.Errorf("dockerfile %q not found inside tarball: %w", trimmed, err)
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("dockerfile %q is not a regular file", trimmed)
	}

	return filepath.Dir(resolved), filepath.Base(resolved), nil
}

// BuildAndDeployInput is the per-deploy input to ProcessBuildAndDeploy.
//
// JobID is the BuildKit row id (unique per attempt). Tag is what the
// resulting image is tagged with in Harbor. They are deliberately
// decoupled: VCS deploys retry the same SHA → JobID needs a uuid suffix
// for row-uniqueness, but the image tag stays SHA-keyed so the Harbor
// presence check on the next deploy of that SHA can short-circuit the
// build. CLI deploys leave Tag empty and the executor falls back to
// JobID — matches the original cliUploadId-as-tag behaviour.
//
// ContextPath / DockerfileDir / DockerfileFilename are optional overrides
// that let VCS deploys pin BuildKit at a non-root build context (monorepo
// apps) or at a non-default Dockerfile name. CLI deploys leave them empty
// and inherit the pre-monorepo behaviour (context = Workdir,
// dockerfile = Workdir/Dockerfile).
type BuildAndDeployInput struct {
	Workdir            string
	OSID               string
	JobID              string
	Tag                string
	ContextPath        string
	DockerfileDir      string
	DockerfileFilename string
	// BuildArgs are the effective Docker build args ([{key,value,source}])
	// merged + validated by conductor. ProcessBuildAndDeploy persists them
	// against the build job row and forwards them to BuildKit. Empty for
	// deploys with no build args (and for older conductors that don't send
	// the field).
	BuildArgs []datastore.BuildArg
	// BuildOnly is the standalone `apps build` path: when true,
	// ProcessBuildAndDeploy returns after a successful Harbor push and does
	// NOT patch the deployment (no rollout). Default false keeps the
	// build+push+patch behaviour both deploy paths have always had.
	BuildOnly bool
	// Repo (credential-stripped clone URL) and Branch annotate the per-build
	// BuildKit pod for manual inspection. Set by VCS deploys; empty for CLI
	// deploys and app-less build-image builds.
	Repo   string
	Branch string
	// TargetRepo + TargetTags drive the app-less build-image push (obj-47):
	// when TargetRepo is set, the build pushes to `runos-apps/<TargetRepo>:<tag>`
	// for every tag in TargetTags (the same built image to each, in a single
	// build) instead of the historical `runos-apps/<OSID>:<Tag>` app-image
	// ref. The project is fixed to runos-apps. Empty for app deploys.
	TargetRepo string
	TargetTags []string
}

// ProcessBuildAndDeploy runs the shared build+push+patch tail used by both the
// CLI-deploy and VCS-deploy flows. Caller is responsible for:
//   - Creating the BuildKitJob row in the datastore (so this function can write
//     log entries and status updates against it).
//   - Populating workdir (CLI-deploy: extract tarball; VCS-deploy: git clone
//   - checkout).
//   - Cleaning up workdir afterwards if it owns the lifecycle.
//
// The image tag pushed to Harbor is `<harbor>/runos-apps/<osid>:<jobID>`.
// For CLI deploys jobID is the cliUploadId (a UUID); for VCS deploys it is the
// commit SHA. Both shapes work because the cluster agent only ever uses jobID
// as an opaque tag.
func ProcessBuildAndDeploy(ctx context.Context, k8sClient *agentstream.K8sClient, in BuildAndDeployInput, buildkitConfig *agentstream.BuildKitConfig) {
	// CLI deploys pass JobID and leave Tag empty; the cliUploadId then doubles
	// as the image tag (matches pre-decoupling behaviour). VCS deploys pass
	// SHA as Tag and a uuid-suffixed JobID, so retries don't collide on the
	// build_kit unique constraint while the image stays SHA-keyed.
	tag := in.Tag
	if tag == "" {
		tag = in.JobID
	}

	commitShort := tag
	if len(tag) > 7 {
		commitShort = tag[:7]
	}

	// Persist the effective build args against the build job row before the
	// build runs, so the record reflects exactly what was sent to BuildKit
	// even if the build later fails. No-op when there are none. Both deploy
	// paths funnel through here and the job row already exists by now, so
	// this is the single place that records args for CLI and VCS alike.
	if err := datastore.InsertBuildKitJobArgs(in.JobID, in.BuildArgs); err != nil {
		// Non-fatal: a failure to record the audit rows shouldn't abort an
		// otherwise-valid build. Log and continue.
		log.Printf("Failed to persist build args for job %s: %v", in.JobID, err)
	} else if len(in.BuildArgs) > 0 {
		datastore.InsertBuildKitLog(in.JobID, fmt.Sprintf("Applying %d build arg(s)", len(in.BuildArgs)))
	}

	// App-less build-image (obj-47): push to runos-apps/<repo>:<tag> for
	// every supplied tag from a single build. When TargetRepo is empty this
	// stays nil and the build falls back to the historical osid/commit ref.
	harborRegistry := buildkitclient.StripRegistryProtocol(buildkitConfig.HarborURL)
	var targetImages []string
	if in.TargetRepo != "" {
		targetImages = buildkitclient.TargetImageRefs(harborRegistry, in.TargetRepo, in.TargetTags)
	}

	buildConfig := buildkitclient.BuildConfig{
		JobID:              in.JobID,
		OSID:               in.OSID,
		Commit:             tag,
		CommitShort:        commitShort,
		LocalContextPath:   in.Workdir,
		ContextPath:        in.ContextPath,
		DockerfileDir:      in.DockerfileDir,
		DockerfileFilename: in.DockerfileFilename,
		BuildArgs:          in.BuildArgs,
		TargetImages:       targetImages,
		Repo:               in.Repo,
		Branch:             in.Branch,
		HarborURL:          buildkitConfig.HarborURL,
		HarborUsername:     buildkitConfig.HarborUsername,
		HarborPassword:     buildkitConfig.HarborPassword,
	}

	if err := buildkitclient.Build(ctx, k8sClient.GetClientset(), buildConfig); err != nil {
		log.Printf("Build failed for job %s: %v", in.JobID, err)
		return
	}

	log.Printf("Build succeeded for job %s", in.JobID)

	// imageTag is the human-facing ref for logs. For a build-image push it is
	// the explicit target ref(s); otherwise the historical app-image ref.
	imageTag := fmt.Sprintf("%s/runos-apps/%s:%s", harborRegistry, in.OSID, tag)
	if len(targetImages) > 0 {
		imageTag = strings.Join(targetImages, ", ")
	}

	// Build-only path (standalone `apps build`): the image is built and
	// pushed to Harbor, and the build_kit row is already JobStatusSuccess
	// from Build(). Stop here without patching the deployment — no rollout.
	// The push is byte-identical to a deploy build at the same SHA + args,
	// so a later run/deploy at this SHA reuses it from Harbor.
	if in.BuildOnly {
		log.Printf("Build-only complete for job %s, osid=%s, image=%s (no rollout)", in.JobID, in.OSID, imageTag)
		datastore.InsertBuildKitLog(in.JobID, fmt.Sprintf("Image built and pushed: %s (build-only, deployment not patched)", imageTag))
		return
	}

	if err := patchDeploymentImage(ctx, k8sClient.GetClientset(), in.OSID, imageTag); err != nil {
		log.Printf("Failed to patch deployment for job %s: %v", in.JobID, err)
		datastore.InsertBuildKitLog(in.JobID, fmt.Sprintf("ERROR: Failed to patch deployment: %v", err))
		// BuildKit's Build() already flipped status to "success" when the
		// image push completed. Without this overwrite the build_kit row
		// would claim success for the whole pipeline even though the
		// deployment never received the new image, leaving the conductor's
		// poller (and any operator looking at the row) blind to the
		// failure. Re-status as failed so the wait-for-build step in
		// deploy.cli surfaces a clean error to the CLI.
		datastore.UpdateBuildKitJobStatus(in.JobID, datastore.JobStatusFailed)
		return
	}

	log.Printf("Deployment succeeded for job %s, osid=%s, image=%s", in.JobID, in.OSID, imageTag)
	datastore.InsertBuildKitLog(in.JobID, fmt.Sprintf("Deployment patched successfully with image: %s", imageTag))
}

// patchDeploymentImage updates the deployment's container image with retry logic
func patchDeploymentImage(ctx context.Context, clientset *kubernetes.Clientset, osid, imageTag string) error {
	const (
		maxRetries    = 60
		retryInterval = 5 * time.Second
	)

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Get the deployment (name and namespace are both osid)
		deployment, err := clientset.AppsV1().Deployments(osid).Get(ctx, osid, metav1.GetOptions{})
		if err != nil {
			lastErr = fmt.Errorf("deployment not found: %w", err)
			log.Printf("Attempt %d/%d: deployment %s not found, retrying in %v...", attempt, maxRetries, osid, retryInterval)
			time.Sleep(retryInterval)
			continue
		}

		// Update the first container's image
		if len(deployment.Spec.Template.Spec.Containers) == 0 {
			return fmt.Errorf("deployment has no containers")
		}
		deployment.Spec.Template.Spec.Containers[0].Image = imageTag

		// Update the deployment
		_, err = clientset.AppsV1().Deployments(osid).Update(ctx, deployment, metav1.UpdateOptions{})
		if err != nil {
			lastErr = fmt.Errorf("failed to update deployment: %w", err)
			log.Printf("Attempt %d/%d: failed to update deployment %s, retrying in %v...", attempt, maxRetries, osid, retryInterval)
			time.Sleep(retryInterval)
			continue
		}

		log.Printf("Patched deployment %s in namespace %s with image %s", osid, osid, imageTag)
		return nil
	}

	return fmt.Errorf("failed after %d attempts: %w", maxRetries, lastErr)
}

// extractTarball extracts a gzipped tarball to the destination directory
func extractTarball(data []byte, destDir string) error {
	// Create gzip reader
	gzipReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzipReader.Close()

	// Create tar reader
	tarReader := tar.NewReader(gzipReader)

	// Decompression-bomb defenses accumulated across the whole archive: the
	// per-file MaxFileSize LimitReader (below) bounds any single entry, but not
	// the aggregate. Track total decompressed bytes and the entry count and
	// abort if either cap is exceeded.
	var totalBytes int64
	entryCount := 0

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read error: %w", err)
		}

		entryCount++
		if entryCount > MaxEntryCount {
			return fmt.Errorf("tarball has too many entries (limit %d): possible exhaustion attack", MaxEntryCount)
		}

		// Skip root directory entry
		if header.Name == "./" || header.Name == "." {
			continue
		}

		// Security: prevent path traversal
		targetPath := filepath.Join(destDir, header.Name)
		cleanTarget := filepath.Clean(targetPath)
		cleanDest := filepath.Clean(destDir)
		if cleanTarget != cleanDest && !strings.HasPrefix(cleanTarget, cleanDest+string(os.PathSeparator)) {
			return fmt.Errorf("invalid file path in tarball: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return fmt.Errorf("failed to create directory: %w", err)
			}
		case tar.TypeReg:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return fmt.Errorf("failed to create parent directory: %w", err)
			}

			outFile, err := os.Create(targetPath)
			if err != nil {
				return fmt.Errorf("failed to create file: %w", err)
			}

			// Limit per-file size to prevent a single huge decompressed entry.
			limited := io.LimitReader(tarReader, MaxFileSize)
			written, err := io.Copy(outFile, limited)
			if err != nil {
				outFile.Close()
				return fmt.Errorf("failed to write file: %w", err)
			}
			outFile.Close()

			// Accumulate against the total-decompressed-bytes cap. Done after
			// the write (the file is already on disk and will be cleaned up with
			// the temp dir) so the very next entry can't push us past the bound.
			totalBytes += written
			if totalBytes > MaxTotalExtractedBytes {
				return fmt.Errorf("tarball decompresses to more than %d bytes: possible decompression bomb", MaxTotalExtractedBytes)
			}

			// Set file permissions
			if err := os.Chmod(targetPath, os.FileMode(header.Mode)); err != nil {
				// Non-fatal, just log
				log.Printf("Warning: failed to set permissions on %s: %v", targetPath, err)
			}
		case tar.TypeSymlink:
			// Reject absolute link targets outright (e.g. /etc/passwd): the
			// contract is that every link stays inside the extracted tree.
			if filepath.IsAbs(header.Linkname) {
				return fmt.Errorf("symlink target is absolute: %s -> %s", header.Name, header.Linkname)
			}
			// Resolve the link target relative to the symlink's own directory
			// and apply the SAME containment check used for regular files
			// above: a bare HasPrefix is bypassable by a sibling sharing the
			// destDir prefix (e.g. /data/app vs /data/app-evil), so guard with
			// a trailing path separator. Validate BEFORE creating the symlink.
			resolvedLink := filepath.Clean(filepath.Join(filepath.Dir(targetPath), header.Linkname))
			if resolvedLink != cleanDest && !strings.HasPrefix(resolvedLink, cleanDest+string(os.PathSeparator)) {
				return fmt.Errorf("symlink escapes destination: %s -> %s", header.Name, header.Linkname)
			}
			if err := os.Symlink(header.Linkname, targetPath); err != nil {
				return fmt.Errorf("failed to create symlink: %w", err)
			}
		}
	}

	return nil
}
