package webhook

import (
	"log"
	"net/http"
	"time"
)

// Start initializes and starts the webhook HTTP server
func Start() {
	mux := http.NewServeMux()

	// Register handlers
	mux.HandleFunc("/health", HandleHealth)

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Println("Starting webhook server on :8080")
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Webhook server failed: %v", err)
	}
}

// HandleHealth handles health check requests
func HandleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
