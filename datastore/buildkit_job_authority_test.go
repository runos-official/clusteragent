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
// through the SAME job_id, with the SAME attribution (the CLI
// upsert-reopens path, review finding 1133), succeeds and leaves the
// binding unchanged: the binding is immutable once the row first exists,
// even across a reopen.
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

	// A legitimate retry with the SAME scoped attribution reopens cleanly.
	if err := CreateScopedBuildKitJob(jobID, firstAuthority, "osid-1", "", "", jobID); err != nil {
		t.Fatalf("retry CreateScopedBuildKitJob with the same attribution: %v", err)
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

// TestCreateBuildKitJob_RefusesConflictingReopen proves review finding
// 1401's fix: a caller that reopens the SAME job_id with DIFFERENT
// attribution (here, a legacy re-creation call reopening a row that was
// first created scoped) is refused outright - the row's real scoped
// attribution is never silently discarded, and a build never runs under
// attribution it was not actually granted.
func TestCreateBuildKitJob_RefusesConflictingReopen(t *testing.T) {
	setupTestDB(t)

	const jobID = "scoped-job-conflict"
	const firstAuthority = "33333333-3333-3333-3333-333333333333"
	if err := CreateScopedBuildKitJob(jobID, firstAuthority, "osid-1", "", "", jobID); err != nil {
		t.Fatalf("first CreateScopedBuildKitJob: %v", err)
	}
	if err := UpdateBuildKitJobStatus(jobID, JobStatusFailed); err != nil {
		t.Fatalf("UpdateBuildKitJobStatus(failed): %v", err)
	}

	if err := CreateBuildKitJob(jobID, "osid-1", "", "", jobID); err == nil {
		t.Fatal("expected a conflict error reopening a scoped row as legacy")
	}

	unchanged, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob after the refused reopen: %v", err)
	}
	if unchanged.AuthorizationClassification != AuthorizationScoped || unchanged.AuthorityJobID != firstAuthority {
		t.Fatalf("binding after refused reopen = (%q, %q), want unchanged (%q, %q)",
			unchanged.AuthorizationClassification, unchanged.AuthorityJobID, AuthorizationScoped, firstAuthority)
	}
	if unchanged.Status != JobStatusFailed {
		t.Fatalf("status after refused reopen = %q, want unchanged failed", unchanged.Status)
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

// TestRecordFailedUploadIfUnclaimed_ThenCorrectedScopedRetrySucceeds proves
// review finding 1401's real scenario end to end: a pre-claim refusal for
// a SCOPED token (the tarball/Dockerfile checks, before the token is
// claimed) must record the token's real attribution, not a hardcoded
// legacy default - otherwise a later corrected retry that claims the same
// token and reaches CreateScopedBuildKitJob under the same job_id (the
// uploadID) would be refused by the reopen conflict check, and the
// legitimate scoped build could never run.
func TestRecordFailedUploadIfUnclaimed_ThenCorrectedScopedRetrySucceeds(t *testing.T) {
	setupTestDB(t)

	const raw = "token-1401-scoped-retry"
	const jobID = "upload-1401-scoped"
	const authorityJobID = "44444444-4444-4444-4444-444444444444"
	if err := CreatePullToken(raw, "osid-1", jobID, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("CreatePullToken: %v", err)
	}

	// The first attempt's pre-claim refusal (e.g. an oversized tarball)
	// carries the token's REAL scoped attribution, not a hardcoded legacy
	// default.
	if err := RecordFailedUploadIfUnclaimed(raw, jobID, "osid-1", "tarball refused", AuthorizationScoped, authorityJobID); err != nil {
		t.Fatalf("RecordFailedUploadIfUnclaimed: %v", err)
	}
	afterRefusal, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob after refusal: %v", err)
	}
	if afterRefusal.AuthorizationClassification != AuthorizationScoped || afterRefusal.AuthorityJobID != authorityJobID {
		t.Fatalf("refusal row attribution = (%q, %q), want (%q, %q)",
			afterRefusal.AuthorizationClassification, afterRefusal.AuthorityJobID, AuthorizationScoped, authorityJobID)
	}

	// The token is still unused (pre-claim refusal never claims it), so a
	// corrected retry can claim it and reach the real admission path.
	if err := MarkUploadTokenUsed(raw, nil); err != nil {
		t.Fatalf("a corrected retry's claim must still succeed: %v", err)
	}
	if err := CreateScopedBuildKitJob(jobID, authorityJobID, "osid-1", "", "", jobID); err != nil {
		t.Fatalf("the corrected retry's CreateScopedBuildKitJob must succeed, not conflict: %v", err)
	}
	reopened, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob after the corrected retry: %v", err)
	}
	if reopened.Status != JobStatusPending {
		t.Fatalf("status after the corrected retry = %q, want pending", reopened.Status)
	}
	if reopened.AuthorizationClassification != AuthorizationScoped || reopened.AuthorityJobID != authorityJobID {
		t.Fatalf("binding after the corrected retry = (%q, %q), want (%q, %q)",
			reopened.AuthorizationClassification, reopened.AuthorityJobID, AuthorizationScoped, authorityJobID)
	}
}
