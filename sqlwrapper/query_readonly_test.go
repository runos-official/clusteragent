package sqlwrapper

import "testing"

// TestRejectNonReadUnderReadOnly pins the hard read-only gate: on a read-only
// connection (readWrite=false) any non-read statement is refused before
// execution — including the keyword-classifier bypasses (leading comment or
// whitespace, SET, DDL) that would otherwise reach the write path on MySQL —
// while reads pass; on a read-write connection nothing is refused. A regression
// here lets a write execute against a tenant DB on a read-only connection.
func TestRejectNonReadUnderReadOnly(t *testing.T) {
	cases := []struct {
		name      string
		readWrite bool
		query     string
		want      bool // true = must be rejected
	}{
		{"ro select allowed", false, "SELECT 1", false},
		{"ro with-cte allowed", false, "WITH t AS (SELECT 1) SELECT * FROM t", false},
		{"ro delete rejected", false, "DELETE FROM users", true},
		{"ro update rejected", false, "UPDATE users SET x = 1", true},
		{"ro insert rejected", false, "INSERT INTO t VALUES (1)", true},
		{"ro comment-prefixed delete rejected", false, "/* x */ DELETE FROM t", true},
		{"ro whitespace-prefixed delete rejected", false, "   DELETE FROM t", true},
		{"ro SET rejected", false, "SET search_path = evil", true},
		{"ro DDL rejected", false, "ALTER TABLE t ADD c int", true},
		{"rw delete allowed", true, "DELETE FROM users", false},
		{"rw select allowed", true, "SELECT 1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rejectNonReadUnderReadOnly(c.readWrite, c.query); got != c.want {
				t.Errorf("rejectNonReadUnderReadOnly(%v, %q) = %v, want %v", c.readWrite, c.query, got, c.want)
			}
		})
	}
}
