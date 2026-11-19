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
//
// Call this for a failure AFTER the token's atomic claim succeeded (the
// archive push, or a permit that expired between the claim and the push):
// the token is consumed either way, so uploadID - fixed for the token's
// whole life - is never reused by a legitimate retry, and the row is safe
// to record. Also call it for a PRE-claim refusal that is terminal (review
// finding 1133): retrying the exact same input would refuse the same way,
// so Conductor's deploy job needs a recorded, pollable reason instead of
// waiting out its whole budget (FCR 744) - CreateBuildKitJob upserts, so a
// later, fixed retry that reuses this same token and uploadID can still
// claim and build under this jobID.
func failUpload(w http.ResponseWriter, osid, uploadID string, status int, message string) {
	failUploadWithReason(w, osid, uploadID, status, message, message)
}

// failUploadWithReason is failUpload with the logged/recorded detail (reason)
// and the caller-visible text (message) split apart, for a refusal whose
// generic HTTP answer must not carry the detail that is still useful in the
// server log and the build row (review "Refusal behavior": never reveal
// enough to enumerate another account, cluster, app, binding, or
// credential over HTTP; the build log is read only by whoever already holds
// this token's own upload, so it is not the same exposure).
func failUploadWithReason(w http.ResponseWriter, osid, uploadID string, status int, reason, message string) {
	log.Printf("Upload failed (osid=%s, uploadID=%s): %s", osid, uploadID, reason)
	if err := recordFailedUpload(uploadID, osid, reason); err != nil {
		log.Printf("Failed to record the failed upload (uploadID=%s): %v", uploadID, err)
	}
	http.Error(w, message, status)
}

// refuseUpload answers a PRE-claim refusal: the token was never claimed, so
// it (and its fixed uploadID) remain valid for a legitimate retry. Unlike
// failUpload, this records no build-job row. Review findings 1075/1086: the
// story moved the atomic claim after the tarball, configuration, Harbor
// identity, and scoped-recheck checks specifically so a refusal here leaves
// the token retryable - but failUpload's build-job row carries a UNIQUE
// job_id keyed on that same uploadID, so recording it here would let the
// very first refusal permanently block every later attempt (including the
// one that is finally permitted) from ever creating its build row.
func refuseUpload(w http.ResponseWriter, osid, uploadID string, status int, reason string, message string) {
	log.Printf("Upload refused before token claim (osid=%s, uploadID=%s): %s", osid, uploadID, reason)
	http.Error(w, message, status)
}

// reportUploadRefusal answers a pre-claim *uploadRefusal from the Harbor
// identity check or the scoped admission decision, choosing failUpload (a
// terminal refusal: record a pollable row) or refuseUpload (a retryable
// refusal: no row, token stays retryable) by refusal.terminal (review
// finding 1133).
func reportUploadRefusal(w http.ResponseWriter, osid, uploadID string, refusal *uploadRefusal) {
	if refusal.terminal {
		failUploadWithReason(w, osid, uploadID, refusal.status, refusal.reason, refusal.message)
		return
	}
	refuseUpload(w, osid, uploadID, refusal.status, refusal.reason, refusal.message)
}
