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
