package datastore

import (
	"time"
)

// BuildKitLog represents a build log entry
type BuildKitLog struct {
	ID        int64     `json:"id"`
	JobID     string    `json:"job_id"`
	LogEntry  string    `json:"log_entry"`
	CreatedAt time.Time `json:"created_at"`
}

// InsertBuildKitLog inserts a new log entry for a build job
func InsertBuildKitLog(jobID, logEntry string) error {
	query := `
		INSERT INTO buildkit_logs (job_id, log_entry)
		VALUES (?, ?)
	`
	_, err := db.Exec(query, jobID, logEntry)
	return err
}

// QueryBuildKitLogsOptions holds filter options for querying logs
type QueryBuildKitLogsOptions struct {
	JobID   string
	SinceID int64
	Limit   int
	Desc    bool
}

// QueryBuildKitLogs retrieves logs for a specific job with optional filters
func QueryBuildKitLogs(opts QueryBuildKitLogsOptions) ([]BuildKitLog, error) {
	query := `
		SELECT id, job_id, log_entry, created_at
		FROM buildkit_logs
		WHERE job_id = ?
	`
	args := []any{opts.JobID}

	if opts.SinceID > 0 {
		query += " AND id > ?"
		args = append(args, opts.SinceID)
	}

	if opts.Desc {
		query += " ORDER BY id DESC"
	} else {
		query += " ORDER BY id ASC"
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

	var logs []BuildKitLog
	for rows.Next() {
		var log BuildKitLog
		err := rows.Scan(
			&log.ID,
			&log.JobID,
			&log.LogEntry,
			&log.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		logs = append(logs, log)
	}

	return logs, rows.Err()
}
