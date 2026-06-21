package sqlwrapper

import "testing"

// TestIsReadStatement pins the read-only enforcement gate. isReadStatement
// decides whether ExecuteQuery routes a statement through QueryContext (rows,
// read) or ExecContext (write). Mis-classifying a write as a read would scan
// it for rows instead of executing it, and the inverse would discard a result
// set, so the keyword set and its first-word extraction are load-bearing.
//
// Classification keys on the FIRST whitespace-delimited token, upper-cased.
// The empty/whitespace-only query is treated as a read (returns true) so it
// is sent to the no-op read path rather than the write path.
func TestIsReadStatement(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  bool
	}{
		// Read statements.
		{"SELECT", "SELECT * FROM users", true},
		{"WITH (CTE)", "WITH t AS (SELECT 1) SELECT * FROM t", true},
		{"SHOW", "SHOW TABLES", true},
		{"DESCRIBE", "DESCRIBE users", true},
		{"DESC", "DESC users", true},
		{"EXPLAIN", "EXPLAIN SELECT 1", true},
		{"lowercase select", "select 1", true},
		{"mixed case select", "SeLeCt 1", true},
		{"leading whitespace", "   \n\t SELECT 1", true},
		{"empty string", "", true},
		{"whitespace only", "   \n\t ", true},

		// Write / non-read statements.
		{"INSERT", "INSERT INTO users (id) VALUES (1)", false},
		{"UPDATE", "UPDATE users SET x = 1", false},
		{"DELETE", "DELETE FROM users", false},
		{"lowercase insert", "insert into users values (1)", false},
		{"CREATE", "CREATE TABLE t (id int)", false},
		{"DROP", "DROP TABLE t", false},
		{"TRUNCATE", "TRUNCATE users", false},
		{"GRANT", "GRANT SELECT ON t TO u", false},
		// A leading SQL comment is NOT a read keyword: the first token is the
		// comment marker, so the gate routes it to the write path. Pinned so a
		// future change that tries to be clever about comments is a conscious
		// decision, not an accident.
		{"leading line comment", "-- read please\nSELECT 1", false},
		{"leading block comment", "/* hi */ SELECT 1", false},
		// A keyword that is only a prefix of a read keyword must not match.
		{"SELECTED is not SELECT", "SELECTED FROM nope", false},
		// Whitespace before a write keyword must NOT make it look like a read:
		// TrimSpace strips it, the first token is DELETE, so it routes to write.
		// Guards a regression where a leading newline/tab smuggles a write past
		// the gate.
		{"whitespace before DELETE", "  \n\t DELETE FROM users", false},
		// A leading block comment before a write keyword: the first token is the
		// comment marker, not a read keyword, so it routes to write. Pinned so a
		// comment-stripping "improvement" can't accidentally make `/* x */ DELETE`
		// classify as read-only.
		{"block comment then DELETE", "/* x */ DELETE FROM users", false},
		// SET (session/config mutation) and DDL are NOT read keywords, so they
		// route to the write/exec path. Pinned because misclassifying SET as a
		// read would scan it for rows instead of executing it, and a read-only
		// caller must never have a SET silently treated as a benign read.
		{"SET is not a read", "SET default_transaction_read_only = off", false},
		{"ALTER (DDL) is not a read", "ALTER TABLE t ADD COLUMN c int", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadStatement(tc.query); got != tc.want {
				t.Errorf("isReadStatement(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestIsReadStatement_WritingCTEIsKnownLimitation pins the documented blind spot
// of the first-keyword classifier: a CTE that writes
// (`WITH t AS (DELETE ... RETURNING *) SELECT ...`) starts with the read keyword
// WITH, so isReadStatement classifies it as a READ. This is the data-safety
// boundary the robustness audit flagged.
//
// This is NOT asserting the misclassification is safe — it is pinning the
// keyword-only contract so a change here is a CONSCIOUS decision, and recording
// why it doesn't open a hole in production: for Postgres the authoritative
// read-only gate is the server-side `default_transaction_read_only = on` session
// setting ExecuteQuery applies (see query.go), which rejects the embedded DELETE
// at the database regardless of how isReadStatement routes it. The classifier is
// the primary gate only for MySQL, where the contract is that a readWrite=false
// caller sends read statements; a writing CTE from such a caller is a contract
// violation, not something isReadStatement is expected to catch by parsing SQL.
//
// If someone later teaches isReadStatement to actually parse CTE bodies, this
// test will fail and force them to update the MySQL routing deliberately rather
// than silently shifting the boundary.
func TestIsReadStatement_WritingCTEIsKnownLimitation(t *testing.T) {
	const writingCTE = "WITH t AS (DELETE FROM users RETURNING *) SELECT * FROM t"
	if got := isReadStatement(writingCTE); got != true {
		t.Fatalf("isReadStatement(%q) = %v; the keyword-only classifier treats a "+
			"writing CTE as a read (true). If this changed, update the MySQL "+
			"read-only routing in query.go deliberately and revise this test.",
			writingCTE, got)
	}
}
