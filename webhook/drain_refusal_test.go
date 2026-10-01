package webhook

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/runos-official/clusteragent/drain"
)

// While the agent drains, the upload endpoint must answer 503 with a retry
// hint BEFORE it touches the upload token. The token is single use and the
// handler marks it used before it does any work, so a refusal after that point
// would burn the user's token and leave the deploy with no way to retry.
func TestHandleCLIDeployUpload_RefusesWhileDrainingBeforeTouchingToken(t *testing.T) {
	prev := beginWork
	beginWork = func() (func(), error) { return nil, drain.ErrDraining }
	t.Cleanup(func() { beginWork = prev })

	// No datastore is installed in this test. If the handler looked the token
	// up, it would fail with 401 or 500, never the 503 the test wants.
	req := httptest.NewRequest(http.MethodPost, "/cli-deploy/not-a-real-token", nil)
	w := httptest.NewRecorder()
	HandleCLIDeployUpload(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %q)", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("Retry-After header missing, want a retry hint for the CLI")
	}
}
