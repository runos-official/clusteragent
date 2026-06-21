package webhook

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/runos-official/clusteragent/agentstream"
)

// bindRetryDelay is how long Start/StartUploadServer wait before re-attempting
// ListenAndServe after a bind/serve error, so a transient bind failure (port
// briefly held by a terminating predecessor) recovers without restarting the
// whole pod.
const bindRetryDelay = 5 * time.Second

// Start initializes and starts the liveness webhook server on :8080.
//
// It NEVER calls log.Fatalf: this runs as a goroutine in main, so a fatal here
// would take down the agent's gRPC control link and every other role with it. A
// serve error is logged and the bind is retried. (Killing the process on a
// liveness-server failure is more defensible than on the upload server, but we
// still prefer log + continue: the gRPC link is the agent's reason to exist.)
func Start() {
	mux := http.NewServeMux()

	// Register handlers
	mux.HandleFunc("/health", HandleHealth)

	for {
		server := &http.Server{
			Addr:         ":8080",
			Handler:      mux,
			ReadTimeout:  15 * time.Second,
			WriteTimeout: 15 * time.Second,
			IdleTimeout:  60 * time.Second,
		}

		log.Println("Starting webhook server on :8080")
		err := server.ListenAndServe()
		// ListenAndServe always returns a non-nil error. Log and retry the bind
		// rather than crash the process.
		log.Printf("Webhook server (:8080) stopped: %v; retrying bind in %s", err, bindRetryDelay)
		time.Sleep(bindRetryDelay)
	}
}

// HandleHealth handles health check requests. It is a liveness probe: it always
// returns 200 OK so a momentarily-disconnected (but alive and reconnecting)
// agent is never killed. The control-stream state is surfaced in the response
// body for observability only, it does not affect the status code.
func HandleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	if agentstream.StreamConnected() {
		fmt.Fprintln(w, "OK connected")
		return
	}
	fmt.Fprintln(w, "OK disconnected")
}
