package sqlwrapper

import (
	"strings"
	"testing"
)

// TestBuildDSN_Postgres pins the postgres DSN: sslmode=disable, a default
// dbname of "postgres" when DatabaseName is empty, the supplied dbname
// otherwise, and (crucially) that the password is pq-escaped so a password
// containing a quote or space cannot inject extra connection parameters.
func TestBuildDSN_Postgres(t *testing.T) {
	t.Run("with database name", func(t *testing.T) {
		dsn, driver, err := buildDSN(ConnectionParams{
			DatabaseType: "postgres",
			Username:     "alice",
			Password:     "s3cret",
			Host:         "db.internal",
			Port:         5432,
			DatabaseName: "appdb",
		})
		if err != nil {
			t.Fatalf("buildDSN: %v", err)
		}
		if driver != "postgres" {
			t.Errorf("driver = %q, want postgres", driver)
		}
		for _, want := range []string{"host='db.internal'", "port=5432", "user='alice'", "password='s3cret'", "dbname='appdb'", "sslmode=disable"} {
			if !strings.Contains(dsn, want) {
				t.Errorf("dsn %q missing %q", dsn, want)
			}
		}
	})

	t.Run("empty database name defaults to postgres", func(t *testing.T) {
		dsn, _, err := buildDSN(ConnectionParams{
			DatabaseType: "postgres",
			Username:     "alice",
			Password:     "s3cret",
			Host:         "db.internal",
			Port:         5432,
		})
		if err != nil {
			t.Fatalf("buildDSN: %v", err)
		}
		if !strings.Contains(dsn, "dbname='postgres'") {
			t.Errorf("dsn %q should default dbname to postgres", dsn)
		}
	})

	t.Run("password with quote and space is escaped", func(t *testing.T) {
		// A password that, if interpolated raw, would inject a second
		// `sslmode=require` parameter and tamper with the connection.
		dsn, _, err := buildDSN(ConnectionParams{
			DatabaseType: "postgres",
			Username:     "alice",
			Password:     `p' sslmode=require '`,
			Host:         "db.internal",
			Port:         5432,
			DatabaseName: "appdb",
		})
		if err != nil {
			t.Fatalf("buildDSN: %v", err)
		}
		// The whole password (including the injected sslmode=require) must sit
		// inside one single-quoted, backslash-escaped token; the embedded
		// quotes are escaped as \'. lib/pq then reads this as one password
		// value rather than parsing the injected key=value pair.
		if !strings.Contains(dsn, `password='p\' sslmode=require \''`) {
			t.Errorf("password not pq-escaped in dsn: %q", dsn)
		}
		// The injected sslmode=require must NOT appear as a bare, unquoted
		// top-level token (i.e. immediately followed by our own dbname token,
		// which is what would happen with raw interpolation). Pin that the
		// only sslmode token outside the quoted password is our sslmode=disable.
		if strings.Contains(dsn, `sslmode=require dbname=`) {
			t.Errorf("password injection produced a bare top-level sslmode=require token: %q", dsn)
		}
		// sslmode=disable is still set by us.
		if !strings.Contains(dsn, "sslmode=disable") {
			t.Errorf("dsn %q missing sslmode=disable", dsn)
		}
	})

	t.Run("backslash in password is escaped", func(t *testing.T) {
		dsn, _, err := buildDSN(ConnectionParams{
			DatabaseType: "postgres",
			Username:     "alice",
			Password:     `a\b`,
			Host:         "db.internal",
			Port:         5432,
			DatabaseName: "appdb",
		})
		if err != nil {
			t.Fatalf("buildDSN: %v", err)
		}
		if !strings.Contains(dsn, `password='a\\b'`) {
			t.Errorf("backslash not escaped in dsn: %q", dsn)
		}
	})
}

// TestBuildDSN_MySQL pins the mysql DSN shape produced by the driver's config
// formatter: user:pass@tcp(host:port)/dbname.
func TestBuildDSN_MySQL(t *testing.T) {
	dsn, driver, err := buildDSN(ConnectionParams{
		DatabaseType: "mysql",
		Username:     "alice",
		Password:     "s3cret",
		Host:         "db.internal",
		Port:         3306,
		DatabaseName: "appdb",
	})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	if driver != "mysql" {
		t.Errorf("driver = %q, want mysql", driver)
	}
	const want = "alice:s3cret@tcp(db.internal:3306)/appdb"
	if dsn != want {
		t.Errorf("mysql dsn = %q, want %q", dsn, want)
	}
}

// TestBuildDSN_UnsupportedType pins that an unknown database type is rejected.
func TestBuildDSN_UnsupportedType(t *testing.T) {
	_, _, err := buildDSN(ConnectionParams{DatabaseType: "oracle"})
	if err == nil {
		t.Fatal("expected an error for unsupported database type, got nil")
	}
}
