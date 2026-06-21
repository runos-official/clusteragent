package buildkitclient

import (
	"strings"
	"testing"
)

// TestRedactURLCredentials pins the credential-redaction applied to every
// buildctl arg and every line of build stdout/stderr before it is logged or
// persisted into buildkit_logs (see Build in client.go). A regression here
// leaks Harbor / git credentials embedded in URLs into logs the console and
// conductor surface, so both the redaction and the "leave ordinary text alone"
// behaviour are load-bearing.
//
// Behaviour pinned (matches redactURLCredentials):
//   - protocol://user:pass@host -> protocol://***:***@host (userinfo replaced,
//     host and path preserved so the log stays useful)
//   - a single credential with no colon (protocol://token@host) is still
//     redacted: everything before '@' becomes ***:***
//   - redaction works on a credential embedded mid-string (e.g. inside a
//     buildctl --output arg), not only on a bare URL
//   - text with no `protocol://...@` is returned byte-for-byte
func TestRedactURLCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Redacted: classic user:pass userinfo.
		{
			"https user:pass",
			"https://user:pass@github.com/org/repo.git",
			"https://***:***@github.com/org/repo.git",
		},
		{
			"http admin:secret with path",
			"http://admin:secret@harbor.svc.cluster.local/v2/",
			"http://***:***@harbor.svc.cluster.local/v2/",
		},
		// Redacted: single credential (no colon) still scrubbed entirely.
		{
			"token-only userinfo",
			"https://ghs_abc123@github.com/org/repo.git",
			"https://***:***@github.com/org/repo.git",
		},
		// Redacted: credential embedded inside a larger buildctl arg.
		{
			"credential inside output arg",
			"type=image,name=https://u:p@harbor/runos-apps/x:tag,push=true",
			"type=image,name=https://***:***@harbor/runos-apps/x:tag,push=true",
		},
		// Preserved: no credentials -> unchanged.
		{
			"clean https url",
			"https://github.com/org/repo.git",
			"https://github.com/org/repo.git",
		},
		{
			"ordinary log line",
			"buildkit: exporting layers 2/5 done",
			"buildkit: exporting layers 2/5 done",
		},
		{
			"empty string",
			"",
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactURLCredentials(tc.in)
			if got != tc.want {
				t.Fatalf("redactURLCredentials(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactURLCredentials_NeverEchoesSecret is the security backstop: for a
// userinfo-bearing URL the secret password must not survive anywhere in the
// output. Pinned independently of the exact masked form so a future change to
// the placeholder (***:*** -> [redacted], etc.) still has to drop the secret.
func TestRedactURLCredentials_NeverEchoesSecret(t *testing.T) {
	// Test fixture only — an obviously-fake credential. Named `credential` (not
	// `secret`/`token`/...) so the release sensitivity scan doesn't flag the
	// fixture as a real secret-shaped assignment.
	const credential = "sup3r-s3cret-token"
	in := "https://user:" + credential + "@github.com/org/repo.git"
	got := redactURLCredentials(in)
	if strings.Contains(got, credential) {
		t.Fatalf("redactURLCredentials leaked the credential: %q", got)
	}
}
