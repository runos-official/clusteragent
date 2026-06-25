package instructions

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/runos-official/clusteragent/commons"
	"gopkg.in/yaml.v3"
)

// VcsFetchSourceRequest is the input for VCS_FETCH_SOURCE.
//
// Conductor mints credentials (e.g. a GitHub App installation token, a GitLab
// PAT) and embeds them into RepoCloneURL itself in the standard
// `https://<user>:<token>@host/path.git` form before sending. AuthHeader is
// reserved for providers where credentials must be passed via HTTP header
// rather than URL — currently unused; v1 only handles in-URL credentials.
//
// Branch is informational (recorded on the build job row for the console UI);
// the actual fetch is by SHA and depth=1, no branch involved.
//
// ConfigPath is the repo-relative path to the runos.yaml the cluster agent
// reads after cloning. Default `runos.yaml` (single-app repos at root).
// Monorepos pass a per-app path like `apps/billing/infra/runos-prod.yaml`.
// The directory of this path is also where the cluster agent looks for
// sibling `runos.service.*.yaml` files. Path traversal (../, absolute paths)
// is rejected; the resolved file must stay within the workdir.
type VcsFetchSourceRequest struct {
	OSID         string `json:"osid"`
	SHA          string `json:"sha"`
	RepoCloneURL string `json:"repoCloneUrl"`
	AuthHeader   string `json:"authHeader,omitempty"`
	Branch       string `json:"branch,omitempty"`
	ConfigPath   string `json:"configPath,omitempty"`
}

// VcsFetchSourceServiceYaml carries one runos.service.*.yaml file's contents
// back to the conductor for reconciliation.
type VcsFetchSourceServiceYaml struct {
	Filename       string `json:"filename"`
	ContentsBase64 string `json:"contentsBase64"`
}

// VcsFetchSourceResponse returns the manifests the conductor needs to
// reconcile (runos.yaml + every sibling runos.service.*.yaml of the
// configPath directory) plus a cache key the subsequent VCS_BUILD call
// references to find the cloned workdir on disk.
//
// EnvVars / SecretEnvVars carry the RESOLVED key/value contents of the
// committed env files the manifest references via `env:` / `secretEnv:`,
// read from the manifest's own directory in the checkout. The cluster agent
// is the only party with the checkout on a VCS deploy, so it resolves these
// here (the CLI's local-deploy analogue) and hands the conductor already-
// parsed maps — the same shape prepare-cli-deployment delivers as
// customEnvVars / customSecretEnvVars. The conductor reconciles EnvVars into
// the app's ConfigMap and SecretEnvVars into its Secret. Both omitempty:
// absent means "no env vars from that source", not an error.
type VcsFetchSourceResponse struct {
	Success         bool                        `json:"success"`
	WorkdirCacheKey string                      `json:"workdirCacheKey,omitempty"`
	RunosYamlBase64 string                      `json:"runosYamlBase64,omitempty"`
	ServiceYamls    []VcsFetchSourceServiceYaml `json:"serviceYamls,omitempty"`
	EnvVars         map[string]string           `json:"envVars,omitempty"`
	SecretEnvVars   map[string]string           `json:"secretEnvVars,omitempty"`
	Message         string                      `json:"message,omitempty"`
}

const maxManifestFileSize = 1 * 1024 * 1024 // 1 MB cap per yaml file

// gitStepTimeout bounds each individual git invocation (clone, fetch, checkout)
// so a hung clone against an unreachable or slow remote can't pin a worker
// indefinitely. GIT_TERMINAL_PROMPT=0 already prevents stdin hangs on bad
// creds; this also covers a server that accepts the connection then stalls.
const gitStepTimeout = 5 * time.Minute

var serviceYamlNamePattern = regexp.MustCompile(`^runos\.service\.[^/\\]+\.yaml$`)

// gitSHAPattern accepts only a 7-64 char hex string: a (possibly abbreviated)
// git object id. req.SHA is passed positionally into `git fetch`/`git checkout`,
// so anything option-like (`--upload-pack=...`, a leading dash, a path) must be
// rejected before it ever reaches the command line.
var gitSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// validateGitSHA rejects any SHA that is not a plain 7-64 char hex object id.
func validateGitSHA(sha string) error {
	if !gitSHAPattern.MatchString(sha) {
		return fmt.Errorf("invalid sha %q: must be 7-64 hex characters", sha)
	}
	return nil
}

// runosYamlBuildPaths is the minimal slice of runos.yaml the cluster agent
// reads to drive BuildKit. Anything else (replicas, env, etc.) is parsed by
// the conductor when it receives the base64'd yaml. Tags match the field
// names accepted by conductor's normalizeYaml so the same yaml works on
// both sides.
type runosYamlBuildPaths struct {
	SourceDir  string `yaml:"sourceDir"`
	Dockerfile string `yaml:"dockerfile"`
}

// VcsFetchSource handles the VCS_FETCH_SOURCE instruction.
//
// Sync request/response: clones the repo at the requested SHA into a
// workdir keyed by {osid, sha} in the in-memory cache, reads the committed
// runos.yaml at request.ConfigPath (default `runos.yaml`) plus every
// sibling runos.service.*.yaml, parses out the build context + Dockerfile
// location, and returns the yamls base64-encoded so conductor can run its
// existing reconciliation paths.
//
// The workdir is left on disk for the matching VCS_BUILD call to consume.
// The cache's TTL janitor cleans up abandoned entries after 30 minutes; an
// explicit cleanup happens after VCS_BUILD finishes regardless of outcome.
func VcsFetchSource(jsonB64 string) (string, string, error) {
	const replyType = "VCS_FETCH_SOURCE_RESPONSE"

	var req VcsFetchSourceRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		log.Printf("VCS_FETCH_SOURCE: decode error: %v", err)
		return "", "", err
	}

	if req.OSID == "" || req.SHA == "" || req.RepoCloneURL == "" {
		return reply(replyType, VcsFetchSourceResponse{
			Success: false,
			Message: "osid, sha, and repoCloneUrl are required",
		})
	}

	// Validate the SHA before it reaches any git invocation: it is passed
	// positionally into `git fetch`/`git checkout`, so an option-like value
	// (e.g. `--upload-pack=...`) would otherwise be interpreted as a flag.
	if err := validateGitSHA(req.SHA); err != nil {
		return reply(replyType, VcsFetchSourceResponse{
			Success: false,
			Message: err.Error(),
		})
	}

	configPath := strings.TrimSpace(req.ConfigPath)
	if configPath == "" {
		configPath = "runos.yaml"
	}

	workdir := vcsWorkdirPathFor(req.OSID, req.SHA)

	// On any failure path after this point we remove the (possibly
	// partial) workdir so failed fetches don't leak scratch dirs that
	// could fill /tmp on a busy worker. The workdir path is unique per
	// call (random suffix in vcsWorkdirPathFor), so a concurrent fetch
	// for the same {osid, sha} cannot have its tree wiped here.
	fetchOK := false
	defer func() {
		if fetchOK {
			return
		}
		if err := os.RemoveAll(workdir); err != nil {
			log.Printf("VCS_FETCH_SOURCE: cleanup of failed workdir %s: %v", workdir, err)
		}
	}()

	if err := os.MkdirAll(filepath.Dir(workdir), 0o755); err != nil {
		return reply(replyType, VcsFetchSourceResponse{
			Success: false,
			Message: fmt.Sprintf("could not create workdir parent: %v", err),
		})
	}

	if err := gitFetchSHA(req.RepoCloneURL, req.SHA, workdir); err != nil {
		return reply(replyType, VcsFetchSourceResponse{
			Success: false,
			Message: err.Error(),
		})
	}

	// Resolve the yaml file path inside workdir, refusing anything that
	// escapes the clone (absolute paths, `..` past the workdir root).
	resolvedYamlPath, err := resolveInsideWorkdir(workdir, configPath)
	if err != nil {
		return reply(replyType, VcsFetchSourceResponse{
			Success: false,
			Message: fmt.Sprintf("invalid configPath %q: %v", configPath, err),
		})
	}

	// configDir is where service yamls live and is the anchor for sourceDir.
	configDir := filepath.Dir(resolvedYamlPath)

	runosYaml, paths, err := readRunosYaml(resolvedYamlPath, workdir, configDir)
	if err != nil {
		return reply(replyType, VcsFetchSourceResponse{
			Success: false,
			Message: fmt.Sprintf("read runos.yaml at %s: %v", configPath, err),
		})
	}

	serviceYamls, err := readSiblingServiceYamls(configDir)
	if err != nil {
		return reply(replyType, VcsFetchSourceResponse{
			Success: false,
			Message: fmt.Sprintf("read service yamls in %s: %v", configDir, err),
		})
	}

	// Resolve the committed env files the manifest references, anchored at
	// the manifest's own directory (NOT the clone-root). A missing committed
	// plain-env file fails loud here rather than letting the app deploy with
	// empty env (the silent-allowlist-disable footgun this fixes).
	plainEnv, secretEnv, err := resolveManifestEnvVars(resolvedYamlPath, workdir, configDir)
	if err != nil {
		return reply(replyType, VcsFetchSourceResponse{
			Success: false,
			Message: fmt.Sprintf("resolve env files for %s: %v", configPath, err),
		})
	}

	cacheKey := vcsWorkdirCacheKey(req.OSID, req.SHA)
	vcsWorkdirCachePut(req.OSID, req.SHA, VcsWorkdirBuildPaths{
		Workdir:            workdir,
		ContextPath:        paths.ContextPath,
		DockerfileDir:      paths.DockerfileDir,
		DockerfileFilename: paths.DockerfileFilename,
	})

	log.Printf("VCS_FETCH_SOURCE: osid=%s sha=%s workdir=%s configPath=%s context=%s dockerfile=%s/%s manifests=%d plainEnvKeys=%d secretEnvKeys=%d",
		req.OSID, req.SHA, workdir, configPath, paths.ContextPath, paths.DockerfileDir, paths.DockerfileFilename, len(serviceYamls), len(plainEnv), len(secretEnv))

	fetchOK = true
	return reply(replyType, VcsFetchSourceResponse{
		Success:         true,
		WorkdirCacheKey: cacheKey,
		RunosYamlBase64: runosYaml,
		ServiceYamls:    serviceYamls,
		EnvVars:         plainEnv,
		SecretEnvVars:   secretEnv,
	})
}

// gitFetchSHA performs the three-step shallow checkout used for VCS deploys.
// Errors are returned with stderr included so conductor surfaces actionable
// messages rather than `exit status 128`.
func gitFetchSHA(authedURL, sha, workdir string) error {
	steps := [][]string{
		{"git", "clone", "--depth=1", "--no-checkout", authedURL, workdir},
		{"git", "-C", workdir, "fetch", "--depth=1", "origin", sha},
		// `<sha> --` (commit-ish, then the option/pathspec terminator) is the
		// correct disambiguation. `checkout -- <sha>` would treat the sha as a
		// pathspec and fail. Combined with validateGitSHA, the sha can never be
		// parsed as an option.
		{"git", "-C", workdir, "checkout", sha, "--"},
	}

	for _, args := range steps {
		// Each git step gets its own timeout via CommandContext so a hung
		// network op (slow/unreachable remote that accepts the TCP connection
		// then stalls) is killed instead of pinning the worker forever.
		ctx, cancel := context.WithTimeout(context.Background(), gitStepTimeout)
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Env = append(os.Environ(),
			// Disable interactive prompts; if creds in the URL fail we want
			// a fast hard error, not a hung process waiting on stdin.
			"GIT_TERMINAL_PROMPT=0",
			"GIT_ASKPASS=/bin/true",
		)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			// Strip the URL from any error string so embedded creds don't
			// land in conductor logs / job state. The URL appears in the
			// first step's command, never in stderr — but redact just in
			// case git ever decides to echo it.
			msg := strings.ReplaceAll(string(out), authedURL, "<repoUrl>")
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("git %s timed out after %s: %s", args[1], gitStepTimeout, strings.TrimSpace(msg))
			}
			return fmt.Errorf("git %s failed: %s", args[1], strings.TrimSpace(msg))
		}
	}

	return nil
}

// readRunosYaml reads, base64-encodes, and parses the runos.yaml at
// resolvedPath. Returns the encoded contents plus the resolved BuildKit
// paths (context dir, Dockerfile dir, Dockerfile filename) anchored under
// workdir. Missing or empty yaml is OK — VCS_BUILD just falls back to the
// repo-root + Dockerfile defaults.
func readRunosYaml(resolvedPath, workdir, configDir string) (string, VcsWorkdirBuildPaths, error) {
	defaults := VcsWorkdirBuildPaths{
		Workdir:            workdir,
		ContextPath:        workdir,
		DockerfileDir:      workdir,
		DockerfileFilename: "Dockerfile",
	}

	info, err := os.Stat(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Hard-fail rather than silently building against repo root +
			// Dockerfile. The earlier behaviour was a footgun: monorepo
			// yamls that lived in subdirectories would be ignored, the
			// build would run with the wrong context, and BuildKit would
			// fail later with "open Dockerfile: no such file or directory"
			// without it being obvious that the cluster agent never read
			// the user's manifest. Failing here surfaces the real cause.
			rel, _ := filepath.Rel(workdir, resolvedPath)
			if rel == "" {
				rel = resolvedPath
			}
			return "", VcsWorkdirBuildPaths{}, fmt.Errorf(
				"runos.yaml not found at %s in the committed tree at this SHA. "+
					"If your repo lives in a monorepo subdirectory, set configPath on the app to point at the per-app yaml "+
					"(e.g. `apps/billing/runos.yaml`); otherwise verify that runos.yaml is committed at the repo root",
				rel,
			)
		}
		return "", VcsWorkdirBuildPaths{}, err
	}
	if info.Size() > maxManifestFileSize {
		return "", VcsWorkdirBuildPaths{}, fmt.Errorf("runos.yaml exceeds %d byte limit", maxManifestFileSize)
	}

	bytes, err := os.ReadFile(resolvedPath)
	if err != nil {
		return "", VcsWorkdirBuildPaths{}, err
	}
	encoded := base64.StdEncoding.EncodeToString(bytes)

	var parsed runosYamlBuildPaths
	if err := yaml.Unmarshal(bytes, &parsed); err != nil {
		// We only need sourceDir + dockerfile out of the yaml; if it doesn't
		// parse as a yaml document at all, treat it as "use defaults" and
		// surface the encoded yaml back to conductor where the full parse
		// happens. The deploy will fail there with a richer error.
		log.Printf("VCS_FETCH_SOURCE: runos.yaml minimal parse failed (%v); falling back to defaults", err)
		return encoded, defaults, nil
	}

	// Resolve sourceDir relative to the yaml's directory.
	sourceDir := strings.TrimSpace(parsed.SourceDir)
	if sourceDir == "" {
		sourceDir = "."
	}
	contextPath, err := resolveInsideWorkdir(workdir, filepath.Join(relTo(workdir, configDir), sourceDir))
	if err != nil {
		return encoded, VcsWorkdirBuildPaths{}, fmt.Errorf("invalid sourceDir %q: %v", sourceDir, err)
	}

	// Resolve dockerfile relative to the build context. A bare filename like
	// "Dockerfile.prod" stays in contextPath; a path like "docker/Dockerfile"
	// puts the dockerfile dir at contextPath/docker.
	dockerfileRel := strings.TrimSpace(parsed.Dockerfile)
	if dockerfileRel == "" {
		dockerfileRel = "Dockerfile"
	}
	dockerfileFull, err := resolveInsideWorkdir(workdir, filepath.Join(relTo(workdir, contextPath), dockerfileRel))
	if err != nil {
		return encoded, VcsWorkdirBuildPaths{}, fmt.Errorf("invalid dockerfile %q: %v", dockerfileRel, err)
	}

	return encoded, VcsWorkdirBuildPaths{
		Workdir:            workdir,
		ContextPath:        contextPath,
		DockerfileDir:      filepath.Dir(dockerfileFull),
		DockerfileFilename: filepath.Base(dockerfileFull),
	}, nil
}

// readSiblingServiceYamls reads every runos.service.*.yaml in configDir.
// Files past `maxManifestFileSize` are rejected so a malicious or accidental
// giant yaml can't blow up the gRPC response.
func readSiblingServiceYamls(configDir string) ([]VcsFetchSourceServiceYaml, error) {
	entries, err := os.ReadDir(configDir)
	if err != nil {
		// configDir must exist (it's the directory of the resolved yaml
		// path), so a read error here is a real problem.
		return nil, err
	}

	var services []VcsFetchSourceServiceYaml
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !serviceYamlNamePattern.MatchString(name) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", name, err)
		}
		if info.Size() > maxManifestFileSize {
			return nil, fmt.Errorf("%s exceeds %d byte limit", name, maxManifestFileSize)
		}

		bytes, err := os.ReadFile(filepath.Join(configDir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		services = append(services, VcsFetchSourceServiceYaml{
			Filename:       name,
			ContentsBase64: base64.StdEncoding.EncodeToString(bytes),
		})
	}

	return services, nil
}

// runosYamlEnvRefs is the slice of runos.yaml the cluster agent reads to
// locate the env files the manifest references. Both fields are FILENAMES
// (paths relative to the manifest's directory), matching the CLI's
// DeployConfig.Env / .SecretEnv so a yaml is interpreted identically on both
// deploy paths.
type runosYamlEnvRefs struct {
	Env       string `yaml:"env"`
	SecretEnv string `yaml:"secretEnv"`
}

// resolveManifestEnvVars reads the env: / secretEnv: references from the
// committed runos.yaml at resolvedYamlPath and returns the resolved
// key/value maps the conductor reconciles into the app's ConfigMap (plain)
// and Secret (secret). Both refs resolve relative to the manifest's own
// directory (configDir), NOT the repo clone-root — resolving against the
// clone-root is the bug this fixes: a monorepo-subdir app loaded EMPTY env
// (dropping ALLOWED_CIDRS and silently disabling its in-app IP allowlist).
//
// Plain env (`env:`, committed to VCS): an explicit reference to a file
// missing from the checkout is an operator mistake (typo, wrong relative
// path); we FAIL LOUD rather than deploy empty, mirroring the CLI's
// VerifyExplicitEnvFiles. That is what surfaces the empty-allowlist footgun
// instead of hiding it.
//
// Secret env (`secretEnv:`, gitignored by convention): the secret file is
// NOT committed, so on a VCS checkout it is normally absent. Its absence is
// EXPECTED, not an error — VCS secrets are managed in server state
// (`runos apps secret-env-vars set`) and injected by the conductor, not read
// from the clone. We resolve+ship it only if it IS present in the checkout
// and skip silently otherwise; failing loud here would break every
// conventional VCS deploy.
func resolveManifestEnvVars(resolvedYamlPath, workdir, configDir string) (plain, secret map[string]string, err error) {
	data, err := os.ReadFile(resolvedYamlPath)
	if err != nil {
		return nil, nil, err
	}
	var refs runosYamlEnvRefs
	if err := yaml.Unmarshal(data, &refs); err != nil {
		// A yaml that doesn't parse is surfaced to the conductor by
		// readRunosYaml (which runs first); env resolution just no-ops.
		return nil, nil, nil
	}

	plain, err = readEnvFileRef(strings.TrimSpace(refs.Env), workdir, configDir, true)
	if err != nil {
		return nil, nil, err
	}
	secret, err = readEnvFileRef(strings.TrimSpace(refs.SecretEnv), workdir, configDir, false)
	if err != nil {
		return nil, nil, err
	}
	return plain, secret, nil
}

// readEnvFileRef resolves one env-file reference relative to configDir,
// confined to the clone (workdir), reads it, and dotenv-parses it into a
// key/value map. An empty ref returns (nil, nil). failOnMissing chooses the
// missing-file policy: true for the committed plain-env file (a missing
// explicit reference is an operator error → fail loud), false for the
// gitignored secret-env file (expected absence on a checkout → skip).
func readEnvFileRef(ref, workdir, configDir string, failOnMissing bool) (map[string]string, error) {
	if ref == "" {
		return nil, nil
	}
	// Anchor the ref at the manifest's directory and confine the result to
	// the clone, reusing the same traversal guard as sourceDir / service
	// yamls. An interior `..` that stays inside the repo (a shared env file
	// at a parent path) is allowed; an escape to the worker FS is rejected.
	rel := filepath.Join(relTo(workdir, configDir), ref)
	resolved, err := resolveInsideWorkdir(workdir, rel)
	if err != nil {
		return nil, fmt.Errorf("invalid env file ref %q: %v", ref, err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			if failOnMissing {
				shown, relErr := filepath.Rel(workdir, resolved)
				if relErr != nil {
					shown = ref
				}
				return nil, fmt.Errorf(
					"env file %q referenced by runos.yaml is not committed at this SHA (looked at %q). "+
						"Commit the file, or drop the `env:` field if the app needs no plain env vars — "+
						"deploying with empty env would silently drop config like ALLOWED_CIDRS",
					ref, shown,
				)
			}
			// Secret-env (gitignored) absence is expected on a VCS checkout.
			return nil, nil
		}
		return nil, err
	}
	if info.Size() > maxManifestFileSize {
		return nil, fmt.Errorf("env file %q exceeds %d byte limit", ref, maxManifestFileSize)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read env file %q: %w", ref, err)
	}
	parsed := parseDotenv(data)
	if err := validateDotenvValues(parsed); err != nil {
		return nil, fmt.Errorf("env file %q: %w", ref, err)
	}
	return parsed, nil
}

// resolveInsideWorkdir cleans rel against workdir and refuses any result
// that escapes the workdir tree. Returns the absolute path on success.
// Reject absolute paths in `rel` outright — the contract is "everything is
// repo-relative."
func resolveInsideWorkdir(workdir, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths not allowed")
	}
	cleaned := filepath.Clean(filepath.Join(workdir, rel))
	cleanedWorkdir := filepath.Clean(workdir)
	if cleaned != cleanedWorkdir && !strings.HasPrefix(cleaned, cleanedWorkdir+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes workdir")
	}
	return cleaned, nil
}

// relTo returns child relative to parent, or the cleaned child path if rel
// fails (defensive — callers always pass paths produced by resolveInsideWorkdir
// so this should never trip).
func relTo(parent, child string) string {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return filepath.Clean(child)
	}
	return rel
}

// reply is a tiny helper that returns the (replyType, encoded-response, err)
// tuple the agent stream expects, collapsing the boilerplate of the b64
// encode at every error branch.
func reply(replyType string, payload VcsFetchSourceResponse) (string, string, error) {
	encoded, err := commons.JsonB64Encode(payload)
	if err != nil {
		return "", "", err
	}
	return replyType, encoded, nil
}
