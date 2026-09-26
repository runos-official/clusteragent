package webhook

import (
	"fmt"
	"log"
	"net/http"

	"github.com/runos-official/clusteragent/datastore"
)

// recordFailedUpload writes a failed build row with one ERROR line under the
// upload's job id. Conductor's deploy job polls that row by the same id, so a
// failed upload ends the job failed with this line as its reason instead of
// leaving it at "Wait for build" (FCR 744). The log line goes in before the
// status flip so a reader that sees "failed" also sees why. It is a variable
// so tests can replace the datastore.
var recordFailedUpload = func(jobID, osid, message string) error {
	if err := datastore.CreateBuildKitJob(jobID, osid, "", "", jobID); err != nil {
		return fmt.Errorf("create build row: %w", err)
	}
	if err := datastore.InsertBuildKitLog(jobID, "ERROR: "+message); err != nil {
		return fmt.Errorf("insert build log: %w", err)
	}
	return datastore.UpdateBuildKitJobStatus(jobID, datastore.JobStatusFailed)
}

// archiveStoreFailure names why the source archive could not be stored. It
// keeps the historical "Failed to store archive" prefix and adds the cause,
// which is the registry address and the transport or TLS error.
func archiveStoreFailure(err error) string {
	return fmt.Sprintf("Failed to store archive in the cluster registry: %v", err)
}

// failUpload ends an upload the agent accepted a token for but cannot build:
// it records the failed build row and answers the CLI with the same message.
// A datastore error is logged and never hides the answer.
func failUpload(w http.ResponseWriter, osid, uploadID string, status int, message string) {
	log.Printf("Upload failed (osid=%s, uploadID=%s): %s", osid, uploadID, message)
	if err := recordFailedUpload(uploadID, osid, message); err != nil {
		log.Printf("Failed to record the failed upload (uploadID=%s): %v", uploadID, err)
	}
	http.Error(w, message, status)
}
