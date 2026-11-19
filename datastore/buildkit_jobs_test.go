package datastore

import (
	"testing"
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
