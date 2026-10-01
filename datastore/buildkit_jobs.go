package datastore

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// BuildKitJob represents a build job record returned to callers. The JSON tags
// are part of the on-the-wire contract for LIST_BUILD_JOBS, so they are
// preserved exactly; the persistence shape lives in BuildKitJobModel.
type BuildKitJob struct {
	ID          int64      `json:"id"`
	JobID       string     `json:"job_id"`
	OSID        string     `json:"osid"`
	Repo        string     `json:"repo"`
	Branch      string     `json:"branch"`
	CommitHash  string     `json:"commit_hash"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

// JobStatus constants
const (
	JobStatusPending = "pending"
	JobStatusBusy    = "busy"
	JobStatusSuccess = "success"
	JobStatusFailed  = "failed"
)

func (m BuildKitJobModel) toDTO() BuildKitJob {
	return BuildKitJob{
		ID:          int64(m.ID),
		JobID:       m.JobID,
		OSID:        m.OSID,
		Repo:        m.Repo,
		Branch:      m.Branch,
		CommitHash:  m.CommitHash,
		Status:      m.Status,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
		CompletedAt: m.CompletedAt,
	}
}

// CreateBuildKitJob creates a new build job record
func CreateBuildKitJob(jobID, osid, repo, branch, commitHash string) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	job := BuildKitJobModel{
		JobID:      jobID,
		OSID:       osid,
		Repo:       repo,
		Branch:     branch,
		CommitHash: commitHash,
		Status:     JobStatusPending,
	}
	return gdb.Create(&job).Error
}

// UpdateBuildKitJobStatus updates the status of a build job
func UpdateBuildKitJobStatus(jobID, status string) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	updates := map[string]any{
		"status":     status,
		"updated_at": gorm.Expr("CURRENT_TIMESTAMP"),
	}
	if status == JobStatusSuccess || status == JobStatusFailed {
		updates["completed_at"] = gorm.Expr("CURRENT_TIMESTAMP")
	}
	res := gdb.Model(&BuildKitJobModel{}).Where("job_id = ?", jobID).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("job not found: %s", jobID)
	}
	return nil
}

// FailNonTerminalBuildKitJobs closes every build row that is still pending or
// busy: it marks the row failed and writes one "ERROR: <reason>" log line, so
// the reason reaches whoever reads the build log. It returns how many rows it
// closed.
//
// Only call it when no build can be running in this process: at startup, before
// the agent accepts work (the previous process took its builds with it), or at
// shutdown after the drain gave up. A row that a live build still owns would be
// failed under it.
func FailNonTerminalBuildKitJobs(reason string) (int, error) {
	gdb, err := activeDB()
	if err != nil {
		return 0, err
	}
	closed := 0
	err = gdb.Transaction(func(tx *gorm.DB) error {
		var jobIDs []string
		if err := tx.Model(&BuildKitJobModel{}).
			Where("status IN ?", []string{JobStatusPending, JobStatusBusy}).
			Pluck("job_id", &jobIDs).Error; err != nil {
			return err
		}
		if len(jobIDs) == 0 {
			return nil
		}
		res := tx.Model(&BuildKitJobModel{}).Where("job_id IN ?", jobIDs).Updates(map[string]any{
			"status":       JobStatusFailed,
			"updated_at":   gorm.Expr("CURRENT_TIMESTAMP"),
			"completed_at": gorm.Expr("CURRENT_TIMESTAMP"),
		})
		if res.Error != nil {
			return res.Error
		}
		logs := make([]BuildKitLogModel, 0, len(jobIDs))
		for _, id := range jobIDs {
			logs = append(logs, BuildKitLogModel{JobID: id, LogEntry: "ERROR: " + reason})
		}
		if err := tx.Create(&logs).Error; err != nil {
			return err
		}
		closed = len(jobIDs)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return closed, nil
}

// GetBuildKitJob retrieves a single job by job_id
func GetBuildKitJob(jobID string) (*BuildKitJob, error) {
	gdb, err := activeDB()
	if err != nil {
		return nil, err
	}
	var m BuildKitJobModel
	if err := gdb.Where("job_id = ?", jobID).First(&m).Error; err != nil {
		return nil, err
	}
	dto := m.toDTO()
	return &dto, nil
}

// QueryBuildKitJobsOptions holds filter options for querying jobs
type QueryBuildKitJobsOptions struct {
	Status    string
	OSID      string
	CreatedAt *time.Time
	Limit     int
	Desc      bool
}

// QueryBuildKitJobs retrieves jobs with optional filters
func QueryBuildKitJobs(opts QueryBuildKitJobsOptions) ([]BuildKitJob, error) {
	gdb, err := activeDB()
	if err != nil {
		return nil, err
	}
	q := gdb.Model(&BuildKitJobModel{})

	if opts.Status != "" {
		q = q.Where("status = ?", opts.Status)
	}
	if opts.OSID != "" {
		q = q.Where("osid = ?", opts.OSID)
	}
	if opts.CreatedAt != nil {
		q = q.Where("created_at >= ?", *opts.CreatedAt)
	}
	if opts.Desc {
		q = q.Order("created_at DESC")
	} else {
		q = q.Order("created_at ASC")
	}
	if opts.Limit > 0 {
		q = q.Limit(opts.Limit)
	}

	var models []BuildKitJobModel
	if err := q.Find(&models).Error; err != nil {
		return nil, err
	}

	var jobs []BuildKitJob
	for _, m := range models {
		jobs = append(jobs, m.toDTO())
	}
	return jobs, nil
}
