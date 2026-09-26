package webhook

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFailUpload pins FCR 744. When the agent could not store the source
// archive, it answered the CLI with a bare "Failed to store archive" and wrote
// no build row, so conductor's deploy job waited at "Wait for build" for its
// whole budget. The failure must now name its cause to the CLI AND leave a
// failed build row with one ERROR line under the upload's job id.
func TestFailUpload(t *testing.T) {
	type recorded struct{ jobID, osid, message string }
	var got []recorded
	prev := recordFailedUpload
	recordFailedUpload = func(jobID, osid, message string) error {
		got = append(got, recorded{jobID, osid, message})
		return nil
	}
	t.Cleanup(func() { recordFailedUpload = prev })

	cause := errors.New(`ensure project: Post "https://harbor.example.test:8890/api/v2.0/projects": tls: failed to verify certificate: x509: certificate signed by unknown authority`)
	w := httptest.NewRecorder()
	failUpload(w, "app-ab1cd", "upload-1", http.StatusInternalServerError, archiveStoreFailure(cause))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Failed to store archive", "x509: certificate signed by unknown authority"} {
		if !strings.Contains(body, want) {
			t.Errorf("response body %q does not contain %q", body, want)
		}
	}
	if len(got) != 1 {
		t.Fatalf("recorded %d failed rows, want 1", len(got))
	}
	if got[0].jobID != "upload-1" || got[0].osid != "app-ab1cd" {
		t.Errorf("recorded row = %+v, want job upload-1 for app-ab1cd", got[0])
	}
	if !strings.Contains(got[0].message, "x509") {
		t.Errorf("recorded message %q does not name the cause", got[0].message)
	}
}

// TestFailUpload_RecorderErrorStillAnswers: a datastore that is down must not
// swallow the answer to the CLI.
func TestFailUpload_RecorderErrorStillAnswers(t *testing.T) {
	prev := recordFailedUpload
	recordFailedUpload = func(string, string, string) error { return errors.New("datastore down") }
	t.Cleanup(func() { recordFailedUpload = prev })

	w := httptest.NewRecorder()
	failUpload(w, "app-ab1cd", "upload-1", http.StatusInternalServerError, "Internal error: no build config")

	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "no build config") {
		t.Fatalf("got %d %q, want 500 naming the cause", w.Code, w.Body.String())
	}
}
