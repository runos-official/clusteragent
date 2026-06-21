package webhook

import (
	"log"
	"net/http"
	"time"
)

// StartUploadServer initializes and starts the upload HTTP server on port 8081.
// This server is exposed publicly via ingress for tarball uploads.
//
// It MUST NOT be able to take down the agent. This runs as a goroutine in main;
// the original log.Fatalf on a serve error would kill the whole process,
// severing the gRPC control link and every other role just because the upload
// listener could not bind. Instead we log the error and retry the bind, leaving
// the rest of the agent serving.
func StartUploadServer() {
	mux := http.NewServeMux()

	// Register handlers
	mux.HandleFunc("/cli-deploy/", HandleCLIDeployUpload)
	mux.HandleFunc("/cli-pull/", HandleCLIPullDownload)
	mux.HandleFunc("/health", HandleHealth)

	for {
		server := &http.Server{
			Addr:         ":8081",
			Handler:      mux,
			ReadTimeout:  5 * time.Minute, // Allow time for large uploads
			WriteTimeout: 5 * time.Minute, // Allow time for large downloads
			IdleTimeout:  60 * time.Second,
		}

		log.Println("Starting upload server on :8081")
		err := server.ListenAndServe()
		// ListenAndServe always returns a non-nil error. Log and retry the bind;
		// never crash the process — the upload role is the agent's least
		// critical and must not be able to sever the control link.
		log.Printf("Upload server (:8081) stopped: %v; retrying bind in %s", err, bindRetryDelay)
		time.Sleep(bindRetryDelay)
	}
}
