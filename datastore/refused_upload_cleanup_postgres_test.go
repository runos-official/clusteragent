package datastore

import (
	"os"
	"testing"
	"time"
)

// TestRecordFailedUploadIfUnclaimed_PostgresSurvivesExpiredTokenCleanup pins
// review finding 1314 (repeat of 1267): recordFailedUploadIfUnclaimedPostgres
// scanned the token's used column into a plain bool. Once
// DeleteExpiredUploadTokens removes the token row (it deletes used and
// unused rows alike, keyed only on expiry), the SELECT returns zero rows,
// the scanned bool keeps its zero value false, and the refusal wrote
// anyway - overwriting a winner's completed build row with a bogus
// failure, even though the winner claimed and finished long before the
// refusal was ever decided.
//
// This reproduces the real sequence: the winner claims and completes while
// the token still exists, the token then expires and cleanup deletes it,
// and only afterwards does the slow refusal (modelling a request that
// loaded the token early but took a long time on tarball/Dockerfile/Harbor
// checks) finally call RecordFailedUploadIfUnclaimed. No lock contention
// is needed to reproduce this: the two calls never overlap in time, which
// is exactly why checking "used" instead of "row found" is wrong - there
// is no row left to be used or unused.
//
// Needs a real PostgreSQL connection (see
// TestMarkUploadTokenUsed_PostgresClockTimestampSurvivesLockWait's doc for
// how to run it); skipped unless CLUSTERAGENT_TEST_POSTGRES_DSN is set.
func TestRecordFailedUploadIfUnclaimed_PostgresSurvivesExpiredTokenCleanup(t *testing.T) {
	dsn := os.Getenv("CLUSTERAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CLUSTERAGENT_TEST_POSTGRES_DSN not set; skipping the PostgreSQL-backed regression for review finding 1314")
	}
	setupPostgresTestDB(t, dsn)

	const raw = "pg-1314-winner-token"
	const jobID = "upload-1314-winner"

	// The token expires almost immediately: short enough that the test can
	// wait it out, long enough that the winner's claim+build below (which
	// happens first, while the token is still live) succeeds normally.
	shortExpiry := time.Now().Add(300 * time.Millisecond)
	if err := CreatePullToken(raw, "osid-1", jobID, shortExpiry); err != nil {
		t.Fatalf("CreatePullToken: %v", err)
	}

	// The winner: claims the token and completes its build while the token
	// row still exists, exactly as the real handler and async build path
	// would.
	if err := MarkUploadTokenUsed(raw, nil); err != nil {
		t.Fatalf("MarkUploadTokenUsed (the winner's claim): %v", err)
	}
	if err := CreateBuildKitJob(jobID, "osid-1", "", "", jobID); err != nil {
		t.Fatalf("CreateBuildKitJob (the winner's row): %v", err)
	}
	if err := InsertBuildKitLog(jobID, "build succeeded"); err != nil {
		t.Fatalf("InsertBuildKitLog: %v", err)
	}
	if err := UpdateBuildKitJobStatus(jobID, JobStatusSuccess); err != nil {
		t.Fatalf("UpdateBuildKitJobStatus(success): %v", err)
	}
	winnerRow, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob after the winner's success: %v", err)
	}
	if winnerRow.Status != JobStatusSuccess || winnerRow.CompletedAt == nil {
		t.Fatalf("winner row = %+v, want a completed success before cleanup runs", winnerRow)
	}
	winnerLogs, err := QueryBuildKitLogs(QueryBuildKitLogsOptions{JobID: jobID})
	if err != nil {
		t.Fatalf("QueryBuildKitLogs (winner, before cleanup): %v", err)
	}

	// Let the token actually expire, then run the exact cleanup that
	// deletes it - any row past its deadline, used or not.
	time.Sleep(400 * time.Millisecond)
	if err := DeleteExpiredUploadTokens(); err != nil {
		t.Fatalf("DeleteExpiredUploadTokens: %v", err)
	}
	if _, err := GetUploadableToken(raw); err == nil {
		t.Fatal("the expired token row must be gone after cleanup, so the test actually exercises the zero-rows case")
	}

	// The slow refusal is only decided now, strictly after the winner
	// finished and strictly after cleanup removed the token row.
	if err := RecordFailedUploadIfUnclaimed(raw, jobID, "osid-1", "slow refusal, decided after cleanup", AuthorizationLegacy, ""); err != nil {
		t.Fatalf("RecordFailedUploadIfUnclaimed must not error once the token row is gone: %v", err)
	}

	after, err := GetBuildKitJob(jobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob after the late refusal: %v", err)
	}
	if after.Status != JobStatusSuccess {
		t.Fatalf("status after the late refusal = %q, want unchanged success (finding 1314)", after.Status)
	}
	if after.CompletedAt == nil || !after.CompletedAt.Equal(*winnerRow.CompletedAt) {
		t.Fatalf("completed_at after the late refusal = %v, want unchanged from %v", after.CompletedAt, winnerRow.CompletedAt)
	}
	afterLogs, err := QueryBuildKitLogs(QueryBuildKitLogsOptions{JobID: jobID})
	if err != nil {
		t.Fatalf("QueryBuildKitLogs (after the late refusal): %v", err)
	}
	if len(afterLogs) != len(winnerLogs) {
		t.Fatalf("log count after the late refusal = %d, want unchanged from %d (no ERROR line appended)", len(afterLogs), len(winnerLogs))
	}

	// Control: an existing, still-unused token must still get its refusal
	// written and pollable - the fix must not make every refusal a no-op.
	const controlRaw = "pg-1314-control-unused-token"
	const controlJobID = "upload-1314-control"
	if err := CreatePullToken(controlRaw, "osid-1", controlJobID, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("CreatePullToken (control): %v", err)
	}
	if err := RecordFailedUploadIfUnclaimed(controlRaw, controlJobID, "osid-1", "control refusal, token still unused", AuthorizationLegacy, ""); err != nil {
		t.Fatalf("RecordFailedUploadIfUnclaimed (control): %v", err)
	}
	controlJob, err := GetBuildKitJob(controlJobID)
	if err != nil {
		t.Fatalf("GetBuildKitJob (control): %v", err)
	}
	if controlJob.Status != JobStatusFailed || controlJob.CompletedAt == nil {
		t.Fatalf("control job = %+v, want a recorded, pollable failure", controlJob)
	}
	controlLogs, err := QueryBuildKitLogs(QueryBuildKitLogsOptions{JobID: controlJobID})
	if err != nil {
		t.Fatalf("QueryBuildKitLogs (control): %v", err)
	}
	if len(controlLogs) == 0 {
		t.Fatal("control job must have at least one recorded log line")
	}
}
