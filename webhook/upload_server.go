package webhook

import (
	"log"
	"net/http"
	"time"
)

// StartUploadServer initializes and starts the upload HTTP server on port 8081
// This server is exposed publicly via ingress for tarball uploads
func StartUploadServer() {
	mux := http.NewServeMux()

	// Register handlers
	mux.HandleFunc("/cli-deploy/", HandleCLIDeployUpload)
	mux.HandleFunc("/cli-pull/", HandleCLIPullDownload)
	mux.HandleFunc("/health", HandleHealth)

	server := &http.Server{
		Addr:         ":8081",
		Handler:      mux,
		ReadTimeout:  5 * time.Minute, // Allow time for large uploads
		WriteTimeout: 5 * time.Minute, // Allow time for large downloads
		IdleTimeout:  60 * time.Second,
	}

	log.Println("Starting upload server on :8081")
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Upload server failed: %v", err)
	}
}
