package webhook

import (
	"strings"
	"testing"
)

// TestGenerateOSID pins the OSID shape contract that the rest of the platform
// relies on: a stable `app-` prefix, a fixed total length, letter-bounded ends
// (so the id is a valid DNS-1123 label start/end and never collides with the
// numeric-suffix conventions), all lowercase, and a restricted charset. The
// generator is randomized, so the invariants are asserted across many samples.
func TestGenerateOSID(t *testing.T) {
	const iterations = 1000
	const prefix = "app-"
	// prefix(4) + first letter(1) + middle(3) + last letter(1) = 9.
	const wantLen = 9

	seen := make(map[string]int)
	for i := 0; i < iterations; i++ {
		osid, err := GenerateOSID()
		if err != nil {
			t.Fatalf("iteration %d: GenerateOSID returned error: %v", i, err)
		}
		seen[osid]++

		if !strings.HasPrefix(osid, prefix) {
			t.Fatalf("osid %q missing %q prefix", osid, prefix)
		}
		if len(osid) != wantLen {
			t.Fatalf("osid %q length %d, want %d", osid, len(osid), wantLen)
		}
		if osid != strings.ToLower(osid) {
			t.Fatalf("osid %q is not all lowercase", osid)
		}

		body := strings.TrimPrefix(osid, prefix) // 5 chars: L + AAA + L
		first := body[0]
		last := body[len(body)-1]
		if !isLetter(first) {
			t.Fatalf("osid %q first body char %q is not a letter", osid, string(first))
		}
		if !isLetter(last) {
			t.Fatalf("osid %q last body char %q is not a letter", osid, string(last))
		}
		for j := 0; j < len(body); j++ {
			c := body[j]
			if !isLowerAlphaNum(c) {
				t.Fatalf("osid %q char %q outside [a-z0-9]", osid, string(c))
			}
		}
	}

	// Not an entropy proof, but with a 26 * 36^3 * 26 space, 1000 draws
	// collapsing to a handful of values would indicate a broken generator.
	if len(seen) < iterations/2 {
		t.Errorf("only %d distinct OSIDs across %d draws; generator may not be random", len(seen), iterations)
	}
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' }

func isLowerAlphaNum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// TestRandomCharInCharset pins that randomChar only ever returns bytes drawn
// from the supplied charset.
func TestRandomCharInCharset(t *testing.T) {
	const charset = "abc123"
	for i := 0; i < 500; i++ {
		c, err := randomChar(charset)
		if err != nil {
			t.Fatalf("randomChar: %v", err)
		}
		if !strings.ContainsRune(charset, rune(c)) {
			t.Fatalf("randomChar returned %q, not in %q", string(c), charset)
		}
	}
}
