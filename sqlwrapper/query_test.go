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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadStatement(tc.query); got != tc.want {
				t.Errorf("isReadStatement(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}
