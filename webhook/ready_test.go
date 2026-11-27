package webhook

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A rolling update stops the old agent pod only when the new one is Ready.
// /readyz is the readiness probe: Ready means this agent holds the control
// connection, has not stepped aside for a newer pod, is not draining, and
// its HTTPS webhook listener accepts connections.

func setReadyChecks(t *testing.T, agent, draining, serving bool) {
	t.Helper()
	oldAgent, oldDraining, oldServing := agentReady, agentDraining, httpsServing
	agentReady = func() bool { return agent }
	agentDraining = func() bool { return draining }
	httpsServing = func() bool { return serving }
	t.Cleanup(func() { agentReady, agentDraining, httpsServing = oldAgent, oldDraining, oldServing })
}

func TestReadyz(t *testing.T) {
	cases := []struct {
		name                     string
		agent, draining, serving bool
		want                     int
	}{
		{"holds the connection and serves", true, false, true, http.StatusOK},
		{"never held the connection, or stepped aside", false, false, true, http.StatusServiceUnavailable},
		{"draining for a restart", true, true, true, http.StatusServiceUnavailable},
		{"HTTPS webhook listener not up", true, false, false, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setReadyChecks(t, tc.agent, tc.draining, tc.serving)
			rec := httptest.NewRecorder()
			webhookMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != tc.want {
				t.Fatalf("/readyz = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// Liveness must not follow readiness: a pod that stepped aside keeps running
// its builds until Kubernetes stops it, so the kubelet must not restart it.
func TestHealthStaysOKWhenNotReady(t *testing.T) {
	setReadyChecks(t, false, true, false)
	rec := httptest.NewRecorder()
	webhookMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/health = %d while not ready, want 200", rec.Code)
	}
}

func TestUploadListenerHasNoReadyzRoute(t *testing.T) {
	rec := httptest.NewRecorder()
	uploadMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("upload listener /readyz = %d, want 404: it is published on the public ingress", rec.Code)
	}
}
