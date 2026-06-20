package datastore

import (
	"fmt"
	"time"
)

// BuildKitJob represents a build job record
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

// CreateBuildKitJob creates a new build job record
func CreateBuildKitJob(jobID, osid, repo, branch, commitHash string) error {
	query := `
		INSERT INTO buildkit_jobs (job_id, osid, repo, branch, commit_hash, status)
		VALUES (?, ?, ?, ?, ?, ?)
	`
	_, err := db.Exec(query, jobID, osid, repo, branch, commitHash, JobStatusPending)
	return err
}

// UpdateBuildKitJobStatus updates the status of a build job
func UpdateBuildKitJobStatus(jobID, status string) error {
	var query string
	if status == JobStatusSuccess || status == JobStatusFailed {
		query = `
			UPDATE buildkit_jobs
			SET status = ?, updated_at = CURRENT_TIMESTAMP, completed_at = CURRENT_TIMESTAMP
			WHERE job_id = ?
		`
	} else {
		query = `
			UPDATE buildkit_jobs
			SET status = ?, updated_at = CURRENT_TIMESTAMP
			WHERE job_id = ?
		`
	}
	result, err := db.Exec(query, status, jobID)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("job not found: %s", jobID)
	}

	return nil
}

// GetBuildKitJob retrieves a single job by job_id
func GetBuildKitJob(jobID string) (*BuildKitJob, error) {
	query := `
		SELECT id, job_id, osid, repo, branch, commit_hash, status, created_at, updated_at, completed_at
		FROM buildkit_jobs
		WHERE job_id = ?
	`
	var job BuildKitJob
	err := db.QueryRow(query, jobID).Scan(
		&job.ID,
		&job.JobID,
		&job.OSID,
		&job.Repo,
		&job.Branch,
		&job.CommitHash,
		&job.Status,
		&job.CreatedAt,
		&job.UpdatedAt,
		&job.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &job, nil
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
	query := `
		SELECT id, job_id, osid, repo, branch, commit_hash, status, created_at, updated_at, completed_at
		FROM buildkit_jobs
		WHERE 1=1
	`
	args := []any{}

	if opts.Status != "" {
		query += " AND status = ?"
		args = append(args, opts.Status)
	}

	if opts.OSID != "" {
		query += " AND osid = ?"
		args = append(args, opts.OSID)
	}

	if opts.CreatedAt != nil {
		query += " AND created_at >= ?"
		args = append(args, opts.CreatedAt)
	}

	if opts.Desc {
		query += " ORDER BY created_at DESC"
	} else {
		query += " ORDER BY created_at ASC"
	}

	if opts.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, opts.Limit)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []BuildKitJob
	for rows.Next() {
		var job BuildKitJob
		err := rows.Scan(
			&job.ID,
			&job.JobID,
			&job.OSID,
			&job.Repo,
			&job.Branch,
			&job.CommitHash,
			&job.Status,
			&job.CreatedAt,
			&job.UpdatedAt,
			&job.CompletedAt,
		)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}

	return jobs, rows.Err()
}
