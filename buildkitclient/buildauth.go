package buildkitclient

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Build-time authentication (SSH key + named secrets) for app builds.
//
// Conductor stores per-app build auth as Kubernetes Secrets in the app
// namespace, deliberately separate from the app's runtime secrets:
//
//   - `<osid>-build-ssh` (key `ssh-privatekey`): one SSH private key,
//     forwarded as BuildKit SSH id `default` (`--mount=type=ssh`).
//   - `<osid>-build-secrets`: one data key per BuildKit secret id
//     (`--mount=type=secret,id=<id>`).
//
// Both are CLIENT-side BuildKit session features: buildctl (this process)
// reads the material and streams it to the daemon per-instruction; nothing
// lands in the image, the registry cache, or the build pod spec. The Secret
// names are the contract with conductor (applications/buildAuth.ts); there
// is no wire-protocol change.
const (
	buildSSHSecretSuffix     = "-build-ssh"
	buildSSHSecretKey        = "ssh-privatekey"
	buildSecretsSecretSuffix = "-build-secrets"
)

// buildAuth is the per-build auth material staged on disk for buildctl.
type buildAuth struct {
	// sshKeyPath is the staged private key file ("" = no key set).
	sshKeyPath string
	// secretPaths maps BuildKit secret id -> staged file path.
	secretPaths map[string]string
	// dir is the staging tempdir (0700, files 0600), removed by cleanup.
	dir string
}

// readBuildAuth fetches the app's build auth Secrets and stages them in a
// private tempdir. Missing Secrets (or a missing namespace, e.g. the
// app-less build-image path whose OSID has no namespace) mean "no auth",
// not an error. The returned cleanup must always be called.
func readBuildAuth(ctx context.Context, clientset kubernetes.Interface, osid string) (buildAuth, func(), error) {
	auth := buildAuth{secretPaths: map[string]string{}}
	cleanup := func() {
		if auth.dir != "" {
			if err := os.RemoveAll(auth.dir); err != nil {
				log.Printf("buildkitclient: failed to remove build-auth dir: %v", err)
			}
		}
	}
	if osid == "" {
		return auth, cleanup, nil
	}

	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	secrets := clientset.CoreV1().Secrets(osid)

	sshSecret, err := secrets.Get(rctx, osid+buildSSHSecretSuffix, metav1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return auth, cleanup, fmt.Errorf("failed to read build SSH secret: %w", err)
	}
	buildSecrets, err := secrets.Get(rctx, osid+buildSecretsSecretSuffix, metav1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return auth, cleanup, fmt.Errorf("failed to read build secrets: %w", err)
	}

	sshKey := []byte(nil)
	if sshSecret != nil {
		sshKey = sshSecret.Data[buildSSHSecretKey]
	}
	if len(sshKey) == 0 && (buildSecrets == nil || len(buildSecrets.Data) == 0) {
		return auth, cleanup, nil
	}

	dir, err := os.MkdirTemp("", "build-auth-*")
	if err != nil {
		return auth, cleanup, fmt.Errorf("failed to create build-auth dir: %w", err)
	}
	auth.dir = dir
	if err := os.Chmod(dir, 0o700); err != nil {
		return auth, cleanup, fmt.Errorf("failed to restrict build-auth dir: %w", err)
	}

	if len(sshKey) > 0 {
		path := filepath.Join(dir, "ssh-key")
		// BuildKit requires the key to end with a newline; tolerate keys
		// stored without one.
		if sshKey[len(sshKey)-1] != '\n' {
			sshKey = append(sshKey, '\n')
		}
		if err := os.WriteFile(path, sshKey, 0o600); err != nil {
			return auth, cleanup, fmt.Errorf("failed to stage build SSH key: %w", err)
		}
		auth.sshKeyPath = path
	}

	if buildSecrets != nil {
		for id, value := range buildSecrets.Data {
			// Ids are validated conductor-side (alphanumeric + [._-], no
			// separators), so they are safe as file names; guard anyway.
			if id == "" || id != filepath.Base(id) {
				log.Printf("buildkitclient: skipping build secret with unsafe id %q", id)
				continue
			}
			path := filepath.Join(dir, "secret-"+id)
			if err := os.WriteFile(path, value, 0o600); err != nil {
				return auth, cleanup, fmt.Errorf("failed to stage build secret %q: %w", id, err)
			}
			auth.secretPaths[id] = path
		}
	}

	return auth, cleanup, nil
}

// buildAuthArgs renders the staged auth as buildctl flags, deterministically
// ordered. Pure, unit-tested.
func buildAuthArgs(auth buildAuth) []string {
	var args []string
	if auth.sshKeyPath != "" {
		args = append(args, "--ssh", "default="+auth.sshKeyPath)
	}
	ids := make([]string, 0, len(auth.secretPaths))
	for id := range auth.secretPaths {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		args = append(args, "--secret", fmt.Sprintf("id=%s,src=%s", id, auth.secretPaths[id]))
	}
	return args
}

// describeBuildAuth summarizes what is being forwarded for the build log
// (ids only, never values). Empty string when nothing is forwarded.
func describeBuildAuth(auth buildAuth) string {
	switch {
	case auth.sshKeyPath != "" && len(auth.secretPaths) > 0:
		return fmt.Sprintf("Forwarding build SSH key (id default) and %d build secret(s)", len(auth.secretPaths))
	case auth.sshKeyPath != "":
		return "Forwarding build SSH key (id default)"
	case len(auth.secretPaths) > 0:
		return fmt.Sprintf("Forwarding %d build secret(s)", len(auth.secretPaths))
	default:
		return ""
	}
}
