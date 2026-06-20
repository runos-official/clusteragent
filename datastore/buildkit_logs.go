package datastore

import (
	"time"
)

// BuildKitLog represents a build log entry returned to callers. JSON tags are
// the LIST_BUILD_LOGS wire contract; the persistence shape is BuildKitLogModel.
type BuildKitLog struct {
	ID        int64     `json:"id"`
	JobID     string    `json:"job_id"`
	LogEntry  string    `json:"log_entry"`
	CreatedAt time.Time `json:"created_at"`
}

func (m BuildKitLogModel) toDTO() BuildKitLog {
	return BuildKitLog{
		ID:        int64(m.ID),
		JobID:     m.JobID,
		LogEntry:  m.LogEntry,
		CreatedAt: m.CreatedAt,
	}
}

// InsertBuildKitLog inserts a new log entry for a build job
func InsertBuildKitLog(jobID, logEntry string) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	entry := BuildKitLogModel{JobID: jobID, LogEntry: logEntry}
	return gdb.Create(&entry).Error
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
	gdb, err := activeDB()
	if err != nil {
		return nil, err
	}
	q := gdb.Model(&BuildKitLogModel{}).Where("job_id = ?", opts.JobID)

	if opts.SinceID > 0 {
		q = q.Where("id > ?", opts.SinceID)
	}
	if opts.Desc {
		q = q.Order("id DESC")
	} else {
		q = q.Order("id ASC")
	}
	if opts.Limit > 0 {
		q = q.Limit(opts.Limit)
	}

	var models []BuildKitLogModel
	if err := q.Find(&models).Error; err != nil {
		return nil, err
	}

	var logs []BuildKitLog
	for _, m := range models {
		logs = append(logs, m.toDTO())
	}
	return logs, nil
}
