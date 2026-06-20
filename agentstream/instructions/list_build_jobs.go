package instructions

import (
	"log"
	"time"

	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/datastore"
)

// ListBuildJobsRequest holds the request parameters
type ListBuildJobsRequest struct {
	Status    string `json:"status"`     // Optional: filter by status
	OSID      string `json:"osid"`       // Optional: filter by osid
	CreatedAt string `json:"created_at"` // Optional: filter by created_at (ISO 8601 format)
	Limit     int    `json:"limit"`      // Optional: limit number of results
	Desc      bool   `json:"desc"`       // Optional: order by newest first (default false = oldest first)
}

// ListBuildJobsResponse holds the response data
type ListBuildJobsResponse struct {
	Jobs []datastore.BuildKitJob `json:"jobs"`
}

// ListBuildJobs handles the LIST_BUILD_JOBS instruction
func ListBuildJobs(jsonB64 string) (string, string, error) {
	var req ListBuildJobsRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		return "ERROR", "", err
	}

	log.Printf("LIST_BUILD_JOBS request: status=%s, osid=%s, created_at=%s, limit=%d, desc=%v", req.Status, req.OSID, req.CreatedAt, req.Limit, req.Desc)

	// Parse createdAt if provided
	var createdAt *time.Time
	if req.CreatedAt != "" {
		t, err := time.Parse(time.RFC3339, req.CreatedAt)
		if err != nil {
			return "ERROR", "", err
		}
		createdAt = &t
	}

	// Query jobs
	opts := datastore.QueryBuildKitJobsOptions{
		Status:    req.Status,
		OSID:      req.OSID,
		CreatedAt: createdAt,
		Limit:     req.Limit,
		Desc:      req.Desc,
	}

	jobs, err := datastore.QueryBuildKitJobs(opts)
	if err != nil {
		return "ERROR", "", err
	}

	// Encode response
	resp := ListBuildJobsResponse{
		Jobs: jobs,
	}
	respJsonB64, err := commons.JsonB64Encode(resp)
	if err != nil {
		return "ERROR", "", err
	}

	return "LIST_BUILD_JOBS_RESPONSE", respJsonB64, nil
}
