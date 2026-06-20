package instructions

import "testing"

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
