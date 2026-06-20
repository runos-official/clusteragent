package datastore

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// setupTestDB points the package-level db at a fresh temp SQLite file and runs
// the same schema + migrations Initialize() would, without touching the
// hardcoded /data path. Restores the previous handle on cleanup.
func setupTestDB(t *testing.T) {
	t.Helper()
	prev := db
	t.Cleanup(func() {
		if db != nil {
			db.Close()
		}
		db = prev
	})

	var err error
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := createTables(); err != nil {
		t.Fatalf("createTables: %v", err)
	}
}

func TestInsertBuildKitJobArgs_RoundTrip(t *testing.T) {
	setupTestDB(t)

	const jobID = "sha123-abcd1234"
	if err := CreateBuildKitJob(jobID, "osid-1", "repo", "main", "sha123"); err != nil {
		t.Fatalf("CreateBuildKitJob: %v", err)
	}

	args := []BuildArg{
		{Key: "NEXT_PUBLIC_API_PORT", Value: "443", Source: "yaml"},
		{Key: "NEXT_PUBLIC_APP_VERSION", Value: "1.2.3", Source: "cli"},
	}
	if err := InsertBuildKitJobArgs(jobID, args); err != nil {
		t.Fatalf("InsertBuildKitJobArgs: %v", err)
	}

	got := queryJobArgs(t, jobID)
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}
	// Insertion order is preserved by the autoincrement id ORDER BY in the helper.
	want := map[string]BuildArg{
		"NEXT_PUBLIC_API_PORT":    {Key: "NEXT_PUBLIC_API_PORT", Value: "443", Source: "yaml"},
		"NEXT_PUBLIC_APP_VERSION": {Key: "NEXT_PUBLIC_APP_VERSION", Value: "1.2.3", Source: "cli"},
	}
	for _, g := range got {
		w, ok := want[g.Key]
		if !ok {
			t.Fatalf("unexpected arg key %q", g.Key)
		}
		if g != w {
			t.Fatalf("arg %q = %+v, want %+v", g.Key, g, w)
		}
	}
}

func TestInsertBuildKitJobArgs_EmptyIsNoOp(t *testing.T) {
	setupTestDB(t)

	const jobID = "sha456-empty000"
	if err := CreateBuildKitJob(jobID, "osid-1", "repo", "main", "sha456"); err != nil {
		t.Fatalf("CreateBuildKitJob: %v", err)
	}

	if err := InsertBuildKitJobArgs(jobID, nil); err != nil {
		t.Fatalf("InsertBuildKitJobArgs(nil): %v", err)
	}
	if err := InsertBuildKitJobArgs(jobID, []BuildArg{}); err != nil {
		t.Fatalf("InsertBuildKitJobArgs([]): %v", err)
	}

	if rows := queryJobArgs(t, jobID); len(rows) != 0 {
		t.Fatalf("expected 0 rows for empty args, got %d", len(rows))
	}
}

func TestUploadTokenBuildArgs_RoundTrip(t *testing.T) {
	setupTestDB(t)

	args := []BuildArg{
		{Key: "NODE_ENV", Value: "production", Source: "yaml"},
		{Key: "NEXT_PUBLIC_APP_VERSION", Value: "9.9.9", Source: "cli"},
	}
	expires := time.Now().Add(5 * time.Minute)
	if err := CreateUploadToken("tok-with-args", "osid-1", "upload-1", "apps/web/Dockerfile", args, expires); err != nil {
		t.Fatalf("CreateUploadToken: %v", err)
	}

	tok, err := GetUploadToken("tok-with-args")
	if err != nil {
		t.Fatalf("GetUploadToken: %v", err)
	}
	if len(tok.BuildArgs) != 2 {
		t.Fatalf("expected 2 build args on token, got %d", len(tok.BuildArgs))
	}
	if tok.BuildArgs[0] != args[0] || tok.BuildArgs[1] != args[1] {
		t.Fatalf("build args round-trip mismatch: got %+v want %+v", tok.BuildArgs, args)
	}
}

func TestUploadTokenBuildArgs_EmptyDefault(t *testing.T) {
	setupTestDB(t)

	expires := time.Now().Add(5 * time.Minute)
	if err := CreateUploadToken("tok-no-args", "osid-1", "upload-2", "", nil, expires); err != nil {
		t.Fatalf("CreateUploadToken: %v", err)
	}

	tok, err := GetUploadToken("tok-no-args")
	if err != nil {
		t.Fatalf("GetUploadToken: %v", err)
	}
	if len(tok.BuildArgs) != 0 {
		t.Fatalf("expected no build args, got %+v", tok.BuildArgs)
	}
}

// queryJobArgs reads the buildkit_job_args rows for a job ordered by insertion.
func queryJobArgs(t *testing.T, jobID string) []BuildArg {
	t.Helper()
	rows, err := db.Query(`SELECT arg_key, arg_value, source FROM buildkit_job_args WHERE build_job_id = ? ORDER BY id ASC`, jobID)
	if err != nil {
		t.Fatalf("query job args: %v", err)
	}
	defer rows.Close()

	var out []BuildArg
	for rows.Next() {
		var a BuildArg
		if err := rows.Scan(&a.Key, &a.Value, &a.Source); err != nil {
			t.Fatalf("scan job arg: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	return out
}
