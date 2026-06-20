package datastore

import (
	"fmt"
	"time"

	"gorm.io/gorm"
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

func (m OneShotJobModel) toDTO() OneShotJob {
	return OneShotJob{
		ID:             int64(m.ID),
		RunID:          m.RunID,
		OSID:           m.OSID,
		Namespace:      m.Namespace,
		Image:          m.Image,
		Command:        m.Command,
		K8sJobName:     m.K8sJobName,
		Status:         m.Status,
		ExitCode:       m.ExitCode,
		Actor:          m.Actor,
		TimeoutSeconds: m.TimeoutSeconds,
		CreatedAt:      m.CreatedAt,
		UpdatedAt:      m.UpdatedAt,
		CompletedAt:    m.CompletedAt,
	}
}

// CreateOneShotJob inserts the audit record at dispatch time, before the Job
// has reached a terminal state. exit_code and completed_at are filled in later
// by UpdateOneShotJobResult.
func CreateOneShotJob(runID, osid, namespace, image, command, k8sJobName, actor string, timeoutSeconds int) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	job := OneShotJobModel{
		RunID:          runID,
		OSID:           osid,
		Namespace:      namespace,
		Image:          image,
		Command:        command,
		K8sJobName:     k8sJobName,
		Status:         OneShotStatusPending,
		Actor:          actor,
		TimeoutSeconds: timeoutSeconds,
	}
	return gdb.Create(&job).Error
}

// UpdateOneShotJobStatus moves a run to a non-terminal status (e.g. running)
// without recording an exit code.
func UpdateOneShotJobStatus(runID, status string) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	res := gdb.Model(&OneShotJobModel{}).
		Where("run_id = ?", runID).
		Updates(map[string]any{
			"status":     status,
			"updated_at": gorm.Expr("CURRENT_TIMESTAMP"),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("one-shot job not found: %s", runID)
	}
	return nil
}

// UpdateOneShotJobResult records the terminal outcome: status, the container's
// real exit code, and the completion timestamp. Idempotent on re-call.
func UpdateOneShotJobResult(runID, status string, exitCode int) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	res := gdb.Model(&OneShotJobModel{}).
		Where("run_id = ?", runID).
		Updates(map[string]any{
			"status":       status,
			"exit_code":    exitCode,
			"updated_at":   gorm.Expr("CURRENT_TIMESTAMP"),
			"completed_at": gorm.Expr("CURRENT_TIMESTAMP"),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("one-shot job not found: %s", runID)
	}
	return nil
}

// GetOneShotJob retrieves a single run by run_id.
func GetOneShotJob(runID string) (*OneShotJob, error) {
	gdb, err := activeDB()
	if err != nil {
		return nil, err
	}
	var m OneShotJobModel
	if err := gdb.Where("run_id = ?", runID).First(&m).Error; err != nil {
		return nil, err
	}
	dto := m.toDTO()
	return &dto, nil
}
