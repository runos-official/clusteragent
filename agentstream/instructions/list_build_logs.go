package instructions

import (
	"fmt"
	"log"

	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/datastore"
)

// ListBuildLogsRequest holds the request parameters
type ListBuildLogsRequest struct {
	JobID   string `json:"job_id"`   // Required: job ID to query logs for
	SinceID int64  `json:"since_id"` // Optional: get logs with ID > since_id
	Limit   int    `json:"limit"`    // Optional: limit number of log entries (default 100)
	Desc    bool   `json:"desc"`     // Optional: order by newest first (default false)
}

// ListBuildLogsResponse holds the response data
type ListBuildLogsResponse struct {
	Logs []datastore.BuildKitLog `json:"logs"`
}

// ListBuildLogs handles the LIST_BUILD_LOGS instruction
func ListBuildLogs(jsonB64 string) (string, string, error) {
	var req ListBuildLogsRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		return "ERROR", "", err
	}

	log.Printf("LIST_BUILD_LOGS request: job_id=%s, since_id=%d, limit=%d, desc=%v", req.JobID, req.SinceID, req.Limit, req.Desc)

	// Validate required fields
	if req.JobID == "" {
		return "ERROR", "", fmt.Errorf("job_id is required")
	}

	// Set default limit if not specified
	if req.Limit == 0 {
		req.Limit = 100
	}

	// Query logs
	opts := datastore.QueryBuildKitLogsOptions{
		JobID:   req.JobID,
		SinceID: req.SinceID,
		Limit:   req.Limit,
		Desc:    req.Desc,
	}

	logs, err := datastore.QueryBuildKitLogs(opts)
	if err != nil {
		return "ERROR", "", err
	}

	// Encode response
	resp := ListBuildLogsResponse{
		Logs: logs,
	}
	respJsonB64, err := commons.JsonB64Encode(resp)
	if err != nil {
		return "ERROR", "", err
	}

	return "LIST_BUILD_LOGS_RESPONSE", respJsonB64, nil
}
