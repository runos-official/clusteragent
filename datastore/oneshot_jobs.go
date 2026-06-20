package datastore

import (
	"database/sql"
	"fmt"
	"time"
)

// OneShotJob is the audit record for a `runos run` one-off task: a Kubernetes
// Job created from an app's prebuilt image to run a single command (typically
// a DB migration) and then torn down. The record outlives the Job object so
// the command, image, exit code, and timing remain retrievable after cleanup.
type OneShotJob struct {
	ID             int64      `json:"id"`
	RunID          string     `json:"run_id"`    // conductor-supplied unique id; also the buildkit_logs job_id for this run's pod logs
	OSID           string     `json:"osid"`      // app id; also the namespace
	Namespace      string     `json:"namespace"` // app namespace the Job runs in
	Image          string     `json:"image"`     // fully-resolved image ref (registry/runos-apps/<osid>:<sha>)
	Command        string     `json:"command"`   // JSON-encoded argv that was run
	K8sJobName     string     `json:"k8s_job_name"`
	Status         string     `json:"status"`    // pending|running|success|failed|timeout
	ExitCode       *int       `json:"exit_code"` // container exit code; nil until terminal
	Actor          string     `json:"actor"`     // who triggered the run (passed through from conductor)
	TimeoutSeconds int        `json:"timeout_seconds"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    *time.Time `json:"completed_at"`
}

// OneShotJob status constants. pending/running are non-terminal; success,
// failed, and timeout are terminal. timeout is kept distinct from failed so the
// audit trail (and the conductor) can tell a deadline kill from a non-zero
// command exit, even though both propagate as a non-zero result.
const (
	OneShotStatusPending = "pending"
	OneShotStatusRunning = "running"
	OneShotStatusSuccess = "success"
	OneShotStatusFailed  = "failed"
	OneShotStatusTimeout = "timeout"
)

// CreateOneShotJob inserts the audit record at dispatch time, before the Job
// has reached a terminal state. exit_code and completed_at are filled in later
// by UpdateOneShotJobResult.
func CreateOneShotJob(runID, osid, namespace, image, command, k8sJobName, actor string, timeoutSeconds int) error {
	query := `
		INSERT INTO one_shot_jobs (run_id, osid, namespace, image, command, k8s_job_name, status, actor, timeout_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := db.Exec(query, runID, osid, namespace, image, command, k8sJobName, OneShotStatusPending, actor, timeoutSeconds)
	return err
}

// UpdateOneShotJobStatus moves a run to a non-terminal status (e.g. running)
// without recording an exit code.
func UpdateOneShotJobStatus(runID, status string) error {
	result, err := db.Exec(
		`UPDATE one_shot_jobs SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE run_id = ?`,
		status, runID,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("one-shot job not found: %s", runID)
	}
	return nil
}

// UpdateOneShotJobResult records the terminal outcome: status, the container's
// real exit code, and the completion timestamp. Idempotent on re-call.
func UpdateOneShotJobResult(runID, status string, exitCode int) error {
	result, err := db.Exec(
		`UPDATE one_shot_jobs
		 SET status = ?, exit_code = ?, updated_at = CURRENT_TIMESTAMP, completed_at = CURRENT_TIMESTAMP
		 WHERE run_id = ?`,
		status, exitCode, runID,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("one-shot job not found: %s", runID)
	}
	return nil
}

// GetOneShotJob retrieves a single run by run_id.
func GetOneShotJob(runID string) (*OneShotJob, error) {
	query := `
		SELECT id, run_id, osid, namespace, image, command, k8s_job_name, status, exit_code, actor, timeout_seconds, created_at, updated_at, completed_at
		FROM one_shot_jobs
		WHERE run_id = ?
	`
	var job OneShotJob
	var exitCode sql.NullInt64
	err := db.QueryRow(query, runID).Scan(
		&job.ID,
		&job.RunID,
		&job.OSID,
		&job.Namespace,
		&job.Image,
		&job.Command,
		&job.K8sJobName,
		&job.Status,
		&exitCode,
		&job.Actor,
		&job.TimeoutSeconds,
		&job.CreatedAt,
		&job.UpdatedAt,
		&job.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	if exitCode.Valid {
		ec := int(exitCode.Int64)
		job.ExitCode = &ec
	}
	return &job, nil
}
