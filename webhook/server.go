package webhook

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/drain"
)

// bindRetryDelay is how long Start/StartUploadServer wait before re-attempting
// ListenAndServe after a bind/serve error, so a transient bind failure (port
// briefly held by a terminating predecessor) recovers without restarting the
// whole pod.
const bindRetryDelay = 5 * time.Second

// Start initializes and starts the liveness and readiness server on :8080.
//
// It NEVER calls log.Fatalf: this runs as a goroutine in main, so a fatal here
// would take down the agent's gRPC control link and every other role with it. A
// serve error is logged and the bind is retried. (Killing the process on a
// liveness-server failure is more defensible than on the upload server, but we
// still prefer log + continue: the gRPC link is the agent's reason to exist.)
func Start() {
	mux := webhookMux()

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

// webhookMux routes the in-cluster listener on :8080.
func webhookMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", HandleHealth)
	mux.HandleFunc("/readyz", HandleReady)
	return mux
}

// HandleHealth answers a liveness check: 200 and "OK" while the process serves.
// It says nothing about the control link: any pod can call it, so the link
// state stays out of the body (FCR 1630). Nothing in the platform reads more.
func HandleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "OK")
}

// httpsWebhookAddr is the cert-manager DNS01 webhook listener (dns01.Start).
// Its /healthz stays the liveness probe.
const httpsWebhookAddr = "127.0.0.1:443"

// httpsDialTimeout bounds the readiness check of httpsWebhookAddr.
const httpsDialTimeout = time.Second

// The readiness checks. Tests replace them.
var (
	agentReady    = agentstream.Ready
	agentDraining = drain.Draining
	httpsServing  = func() bool {
		conn, err := net.DialTimeout("tcp", httpsWebhookAddr, httpsDialTimeout)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
)

// HandleReady answers the readiness probe. It answers 200 when this agent
// has held the control connection, has not stepped aside for a newer agent
// pod, is not draining, and its HTTPS webhook listener accepts connections.
// Otherwise it answers 503. A rolling update stops the old pod only when the
// new one is Ready, and the Service sends uploads and DNS01 webhook calls
// only to Ready pods, so one agent takes that traffic at a time.
//
// The answer does not follow the live state of the control link: after the
// first answered heartbeat a dropped link keeps the pod Ready (FCR 1630
// keeps the link state from any caller). The body names no reason.
func HandleReady(w http.ResponseWriter, r *http.Request) {
	if agentReady() && !agentDraining() && httpsServing() {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ready")
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintln(w, "not ready")
}
