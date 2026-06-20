package instructions

import (
	"strings"
	"testing"

	"github.com/runos-official/clusteragent/sqlwrapper"
)

// validConn returns a fully valid postgres connection used as the mutation
// base for the required-field error cases.
func validConn() sqlwrapper.ConnectionParams {
	return sqlwrapper.ConnectionParams{
		Username:     "alice",
		Password:     "s3cret",
		Host:         "db.internal",
		Port:         5432,
		DatabaseType: "postgres",
		DatabaseName: "appdb",
	}
}

// TestValidateConnectionParams pins one error per required field, the unknown
// database-type rejection, and the two valid happy paths (postgres, mysql).
func TestValidateConnectionParams(t *testing.T) {
	cases := []struct {
		name          string
		mutate        func(*sqlwrapper.ConnectionParams)
		wantErrSubstr string // "" means expect no error
	}{
		{"empty username", func(c *sqlwrapper.ConnectionParams) { c.Username = "" }, "username is required"},
		{"empty password", func(c *sqlwrapper.ConnectionParams) { c.Password = "" }, "password is required"},
		{"empty host", func(c *sqlwrapper.ConnectionParams) { c.Host = "" }, "host is required"},
		{"zero port", func(c *sqlwrapper.ConnectionParams) { c.Port = 0 }, "port is required"},
		{"unknown database type", func(c *sqlwrapper.ConnectionParams) { c.DatabaseType = "oracle" }, "databaseType must be"},
		{"valid postgres", func(c *sqlwrapper.ConnectionParams) {}, ""},
		{"valid mysql", func(c *sqlwrapper.ConnectionParams) { c.DatabaseType = "mysql" }, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConn()
			tc.mutate(&c)
			err := validateConnectionParams(c)
			if tc.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrSubstr)
			}
		})
	}
}
