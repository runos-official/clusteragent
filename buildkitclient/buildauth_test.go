package buildkitclient

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestBuildAuthArgs pins the buildctl flag rendering: --ssh default=<key>
// first, then --secret per id in sorted (deterministic) order.
func TestBuildAuthArgs(t *testing.T) {
	if got := buildAuthArgs(buildAuth{}); len(got) != 0 {
		t.Errorf("empty auth: got %v, want none", got)
	}

	auth := buildAuth{
		sshKeyPath:  "/tmp/x/ssh-key",
		secretPaths: map[string]string{"npm-token": "/tmp/x/secret-npm-token", "a-token": "/tmp/x/secret-a-token"},
	}
	got := strings.Join(buildAuthArgs(auth), " ")
	want := "--ssh default=/tmp/x/ssh-key --secret id=a-token,src=/tmp/x/secret-a-token --secret id=npm-token,src=/tmp/x/secret-npm-token"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestDescribeBuildAuth pins the build-log summary: ids/counts only, never
// values or paths.
func TestDescribeBuildAuth(t *testing.T) {
	if got := describeBuildAuth(buildAuth{}); got != "" {
		t.Errorf("empty: got %q", got)
	}
	if got := describeBuildAuth(buildAuth{sshKeyPath: "/k"}); !strings.Contains(got, "SSH key") {
		t.Errorf("ssh only: got %q", got)
	}
	got := describeBuildAuth(buildAuth{sshKeyPath: "/k", secretPaths: map[string]string{"a": "1", "b": "2"}})
	if !strings.Contains(got, "SSH key") || !strings.Contains(got, "2 build secret") {
		t.Errorf("both: got %q", got)
	}
}

// TestReadBuildAuth pins the Secret discovery against a fake clientset:
// missing Secrets/namespace mean no auth (not an error); present Secrets are
// staged to 0600 files with a trailing newline appended to the SSH key.
func TestReadBuildAuth(t *testing.T) {
	ctx := context.Background()

	// Nothing set (and effectively a missing namespace): no auth, no error.
	auth, cleanup, err := readBuildAuth(ctx, fake.NewSimpleClientset(), "app-ab12c")
	if err != nil {
		t.Fatalf("missing secrets: unexpected error %v", err)
	}
	cleanup()
	if auth.sshKeyPath != "" || len(auth.secretPaths) != 0 {
		t.Errorf("missing secrets: got %+v, want empty", auth)
	}

	// App-less build (no osid namespace semantics): skipped entirely.
	if auth, cleanup, err := readBuildAuth(ctx, fake.NewSimpleClientset(), ""); err != nil || auth.sshKeyPath != "" {
		t.Errorf("empty osid: got %+v err=%v", auth, err)
	} else {
		cleanup()
	}

	// Both set: staged and rendered.
	clientset := fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "app-ab12c-build-ssh", Namespace: "app-ab12c"},
			Data:       map[string][]byte{"ssh-privatekey": []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "app-ab12c-build-secrets", Namespace: "app-ab12c"},
			Data:       map[string][]byte{"npm-token": []byte("tok_123"), "../evil": []byte("x")},
		},
	)
	auth, cleanup, err = readBuildAuth(ctx, clientset, "app-ab12c")
	defer cleanup()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth.sshKeyPath == "" {
		t.Fatal("ssh key not staged")
	}
	if _, ok := auth.secretPaths["npm-token"]; !ok {
		t.Error("npm-token not staged")
	}
	if _, ok := auth.secretPaths["../evil"]; ok {
		t.Error("unsafe id must be skipped")
	}

	args := strings.Join(buildAuthArgs(auth), " ")
	if !strings.Contains(args, "--ssh default=") || !strings.Contains(args, "id=npm-token,src=") {
		t.Errorf("args missing expected flags: %q", args)
	}
}
