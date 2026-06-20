package buildkitclient

import (
	"strings"
	"testing"
)

// TestSanitizeK8sName pins the DNS-1123 character mapping used by both pod
// names and label values: lowercase a-z / 0-9 / '-' pass through, A-Z is
// lowercased, every other rune becomes '-'. Length and trailing-dash trimming
// are the callers' responsibility (see builderPodName / labelValue), so they
// are asserted there, not here.
func TestSanitizeK8sName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already safe", "buildkit-build-123", "buildkit-build-123"},
		{"uppercase lowercased", "MyApp-V1", "myapp-v1"},
		{"underscores and dots", "job_id.v2", "job-id-v2"},
		{"slashes and spaces", "a/b c", "a-b-c"},
		// Iterates by rune: é is one rune -> one '-', each CJK char one '-'.
		{"unicode replaced", "café-日本", "caf----"},
		{"empty", "", ""},
		{"digits preserved", "0123456789", "0123456789"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeK8sName(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeK8sName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Every output rune must be in the DNS-1123 subset.
			for _, r := range got {
				ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
				if !ok {
					t.Errorf("sanitizeK8sName(%q) produced disallowed rune %q", tc.in, string(r))
				}
			}
		})
	}
}

// TestLabelValue pins the K8s label-value derivation: sanitized, capped at 63
// chars, and trimmed of leading/trailing '-' (a label value must start and end
// with an alphanumeric). The 63-char cap must apply BEFORE the trim so a cap
// that lands on a '-' does not leave a trailing dash.
func TestLabelValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"lowercased", "Main", "main"},
		{"leading dash trimmed", "-abc", "abc"},
		{"trailing dash trimmed", "abc-", "abc"},
		{"unsafe wrapped then trimmed", "_x_", "x"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := labelValue(tc.in); got != tc.want {
				t.Errorf("labelValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// A value whose sanitized form exceeds 63 chars is capped to 63 then
	// trimmed. Build an input that lands a '-' exactly at index 62 so the cap
	// would leave a trailing dash if the trim ran first.
	in := strings.Repeat("a", 62) + "-bbbb"
	got := labelValue(in)
	if len(got) > 63 {
		t.Errorf("labelValue length %d exceeds 63", len(got))
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("labelValue(%q) = %q has a trailing '-'", in, got)
	}
	if got != strings.Repeat("a", 62) {
		t.Errorf("labelValue(%q) = %q, want the 62 'a's with the dash-at-cap trimmed", in, got)
	}
}

// TestCacheRefForPushRef_NoSlash pins the degenerate input where the push ref
// has no '/' separator: the last ':' is still treated as the tag separator
// (LastIndex("/")==-1, so any ':' qualifies), and the cache ref carries the
// fixed buildcache tag. Complements ephemeral_test.go's host:port cases.
func TestCacheRefForPushRef_NoSlash(t *testing.T) {
	cases := map[string]string{
		"app:tag":      "app:buildcache",
		"bareimage":    "bareimage:buildcache",
		"app:v1:weird": "app:v1:buildcache",
	}
	for in, want := range cases {
		if got := cacheRefForPushRef(in); got != want {
			t.Errorf("cacheRefForPushRef(%q) = %q, want %q", in, got, want)
		}
	}
}
