package instructions

import (
	"path/filepath"
	"testing"
)

// TestValidateGitSHA pins the SHA allowlist guarding the positional git args:
// only a plain 7-64 char hex object id is accepted; anything option-like,
// dash-leading, non-hex, or out-of-length is rejected so it can never reach
// `git fetch`/`git checkout` as a flag.
func TestValidateGitSHA(t *testing.T) {
	cases := []struct {
		name string
		sha  string
		ok   bool
	}{
		// Accepted: valid hex of allowed lengths.
		{"7 hex (min abbrev)", "abc1234", true},
		{"40 hex (full sha1)", "0123456789abcdef0123456789abcdef01234567", true},
		{"64 hex (full sha256)", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", true},
		{"uppercase hex", "ABCDEF0", true},
		{"mixed case hex", "AbCdEf0123", true},

		// Rejected: too short / too long.
		{"6 hex too short", "abc123", false},
		{"65 hex too long", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0", false},
		{"empty", "", false},

		// Rejected: non-hex / option-like / path-like.
		{"non-hex letters", "ghijklm", false},
		{"leading dash", "-abc1234", false},
		{"double dash option", "--upload-pack=/x", false},
		{"upload-pack injection", "--upload-pack=touch /tmp/pwn", false},
		{"path-like", "refs/heads/main", false},
		{"contains space", "abc1234 def5678", false},
		{"branch name", "main", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGitSHA(tc.sha)
			if tc.ok && err != nil {
				t.Errorf("validateGitSHA(%q) = %v, want accept", tc.sha, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("validateGitSHA(%q) = nil, want reject", tc.sha)
			}
		})
	}
}

// TestResolveInsideWorkdir pins the path-traversal guard that confines every
// caller-supplied repo path (configPath, sourceDir, dockerfile) to the cloned
// workdir. A regression here lets a malicious runos.yaml read or build against
// files OUTSIDE the clone (e.g. ../../etc on the worker), so both the accept and
// the reject paths are load-bearing.
//
// Behaviour pinned (matches resolveInsideWorkdir in vcs_fetch_source.go):
//   - absolute paths are rejected outright ("absolute paths not allowed")
//   - any cleaned result that escapes the workdir tree is rejected
//     ("path escapes workdir"), including `..`-walks that re-enter via a
//     sibling sharing the workdir's name prefix
//   - legitimate relative paths (incl. ones with interior `..` that stay inside,
//     and "."/trailing-slash forms) resolve to the cleaned absolute path
func TestResolveInsideWorkdir(t *testing.T) {
	workdir := filepath.Join(string(filepath.Separator)+"tmp", "vcs-work")

	cases := []struct {
		name    string
		rel     string
		wantErr string // "" means accept
		want    string // expected resolved path when accepted
	}{
		// Accepted: legitimate repo-relative paths.
		{"root yaml", "runos.yaml", "", filepath.Join(workdir, "runos.yaml")},
		{"monorepo subdir", "apps/api/runos.yaml", "", filepath.Join(workdir, "apps/api/runos.yaml")},
		{"dot is workdir itself", ".", "", workdir},
		{"trailing slash dir", "sub/", "", filepath.Join(workdir, "sub")},
		// Interior `..` that stays inside the tree is fine: a/../b == b.
		{"interior dotdot stays inside", "a/../b.yaml", "", filepath.Join(workdir, "b.yaml")},

		// Rejected: absolute paths.
		{"absolute path", string(filepath.Separator) + "etc/passwd", "absolute paths not allowed", ""},

		// Rejected: escapes via `..`.
		{"leading dotdot", "../escape.yaml", "path escapes workdir", ""},
		{"bare dotdot", "..", "path escapes workdir", ""},
		{"dotdot past root", "a/../../escape", "path escapes workdir", ""},
		{"deep dotdot escape", "../../../etc/passwd", "path escapes workdir", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveInsideWorkdir(workdir, tc.rel)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveInsideWorkdir(%q,%q) = %q, want error %q", workdir, tc.rel, got, tc.wantErr)
				}
				if err.Error() != tc.wantErr {
					t.Fatalf("resolveInsideWorkdir(%q,%q) error = %q, want %q", workdir, tc.rel, err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveInsideWorkdir(%q,%q) unexpected error: %v", workdir, tc.rel, err)
			}
			if got != tc.want {
				t.Errorf("resolveInsideWorkdir(%q,%q) = %q, want %q", workdir, tc.rel, got, tc.want)
			}
		})
	}
}

// TestResolveInsideWorkdir_SiblingPrefixEscape pins the trailing-separator
// containment check specifically: a sibling directory whose name SHARES the
// workdir's prefix (e.g. /tmp/vcs-work-evil vs workdir /tmp/vcs-work) must be
// rejected. A naive strings.HasPrefix(workdir) check would wrongly accept it;
// the guard requires the separator boundary. This is the same class of bug the
// tarball symlink guard defends against, here on the VCS source path.
func TestResolveInsideWorkdir_SiblingPrefixEscape(t *testing.T) {
	workdir := filepath.Join(string(filepath.Separator)+"tmp", "vcs-work")
	// `../vcs-work-evil/secret` resolves to /tmp/vcs-work-evil/secret, which
	// shares the "vcs-work" prefix but is a sibling of the workdir.
	if _, err := resolveInsideWorkdir(workdir, "../vcs-work-evil/secret"); err == nil {
		t.Fatal("sibling-prefix path escaping the workdir should be rejected, got accept")
	}
}
