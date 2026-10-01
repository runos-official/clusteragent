package datastore

import (
	"strings"
	"testing"
)

func TestFailNonTerminalBuildKitJobs_ClosesOnlyNonTerminalRows(t *testing.T) {
	setupTestDB(t)

	rows := map[string]string{
		"job-pending": JobStatusPending,
		"job-busy":    JobStatusBusy,
		"job-success": JobStatusSuccess,
		"job-failed":  JobStatusFailed,
	}
	for id, status := range rows {
		if err := CreateBuildKitJob(id, "osid-1", "", "", id); err != nil {
			t.Fatalf("CreateBuildKitJob(%s): %v", id, err)
		}
		if status != JobStatusPending {
			if err := UpdateBuildKitJobStatus(id, status); err != nil {
				t.Fatalf("UpdateBuildKitJobStatus(%s): %v", id, err)
			}
		}
	}

	const reason = "interrupted by agent restart"
	closed, err := FailNonTerminalBuildKitJobs(reason)
	if err != nil {
		t.Fatalf("FailNonTerminalBuildKitJobs: %v", err)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2 (the pending row and the busy row)", closed)
	}

	for _, id := range []string{"job-pending", "job-busy"} {
		got, err := GetBuildKitJob(id)
		if err != nil {
			t.Fatalf("GetBuildKitJob(%s): %v", id, err)
		}
		if got.Status != JobStatusFailed {
			t.Errorf("%s status = %q, want %q", id, got.Status, JobStatusFailed)
		}
		if got.CompletedAt == nil {
			t.Errorf("%s CompletedAt not set", id)
		}
		logs, err := QueryBuildKitLogs(QueryBuildKitLogsOptions{JobID: id})
		if err != nil {
			t.Fatalf("QueryBuildKitLogs(%s): %v", id, err)
		}
		if len(logs) != 1 || !strings.HasPrefix(logs[0].LogEntry, "ERROR: ") || !strings.Contains(logs[0].LogEntry, reason) {
			t.Errorf("%s logs = %+v, want one ERROR line that names the reason", id, logs)
		}
	}

	// Terminal rows stay as they were and gain no log line.
	for id, want := range map[string]string{"job-success": JobStatusSuccess, "job-failed": JobStatusFailed} {
		got, err := GetBuildKitJob(id)
		if err != nil {
			t.Fatalf("GetBuildKitJob(%s): %v", id, err)
		}
		if got.Status != want {
			t.Errorf("%s status = %q, want %q (terminal rows must not change)", id, got.Status, want)
		}
		logs, _ := QueryBuildKitLogs(QueryBuildKitLogsOptions{JobID: id})
		if len(logs) != 0 {
			t.Errorf("%s gained log lines: %+v", id, logs)
		}
	}
}

func TestFailNonTerminalBuildKitJobs_NothingToClose(t *testing.T) {
	setupTestDB(t)

	closed, err := FailNonTerminalBuildKitJobs("interrupted by agent restart")
	if err != nil {
		t.Fatalf("FailNonTerminalBuildKitJobs: %v", err)
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 on an empty table", closed)
	}
}
