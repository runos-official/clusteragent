package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustWriteEnvTest writes content at path, creating parent dirs.
func mustWriteEnvTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestResolveManifestEnvVars_AnchorsToConfigDir pins the core fix for bug
// #102: on a VCS deploy the cluster agent must resolve `env:` relative to the
// MANIFEST's own directory, not the repo clone-root. A monorepo app whose
// runos.yaml lives in a subdir references a sibling env file by bare name; the
// pre-fix code looked it up at <clone-root>/<name>, found nothing, and shipped
// EMPTY env (dropping ALLOWED_CIDRS, silently disabling the in-app allowlist).
//
// A decoy file with the same name at the clone-root proves the anchoring: if
// resolution regressed to clone-root we'd pick up the decoy's value.
func TestResolveManifestEnvVars_AnchorsToConfigDir(t *testing.T) {
	workdir := t.TempDir()
	configDir := filepath.Join(workdir, "apps", "x", "infra")
	yamlPath := filepath.Join(configDir, "runos.yaml")

	mustWriteEnvTest(t, yamlPath, "app: demo\nport: 8080\nenv: app.config.env\n")
	// The correct sibling, next to the manifest.
	mustWriteEnvTest(t, filepath.Join(configDir, "app.config.env"),
		"ALLOWED_CIDRS=10.0.0.0/8\nVA64_MARKER=present\n")
	// Decoy at the clone-root: must NOT be the one we read.
	mustWriteEnvTest(t, filepath.Join(workdir, "app.config.env"),
		"ALLOWED_CIDRS=0.0.0.0/0\n")

	plain, secret, err := resolveManifestEnvVars(yamlPath, workdir, configDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := plain["ALLOWED_CIDRS"]; got != "10.0.0.0/8" {
		t.Errorf("ALLOWED_CIDRS = %q, want sibling value 10.0.0.0/8 (resolved against clone-root decoy?)", got)
	}
	if got := plain["VA64_MARKER"]; got != "present" {
		t.Errorf("VA64_MARKER = %q, want present", got)
	}
	if secret != nil {
		t.Errorf("secret = %v, want nil (no secretEnv referenced)", secret)
	}
}

// TestResolveManifestEnvVars_MissingPlainFailsLoud pins defect (b): an
// explicit `env:` reference to a file that isn't committed must ERROR, not
// ship empty env. The error names the file and warns about dropped config so
// the operator sees the real cause instead of an app that came up with an
// empty allowlist.
func TestResolveManifestEnvVars_MissingPlainFailsLoud(t *testing.T) {
	workdir := t.TempDir()
	configDir := filepath.Join(workdir, "svc")
	yamlPath := filepath.Join(configDir, "runos.yaml")
	mustWriteEnvTest(t, yamlPath, "app: demo\nport: 8080\nenv: does-not-exist.config.env\n")

	_, _, err := resolveManifestEnvVars(yamlPath, workdir, configDir)
	if err == nil {
		t.Fatal("expected fail-loud error for missing committed plain env file, got nil")
	}
	if !strings.Contains(err.Error(), "does-not-exist.config.env") {
		t.Errorf("error should name the missing file, got: %v", err)
	}
}

// TestResolveManifestEnvVars_MissingSecretIsTolerated pins the gitignored-
// secret nuance: the secret-env file is conventionally NOT committed, so its
// absence on a VCS checkout is expected (secrets come from server state). A
// missing `secretEnv:` reference must NOT fail the deploy.
func TestResolveManifestEnvVars_MissingSecretIsTolerated(t *testing.T) {
	workdir := t.TempDir()
	configDir := filepath.Join(workdir, "svc")
	yamlPath := filepath.Join(configDir, "runos.yaml")
	// Plain env present (so the deploy is otherwise valid), secret ref points
	// at a gitignored file that isn't in the checkout.
	mustWriteEnvTest(t, yamlPath, "app: demo\nport: 8080\nenv: app.config.env\nsecretEnv: .runos.cid.id.env\n")
	mustWriteEnvTest(t, filepath.Join(configDir, "app.config.env"), "FOO=bar\n")

	plain, secret, err := resolveManifestEnvVars(yamlPath, workdir, configDir)
	if err != nil {
		t.Fatalf("missing gitignored secret file must not error, got: %v", err)
	}
	if plain["FOO"] != "bar" {
		t.Errorf("plain FOO = %q, want bar", plain["FOO"])
	}
	if secret != nil {
		t.Errorf("secret = %v, want nil (gitignored file absent)", secret)
	}
}

// TestResolveManifestEnvVars_SecretPresentIsShipped: if a secret-env file IS
// present in the checkout (someone committed it), resolve + ship it so it
// isn't silently dropped.
func TestResolveManifestEnvVars_SecretPresentIsShipped(t *testing.T) {
	workdir := t.TempDir()
	configDir := filepath.Join(workdir, "svc")
	yamlPath := filepath.Join(configDir, "runos.yaml")
	mustWriteEnvTest(t, yamlPath, "app: demo\nport: 8080\nsecretEnv: committed.secret.env\n")
	mustWriteEnvTest(t, filepath.Join(configDir, "committed.secret.env"), "TOKEN=s3cr3t\n")

	plain, secret, err := resolveManifestEnvVars(yamlPath, workdir, configDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plain != nil {
		t.Errorf("plain = %v, want nil (no env: referenced)", plain)
	}
	if secret["TOKEN"] != "s3cr3t" {
		t.Errorf("secret TOKEN = %q, want s3cr3t", secret["TOKEN"])
	}
}

// TestResolveManifestEnvVars_NoRefs: a manifest with neither env: nor
// secretEnv: yields two nil maps and no error.
func TestResolveManifestEnvVars_NoRefs(t *testing.T) {
	workdir := t.TempDir()
	configDir := workdir
	yamlPath := filepath.Join(configDir, "runos.yaml")
	mustWriteEnvTest(t, yamlPath, "app: demo\nport: 8080\n")

	plain, secret, err := resolveManifestEnvVars(yamlPath, workdir, configDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plain != nil || secret != nil {
		t.Errorf("plain=%v secret=%v, want both nil", plain, secret)
	}
}

// TestResolveManifestEnvVars_TraversalRejected: an env ref that escapes the
// clone (../../etc/...) is rejected by the same workdir-confinement guard
// that protects configPath / sourceDir, so a malicious manifest can't read
// worker-host files into the app's env.
func TestResolveManifestEnvVars_TraversalRejected(t *testing.T) {
	workdir := t.TempDir()
	configDir := filepath.Join(workdir, "svc")
	yamlPath := filepath.Join(configDir, "runos.yaml")
	mustWriteEnvTest(t, yamlPath, "app: demo\nport: 8080\nenv: ../../../../etc/passwd\n")

	_, _, err := resolveManifestEnvVars(yamlPath, workdir, configDir)
	if err == nil {
		t.Fatal("expected rejection of env ref escaping the workdir, got nil")
	}
	if !strings.Contains(err.Error(), "escapes workdir") {
		t.Errorf("error should report workdir escape, got: %v", err)
	}
}

// TestResolveManifestEnvVars_ControlCharFailsLoud: a committed env value with
// a control byte kubectl can't YAML-marshal must fail at fetch time, not
// mid-orchestration.
func TestResolveManifestEnvVars_ControlCharFailsLoud(t *testing.T) {
	workdir := t.TempDir()
	configDir := filepath.Join(workdir, "svc")
	yamlPath := filepath.Join(configDir, "runos.yaml")
	mustWriteEnvTest(t, yamlPath, "app: demo\nport: 8080\nenv: app.config.env\n")
	mustWriteEnvTest(t, filepath.Join(configDir, "app.config.env"), "BAD=\x01value\n")

	_, _, err := resolveManifestEnvVars(yamlPath, workdir, configDir)
	if err == nil {
		t.Fatal("expected control-char rejection, got nil")
	}
	if !strings.Contains(err.Error(), "control byte") {
		t.Errorf("error should name the control byte, got: %v", err)
	}
}

// TestParseDotenv pins the ported parser against the shapes the CLI's
// internal/envfile.Parse accepts, so a committed .config.env is interpreted
// identically on the VCS path and the CLI path.
func TestParseDotenv(t *testing.T) {
	in := strings.Join([]string{
		"# a comment",
		"",
		"PLAIN=value",
		"  SPACED  =  trimmed  ",
		`QUOTED="has \"quote\" and \n newline"`,
		"SINGLE='no $escapes here'",
		"noequals line is skipped",
		"EMPTY=",
	}, "\n")

	got := parseDotenv([]byte(in))

	want := map[string]string{
		"PLAIN":  "value",
		"SPACED": "trimmed",
		"QUOTED": "has \"quote\" and \n newline",
		"SINGLE": "no $escapes here",
		"EMPTY":  "",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d keys, want %d: %#v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["noequals line is skipped"]; ok {
		t.Error("malformed line should have been skipped")
	}
}
