package sqlwrapper

import (
	"regexp"
	"testing"
)

// baseParams is a fully populated ConnectionParams used as the mutation base
// for the per-field hash-sensitivity cases.
func baseParams() ConnectionParams {
	return ConnectionParams{
		Username:     "alice",
		Password:     "s3cret",
		Host:         "db.internal",
		Port:         5432,
		DatabaseType: "postgres",
		DatabaseName: "appdb",
	}
}

var hexHash64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TestConnectionHash_Stable pins that identical params hash to the same value
// and that the output is always 64 lowercase hex chars (a SHA-256 digest).
func TestConnectionHash_Stable(t *testing.T) {
	a := ConnectionHash(baseParams())
	b := ConnectionHash(baseParams())
	if a != b {
		t.Fatalf("identical params hashed differently: %q vs %q", a, b)
	}
	if !hexHash64.MatchString(a) {
		t.Errorf("hash %q is not 64 lowercase hex chars", a)
	}
}

// TestConnectionHash_FieldSensitivity pins that flipping each of the six fields
// independently changes the hash. If any field were dropped from the digest
// input, two distinct targets could collide on one pool/cache entry.
func TestConnectionHash_FieldSensitivity(t *testing.T) {
	base := baseParams()
	baseHash := ConnectionHash(base)

	cases := []struct {
		name   string
		mutate func(*ConnectionParams)
	}{
		{"Username", func(p *ConnectionParams) { p.Username = "bob" }},
		{"Password", func(p *ConnectionParams) { p.Password = "other" }},
		{"Host", func(p *ConnectionParams) { p.Host = "db2.internal" }},
		{"Port", func(p *ConnectionParams) { p.Port = 5433 }},
		{"DatabaseType", func(p *ConnectionParams) { p.DatabaseType = "mysql" }},
		{"DatabaseName", func(p *ConnectionParams) { p.DatabaseName = "otherdb" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			got := ConnectionHash(p)
			if got == baseHash {
				t.Errorf("changing %s did not change the hash (%q)", tc.name, got)
			}
			if !hexHash64.MatchString(got) {
				t.Errorf("hash %q is not 64 lowercase hex chars", got)
			}
		})
	}
}
