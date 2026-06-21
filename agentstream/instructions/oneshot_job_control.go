package instructions

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/datastore"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// oneShotDeleteTimeout bounds the Job delete API call so a slow/unreachable API
// server can't hang the cleanup handler indefinitely.
const oneShotDeleteTimeout = 30 * time.Second

// OneShotJobStatusRequest queries a run's terminal state. The conductor polls
// this alongside LIST_BUILD_LOGS to learn when the run has finished and with
// what exit code, then records it and signals cleanup.
type OneShotJobStatusRequest struct {
	RunID string `json:"runId"`
}

// OneShotJobStatusResponse reports the run's status and (once terminal) the
// real exit code. Terminal is true for success, failed, and timeout.
type OneShotJobStatusResponse struct {
	Found       bool   `json:"found"`
	RunID       string `json:"runId,omitempty"`
	Status      string `json:"status,omitempty"`
	Terminal    bool   `json:"terminal"`
	ExitCode    *int   `json:"exitCode,omitempty"`
	K8sJobName  string `json:"k8sJobName,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
	Message     string `json:"message,omitempty"`
}

// OneShotJobStatus handles the ONESHOT_JOB_STATUS instruction.
func OneShotJobStatus(jsonB64 string) (string, string, error) {
	const replyType = "ONESHOT_JOB_STATUS_RESPONSE"

	var req OneShotJobStatusRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		return "", "", err
	}
	if req.RunID == "" {
		return "", "", fmt.Errorf("runId is required")
	}

	job, err := datastore.GetOneShotJob(req.RunID)
	if err != nil {
		// Treat a missing row as not-found rather than an error, so the
		// conductor's poller gets a clean negative instead of a gRPC error.
		return replyOneShotStatus(replyType, OneShotJobStatusResponse{
			Found:   false,
			RunID:   req.RunID,
			Message: "no such run",
		})
	}

	resp := OneShotJobStatusResponse{
		Found:      true,
		RunID:      job.RunID,
		Status:     job.Status,
		Terminal:   isOneShotTerminal(job.Status),
		ExitCode:   job.ExitCode,
		K8sJobName: job.K8sJobName,
	}
	if job.CompletedAt != nil {
		resp.CompletedAt = job.CompletedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return replyOneShotStatus(replyType, resp)
}

// CleanupOneShotJobRequest asks the cluster agent to delete the Job object.
// The conductor sends this on every terminal path (success, failure, timeout)
// once it has captured the result. Logs survive in the datastore.
type CleanupOneShotJobRequest struct {
	RunID      string `json:"runId"`
	Namespace  string `json:"namespace,omitempty"`
	K8sJobName string `json:"k8sJobName,omitempty"`
}

// CleanupOneShotJobResponse acknowledges the delete. Deleting an
// already-absent Job is treated as success (idempotent).
type CleanupOneShotJobResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// CleanupOneShotJob handles the CLEANUP_ONESHOT_JOB instruction.
func CleanupOneShotJob(jsonB64 string) (string, string, error) {
	const replyType = "CLEANUP_ONESHOT_JOB_RESPONSE"

	var req CleanupOneShotJobRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		return "", "", err
	}
	if req.RunID == "" && req.K8sJobName == "" {
		return "", "", fmt.Errorf("runId or k8sJobName is required")
	}

	// Resolve namespace + job name from the audit record when not supplied,
	// so the conductor can clean up with just the run id.
	namespace := req.Namespace
	jobName := req.K8sJobName
	if namespace == "" || jobName == "" {
		if rec, err := datastore.GetOneShotJob(req.RunID); err == nil {
			if namespace == "" {
				namespace = rec.Namespace
			}
			if jobName == "" {
				jobName = rec.K8sJobName
			}
		}
	}
	if namespace == "" || jobName == "" {
		return replyCleanupOneShot(replyType, CleanupOneShotJobResponse{
			Success: false,
			Message: "could not resolve namespace/jobName for cleanup",
		})
	}

	clientset, err := newClientset()
	if err != nil {
		return replyCleanupOneShot(replyType, CleanupOneShotJobResponse{
			Success: false,
			Message: fmt.Sprintf("failed to create kubernetes client: %v", err),
		})
	}

	// Foreground propagation so the Job's pod is removed with it.
	policy := metav1.DeletePropagationForeground
	ctx, cancel := context.WithTimeout(context.Background(), oneShotDeleteTimeout)
	defer cancel()
	err = clientset.BatchV1().Jobs(namespace).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: &policy,
	})
	if err != nil && !errors.IsNotFound(err) {
		return replyCleanupOneShot(replyType, CleanupOneShotJobResponse{
			Success: false,
			Message: fmt.Sprintf("failed to delete job %s/%s: %v", namespace, jobName, err),
		})
	}

	log.Printf("CLEANUP_ONESHOT_JOB: deleted job %s/%s (run_id=%s)", namespace, jobName, req.RunID)
	return replyCleanupOneShot(replyType, CleanupOneShotJobResponse{Success: true})
}

func isOneShotTerminal(status string) bool {
	switch status {
	case datastore.OneShotStatusSuccess, datastore.OneShotStatusFailed, datastore.OneShotStatusTimeout:
		return true
	default:
		return false
	}
}

func replyOneShotStatus(replyType string, payload OneShotJobStatusResponse) (string, string, error) {
	encoded, err := commons.JsonB64Encode(payload)
	if err != nil {
		return "", "", err
	}
	return replyType, encoded, nil
}

func replyCleanupOneShot(replyType string, payload CleanupOneShotJobResponse) (string, string, error) {
	encoded, err := commons.JsonB64Encode(payload)
	if err != nil {
		return "", "", err
	}
	return replyType, encoded, nil
}
