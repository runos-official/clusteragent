package datastore

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestOneShotJob_Lifecycle(t *testing.T) {
	setupTestDB(t)

	const runID = "run-abc123"
	if err := CreateOneShotJob(runID, "osid-1", "osid-1", "harbor.example.com/runos-apps/osid-1:sha123", `["scripts/release.sh"]`, "runos-run-run-abc123", "alice", 1800); err != nil {
		t.Fatalf("CreateOneShotJob: %v", err)
	}

	// Fresh record is pending with no exit code.
	got, err := GetOneShotJob(runID)
	if err != nil {
		t.Fatalf("GetOneShotJob: %v", err)
	}
	if got.Status != OneShotStatusPending {
		t.Fatalf("status = %q, want %q", got.Status, OneShotStatusPending)
	}
	if got.ExitCode != nil {
		t.Fatalf("ExitCode = %v, want nil before terminal", *got.ExitCode)
	}
	if got.TimeoutSeconds != 1800 || got.Actor != "alice" {
		t.Fatalf("metadata not persisted: timeout=%d actor=%q", got.TimeoutSeconds, got.Actor)
	}

	// Move to running.
	if err := UpdateOneShotJobStatus(runID, OneShotStatusRunning); err != nil {
		t.Fatalf("UpdateOneShotJobStatus: %v", err)
	}

	// Record a non-zero terminal result.
	if err := UpdateOneShotJobResult(runID, OneShotStatusFailed, 3); err != nil {
		t.Fatalf("UpdateOneShotJobResult: %v", err)
	}
	got, err = GetOneShotJob(runID)
	if err != nil {
		t.Fatalf("GetOneShotJob after result: %v", err)
	}
	if got.Status != OneShotStatusFailed {
		t.Fatalf("status = %q, want %q", got.Status, OneShotStatusFailed)
	}
	if got.ExitCode == nil || *got.ExitCode != 3 {
		t.Fatalf("ExitCode = %v, want 3", got.ExitCode)
	}
	if got.CompletedAt == nil {
		t.Fatalf("CompletedAt not set on terminal result")
	}
}

func TestOneShotJob_StatusUpdateMissing(t *testing.T) {
	setupTestDB(t)

	if err := UpdateOneShotJobStatus("nope", OneShotStatusRunning); err == nil {
		t.Fatalf("expected error updating a missing run, got nil")
	}
	if _, err := GetOneShotJob("nope"); err == nil {
		t.Fatalf("expected error getting a missing run, got nil")
	}
}

// TestOneShotJob_ResultUpdateMissing pins that recording a terminal result for
// a run_id that was never inserted is a "not found" error, not a silent no-op
// (the conductor relies on this to detect a lost audit row).
func TestOneShotJob_ResultUpdateMissing(t *testing.T) {
	setupTestDB(t)

	if err := UpdateOneShotJobResult("ghost", OneShotStatusSuccess, 0); err == nil {
		t.Fatalf("expected error recording a result for a missing run, got nil")
	}
}

// TestOneShotJob_SuccessTransition pins the happy path: pending -> running ->
// success with exit code 0, and that a zero exit code is persisted as a real
// 0 (not left NULL), so a clean run is distinguishable from one that never
// reached a terminal state.
func TestOneShotJob_SuccessTransition(t *testing.T) {
	setupTestDB(t)

	const runID = "run-success"
	if err := CreateOneShotJob(runID, "app-mn4pq", "app-mn4pq", "harbor.example.com/runos-apps/app-mn4pq:sha9", `["migrate"]`, "runos-run-run-success", "bob", 600); err != nil {
		t.Fatalf("CreateOneShotJob: %v", err)
	}
	if err := UpdateOneShotJobStatus(runID, OneShotStatusRunning); err != nil {
		t.Fatalf("to running: %v", err)
	}
	if err := UpdateOneShotJobResult(runID, OneShotStatusSuccess, 0); err != nil {
		t.Fatalf("to success: %v", err)
	}

	got, err := GetOneShotJob(runID)
	if err != nil {
		t.Fatalf("GetOneShotJob: %v", err)
	}
	if got.Status != OneShotStatusSuccess {
		t.Errorf("status = %q, want %q", got.Status, OneShotStatusSuccess)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want a real 0", got.ExitCode)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt not set on terminal success")
	}
}

// TestOneShotJob_TimeoutTransition pins that the timeout terminal status is
// recorded distinctly from failed (the audit trail must tell a deadline kill
// apart from a non-zero command exit), and carries the kill exit code.
func TestOneShotJob_TimeoutTransition(t *testing.T) {
	setupTestDB(t)

	const runID = "run-timeout"
	if err := CreateOneShotJob(runID, "app-mn4pq", "app-mn4pq", "img:sha", `["sleep","infinity"]`, "runos-run-run-timeout", "carol", 5); err != nil {
		t.Fatalf("CreateOneShotJob: %v", err)
	}
	if err := UpdateOneShotJobResult(runID, OneShotStatusTimeout, 137); err != nil {
		t.Fatalf("to timeout: %v", err)
	}

	got, err := GetOneShotJob(runID)
	if err != nil {
		t.Fatalf("GetOneShotJob: %v", err)
	}
	if got.Status != OneShotStatusTimeout {
		t.Errorf("status = %q, want %q (distinct from failed)", got.Status, OneShotStatusTimeout)
	}
	if got.ExitCode == nil || *got.ExitCode != 137 {
		t.Errorf("ExitCode = %v, want 137", got.ExitCode)
	}
}

// TestOneShotJob_ResultIsIdempotent pins that re-recording the terminal result
// (the conductor may re-deliver the completion) succeeds and overwrites the
// prior values rather than erroring or duplicating the row.
func TestOneShotJob_ResultIsIdempotent(t *testing.T) {
	setupTestDB(t)

	const runID = "run-idem"
	if err := CreateOneShotJob(runID, "app-mn4pq", "app-mn4pq", "img:sha", `["x"]`, "k8s-job", "dan", 60); err != nil {
		t.Fatalf("CreateOneShotJob: %v", err)
	}
	if err := UpdateOneShotJobResult(runID, OneShotStatusFailed, 1); err != nil {
		t.Fatalf("first result: %v", err)
	}
	// Re-deliver with a corrected outcome; the latest write wins.
	if err := UpdateOneShotJobResult(runID, OneShotStatusSuccess, 0); err != nil {
		t.Fatalf("second result: %v", err)
	}

	got, err := GetOneShotJob(runID)
	if err != nil {
		t.Fatalf("GetOneShotJob: %v", err)
	}
	if got.Status != OneShotStatusSuccess || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("after re-deliver: status=%q exit=%v, want success/0", got.Status, got.ExitCode)
	}
}

// TestOneShotJob_DuplicateRunID pins that run_id is unique: a second insert
// with the same run_id is rejected by the schema constraint.
func TestOneShotJob_DuplicateRunID(t *testing.T) {
	setupTestDB(t)

	const runID = "run-dup"
	if err := CreateOneShotJob(runID, "app-mn4pq", "app-mn4pq", "img:sha", `["x"]`, "k8s-job", "eve", 60); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := CreateOneShotJob(runID, "app-mn4pq", "app-mn4pq", "img:sha", `["x"]`, "k8s-job", "eve", 60); err == nil {
		t.Fatal("expected a UNIQUE-constraint error on duplicate run_id, got nil")
	}
}
