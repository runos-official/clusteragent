package webhook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// FCR 1630: /health answered "OK connected" or "OK disconnected" to any caller,
// on the in-cluster listener and on the public upload listener, so anyone could
// read the state of the agent's control link.

func TestHealthBodyCarriesNoControlLinkState(t *testing.T) {
	rec := httptest.NewRecorder()
	HandleHealth(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (liveness must stay open)", rec.Code)
	}
	body := rec.Body.String()
	if body != "OK\n" {
		t.Fatalf("body = %q, want exactly %q", body, "OK\n")
	}
	if strings.Contains(body, "connected") {
		t.Fatalf("body %q reveals the control-link state", body)
	}
}

func TestWebhookListenerStillServesHealth(t *testing.T) {
	rec := httptest.NewRecorder()
	webhookMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "OK\n" {
		t.Fatalf("webhook /health = %d %q, want 200 \"OK\\n\"", rec.Code, rec.Body.String())
	}
}

func TestUploadListenerHasNoHealthRoute(t *testing.T) {
	rec := httptest.NewRecorder()
	uploadMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("upload listener /health = %d, want 404: it is published on the public ingress", rec.Code)
	}
}
