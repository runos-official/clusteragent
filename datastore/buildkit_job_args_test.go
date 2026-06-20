package datastore

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// setupTestDB installs a fresh temp SQLite database (pure-Go glebarez driver, no
// CGO) as the active handle DIRECTLY via setHandle, bypassing the reconcile
// loop, the pointer ConfigMap, and Kubernetes entirely so the query-function
// tests run with no Postgres and no cluster. AutoMigrate uses the SAME
// NamingStrategy (cluster_agent_ prefix) Initialize()'s loop uses, so the schema
// under test matches production. Restores the previous handle on cleanup.
//
// testDB returns the live handle so tests that need to read raw rows can do so.
func setupTestDB(t *testing.T) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	gdb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
		NamingStrategy: schema.NamingStrategy{
			TablePrefix: tablePrefix,
		},
	})
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	if err := gdb.AutoMigrate(allModels()...); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	prev := setHandle(gdb)
	t.Cleanup(func() {
		if cur := setHandle(prev); cur != nil {
			if sqlDB, err := cur.DB(); err == nil {
				sqlDB.Close()
			}
		}
	})
}

// testDB returns the active handle for tests that read raw model rows directly.
// It fails the test if no handle is installed (setupTestDB must run first).
func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := activeDB()
	if err != nil {
		t.Fatalf("no active test DB: %v", err)
	}
	return gdb
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

// queryJobArgs reads the buildkit_job_args rows for a job ordered by insertion
// (the autoincrement id), straight from the model so the test exercises the
// real persisted shape.
func queryJobArgs(t *testing.T, jobID string) []BuildArg {
	t.Helper()
	var rows []BuildKitJobArgModel
	if err := testDB(t).Where("build_job_id = ?", jobID).Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("query job args: %v", err)
	}
	out := make([]BuildArg, 0, len(rows))
	for _, r := range rows {
		out = append(out, BuildArg{Key: r.ArgKey, Value: r.ArgValue, Source: r.Source})
	}
	return out
}
