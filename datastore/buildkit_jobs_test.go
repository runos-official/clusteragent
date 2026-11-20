package datastore

import (
	"testing"
	"time"
)

// TestCreateBuildKitJob_UpsertReopensAFailedRow pins review finding 1133:
// a terminal pre-claim refusal now records a failed build row under the
// token's uploadID (job_id) BEFORE the token is claimed, so a legitimate
// retry with the same token reuses the same job_id. job_id carries a
// unique index; a plain INSERT would fail that retry's own
// CreateBuildKitJob call with a duplicate-key error. The upsert must
// instead reset the row to a fresh pending job: status back to pending,
// completed_at cleared, created_at preserved (the row's original
// first-seen time), and the retry's build must then be able to complete
// normally through the ordinary success path.
func TestCreateBuildKitJob_UpsertReopensAFailedRow(t *testing.T) {
	setupTestDB(t)

	const jobID = "upload-reused-1"
	if err := CreateBuildKitJob(jobID, "osid-1", "repo-a", "", jobID); err != nil {
		t.Fatalf("first CreateBuildKitJob: %v", err)
	}
	first, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob after first create: %v", err)
	}
	if err := InsertBuildKitLog(jobID, "ERROR: tarball refused"); err != nil {
		t.Fatalf("InsertBuildKitLog: %v", err)
	}
	if err := UpdateBuildKitJobStatus(jobID, JobStatusFailed); err != nil {
		t.Fatalf("UpdateBuildKitJobStatus(failed): %v", err)
	}
	failed, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob after failing: %v", err)
	}
	if failed.Status != JobStatusFailed || failed.CompletedAt == nil {
		t.Fatalf("job = %+v, want status failed with completed_at set", failed)
	}

	// The retry: same token, same uploadID, so the same job_id. This must
	// NOT return a duplicate-key error.
	if err := CreateBuildKitJob(jobID, "osid-1", "repo-a", "", jobID); err != nil {
		t.Fatalf("retry CreateBuildKitJob must upsert, not error: %v", err)
	}
	reopened, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob after retry: %v", err)
	}
	if reopened.Status != JobStatusPending {
		t.Errorf("status after retry = %q, want pending", reopened.Status)
	}
	if reopened.CompletedAt != nil {
		t.Errorf("completed_at after retry = %v, want nil", reopened.CompletedAt)
	}
	if !reopened.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created_at after retry = %v, want unchanged from the first attempt %v", reopened.CreatedAt, first.CreatedAt)
	}
	if reopened.UpdatedAt.Before(failed.UpdatedAt) {
		t.Errorf("updated_at after retry = %v, want not before the failed row's %v", reopened.UpdatedAt, failed.UpdatedAt)
	}

	// The reopened row must behave exactly like a fresh job for the
	// ordinary success path that follows a real build.
	if err := UpdateBuildKitJobStatus(jobID, JobStatusSuccess); err != nil {
		t.Fatalf("UpdateBuildKitJobStatus(success) on the reopened row: %v", err)
	}
	succeeded, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob after success: %v", err)
	}
	if succeeded.Status != JobStatusSuccess {
		t.Errorf("final status = %q, want success", succeeded.Status)
	}
}

// TestCreateBuildKitJob_FirstCallIsOrdinary proves a brand-new job_id still
// creates a normal pending row (a legitimate first attempt must still
// pass).
func TestCreateBuildKitJob_FirstCallIsOrdinary(t *testing.T) {
	setupTestDB(t)

	if err := CreateBuildKitJob("job-fresh", "osid-1", "repo-a", "branch-a", "sha-a"); err != nil {
		t.Fatalf("CreateBuildKitJob: %v", err)
	}
	job, err := GetBuildKitJob("job-fresh")
	if err != nil {
		t.Fatalf("GetBuildKitJob: %v", err)
	}
	if job.Status != JobStatusPending || job.CompletedAt != nil {
		t.Fatalf("job = %+v, want a fresh pending row", job)
	}
}

// TestRecordFailedUploadIfUnclaimed_SkipsWhenAlreadyClaimed pins review
// finding 1267: a terminal pre-claim refusal for a token a CONCURRENT,
// faster request already claimed must not touch that request's build row.
// Without this check, the unconditional upsert resets a winner's
// completed, successful row back to pending and then to failed - Conductor
// could then report a failed deployment that actually succeeded.
func TestRecordFailedUploadIfUnclaimed_SkipsWhenAlreadyClaimed(t *testing.T) {
	setupTestDB(t)

	const raw = "token-1267-claimed-by-winner"
	const jobID = "upload-1267-claimed"
	if err := CreatePullToken(raw, "osid-1", jobID, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("CreatePullToken: %v", err)
	}
	// The winner's claim and successful completion, exactly as the real
	// handler and async build path would leave it.
	if err := MarkUploadTokenUsed(raw, nil); err != nil {
		t.Fatalf("MarkUploadTokenUsed (the winner's claim): %v", err)
	}
	if err := CreateBuildKitJob(jobID, "osid-1", "", "", jobID); err != nil {
		t.Fatalf("CreateBuildKitJob (the winner's row): %v", err)
	}
	if err := UpdateBuildKitJobStatus(jobID, JobStatusSuccess); err != nil {
		t.Fatalf("UpdateBuildKitJobStatus(success): %v", err)
	}
	winnerRow, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob after the winner's success: %v", err)
	}

	// The loser's terminal refusal arrives after the winner already
	// claimed and finished. It must change nothing.
	if err := RecordFailedUploadIfUnclaimed(raw, jobID, "osid-1", "loser's refusal, arrived late"); err != nil {
		t.Fatalf("RecordFailedUploadIfUnclaimed must not error when the token is already claimed: %v", err)
	}

	after, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob after the late refusal: %v", err)
	}
	if after.Status != JobStatusSuccess {
		t.Fatalf("status after the late refusal = %q, want unchanged success", after.Status)
	}
	if after.CompletedAt == nil || !after.CompletedAt.Equal(*winnerRow.CompletedAt) {
		t.Fatalf("completed_at after the late refusal = %v, want unchanged from %v", after.CompletedAt, winnerRow.CompletedAt)
	}
}

// TestRecordFailedUploadIfUnclaimed_WritesWhenStillUnclaimed proves the
// legitimate case still works: no concurrent winner exists, so the
// terminal refusal's pollable row must still be written (review finding
// 1133's requirement, which this function must preserve).
func TestRecordFailedUploadIfUnclaimed_WritesWhenStillUnclaimed(t *testing.T) {
	setupTestDB(t)

	const raw = "token-1267-never-claimed"
	const jobID = "upload-1267-unclaimed"
	if err := CreatePullToken(raw, "osid-1", jobID, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("CreatePullToken: %v", err)
	}

	if err := RecordFailedUploadIfUnclaimed(raw, jobID, "osid-1", "tarball refused"); err != nil {
		t.Fatalf("RecordFailedUploadIfUnclaimed: %v", err)
	}

	job, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob: %v", err)
	}
	if job.Status != JobStatusFailed || job.CompletedAt == nil {
		t.Fatalf("job = %+v, want a recorded failure", job)
	}
	logs, err := QueryBuildKitLogs(QueryBuildKitLogsOptions{JobID: jobID})
	if err != nil {
		t.Fatalf("QueryBuildKitLogs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("expected at least one recorded log line")
	}

	// A legitimate retry with the SAME token and uploadID must still be
	// able to claim and build (review finding 1133's upsert-reopens
	// guarantee, unaffected by this change).
	if err := MarkUploadTokenUsed(raw, nil); err != nil {
		t.Fatalf("a later legitimate claim of the same token must still succeed: %v", err)
	}
	if err := CreateBuildKitJob(jobID, "osid-1", "", "", jobID); err != nil {
		t.Fatalf("the retry's CreateBuildKitJob must still reopen the row: %v", err)
	}
	reopened, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob after the retry: %v", err)
	}
	if reopened.Status != JobStatusPending {
		t.Fatalf("status after the retry = %q, want pending", reopened.Status)
	}
}
