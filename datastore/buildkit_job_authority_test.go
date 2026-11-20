package datastore

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// TestBuildKitJob_MigratesOldRowsToExplicitLegacy proves an old build-job row
// (predating the authorization_classification/authority_job_id columns)
// migrates to an explicit "legacy" classification with no authority job id,
// mirroring TestUploadToken_MigratesOldRowsToExplicitLegacy's discipline for
// the sibling upload-token table (datastore/scoped_upload_tokens_test.go).
func TestBuildKitJob_MigratesOldRowsToExplicitLegacy(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-buildkit-jobs.db")
	gdb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		NamingStrategy: schema.NamingStrategy{TablePrefix: tablePrefix},
	})
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if err := gdb.Exec(`CREATE TABLE cluster_agent_buildkit_jobs (
		id integer PRIMARY KEY AUTOINCREMENT,
		job_id text NOT NULL UNIQUE,
		osid text NOT NULL,
		repo text NOT NULL,
		branch text NOT NULL,
		commit_hash text NOT NULL,
		status text NOT NULL,
		created_at datetime DEFAULT CURRENT_TIMESTAMP,
		updated_at datetime DEFAULT CURRENT_TIMESTAMP,
		completed_at datetime
	)`).Error; err != nil {
		t.Fatalf("create old table: %v", err)
	}
	if err := gdb.Exec(`INSERT INTO cluster_agent_buildkit_jobs
		(job_id, osid, repo, branch, commit_hash, status) VALUES (?, ?, ?, ?, ?, ?)`,
		"old-job-1", "osid-1", "", "", "old-job-1", JobStatusSuccess).Error; err != nil {
		t.Fatalf("insert old row: %v", err)
	}

	if err := gdb.AutoMigrate(&BuildKitJobModel{}); err != nil {
		t.Fatalf("migrate old table: %v", err)
	}

	prev := setHandle(gdb)
	t.Cleanup(func() { setHandle(prev) })

	job, err := GetBuildKitJob("old-job-1")
	if err != nil {
		t.Fatalf("GetBuildKitJob: %v", err)
	}
	if job.AuthorizationClassification != AuthorizationLegacy {
		t.Fatalf("migrated classification = %q, want explicit legacy", job.AuthorizationClassification)
	}
	if job.AuthorityJobID != "" {
		t.Fatalf("migrated authority job id = %q, want empty", job.AuthorityJobID)
	}
}

// TestCreateScopedBuildKitJob_RoundTrip proves a scoped build job persists
// its explicit classification and authority job id, and that they survive a
// status update unchanged (plan: "Keep the binding immutable after job
// creation. Status and completion updates must not replace or clear the
// binding.").
func TestCreateScopedBuildKitJob_RoundTrip(t *testing.T) {
	setupTestDB(t)

	const jobID = "scoped-job-1"
	const authorityJobID = "11111111-1111-1111-1111-111111111111"
	if err := CreateScopedBuildKitJob(jobID, authorityJobID, "osid-1", "repo-a", "main", "sha-a"); err != nil {
		t.Fatalf("CreateScopedBuildKitJob: %v", err)
	}
	job, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob: %v", err)
	}
	if job.AuthorizationClassification != AuthorizationScoped {
		t.Fatalf("classification = %q, want scoped", job.AuthorizationClassification)
	}
	if job.AuthorityJobID != authorityJobID {
		t.Fatalf("authority job id = %q, want %q", job.AuthorityJobID, authorityJobID)
	}

	if err := UpdateBuildKitJobStatus(jobID, JobStatusSuccess); err != nil {
		t.Fatalf("UpdateBuildKitJobStatus: %v", err)
	}
	after, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob after status update: %v", err)
	}
	if after.AuthorizationClassification != AuthorizationScoped || after.AuthorityJobID != authorityJobID {
		t.Fatalf("binding changed after status update: classification=%q authorityJobID=%q", after.AuthorizationClassification, after.AuthorityJobID)
	}
}

// TestCreateScopedBuildKitJob_RejectsInvalidAuthorityJobID proves a
// malformed (non-UUID) authority job id is refused rather than silently
// stored: the recheck client (agentstream.JobAuthority) requires a
// canonical lowercase UUID, so an invalid id stored here would fail every
// later stage recheck with an opaque transport error instead of a clear
// refusal at creation time.
func TestCreateScopedBuildKitJob_RejectsInvalidAuthorityJobID(t *testing.T) {
	setupTestDB(t)

	if err := CreateScopedBuildKitJob("scoped-job-2", "not-a-uuid", "osid-1", "", "", "sha-a"); err == nil {
		t.Fatal("expected an error for a malformed authority job id")
	}
	if _, err := GetBuildKitJob("scoped-job-2"); err == nil {
		t.Fatal("expected no row to be created for a refused authority job id")
	}
}

// TestCreateScopedBuildKitJob_UpsertPreservesOriginalBinding proves a retry
// through the SAME job_id (the CLI upsert-reopens path, review finding
// 1133) never changes the row's classification or authority job id: the
// binding is immutable once the row first exists, even across a reopen.
func TestCreateScopedBuildKitJob_UpsertPreservesOriginalBinding(t *testing.T) {
	setupTestDB(t)

	const jobID = "scoped-job-3"
	const firstAuthority = "22222222-2222-2222-2222-222222222222"
	if err := CreateScopedBuildKitJob(jobID, firstAuthority, "osid-1", "", "", jobID); err != nil {
		t.Fatalf("first CreateScopedBuildKitJob: %v", err)
	}
	if err := UpdateBuildKitJobStatus(jobID, JobStatusFailed); err != nil {
		t.Fatalf("UpdateBuildKitJobStatus(failed): %v", err)
	}

	// A legacy re-creation call (e.g. a caller that forgot the binding) must
	// not silently downgrade or overwrite the stored scoped attribution.
	if err := CreateBuildKitJob(jobID, "osid-1", "", "", jobID); err != nil {
		t.Fatalf("retry CreateBuildKitJob: %v", err)
	}
	reopened, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob after retry: %v", err)
	}
	if reopened.AuthorizationClassification != AuthorizationScoped || reopened.AuthorityJobID != firstAuthority {
		t.Fatalf("binding after reopen = (%q, %q), want unchanged (%q, %q)",
			reopened.AuthorizationClassification, reopened.AuthorityJobID, AuthorizationScoped, firstAuthority)
	}
	if reopened.Status != JobStatusPending {
		t.Fatalf("status after reopen = %q, want pending", reopened.Status)
	}
}

// TestFailNonTerminalBuildKitJobs_PreservesBinding proves restart cleanup
// (plan: "Restart cleanup changes only status, completion time, and the
// failure log") never clears the scoped attribution it needs to preserve
// for audit.
func TestFailNonTerminalBuildKitJobs_PreservesBinding(t *testing.T) {
	setupTestDB(t)

	const jobID = "scoped-job-restart"
	const authorityJobID = "33333333-3333-3333-3333-333333333333"
	if err := CreateScopedBuildKitJob(jobID, authorityJobID, "osid-1", "", "", jobID); err != nil {
		t.Fatalf("CreateScopedBuildKitJob: %v", err)
	}

	closed, err := FailNonTerminalBuildKitJobs("agent restart")
	if err != nil {
		t.Fatalf("FailNonTerminalBuildKitJobs: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	job, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob: %v", err)
	}
	if job.Status != JobStatusFailed {
		t.Fatalf("status = %q, want failed", job.Status)
	}
	if job.AuthorizationClassification != AuthorizationScoped || job.AuthorityJobID != authorityJobID {
		t.Fatalf("binding after restart cleanup = (%q, %q), want unchanged (%q, %q)",
			job.AuthorizationClassification, job.AuthorityJobID, AuthorizationScoped, authorityJobID)
	}
}
