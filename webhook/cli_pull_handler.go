package webhook

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/datastore"
	"github.com/runos-official/clusteragent/harborclient"
)

// HandleCLIPullDownload streams a previously archived CLI tarball back to the
// caller. Mirrors HandleCLIDeployUpload's single-use token validation.
func HandleCLIPullDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/cli-pull/"), "/")
	if len(pathParts) == 0 || pathParts[0] == "" {
		http.Error(w, "Token required", http.StatusBadRequest)
		return
	}
	token := pathParts[0]

	pullToken, err := datastore.GetPullToken(token)
	if err != nil {
		log.Printf("Pull token lookup failed: %v", err)
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	if time.Now().After(pullToken.ExpiresAt) {
		log.Printf("Pull token expired: %s", token[:8])
		http.Error(w, "Token expired", http.StatusUnauthorized)
		return
	}

	if pullToken.Used {
		log.Printf("Pull token already used: %s", token[:8])
		http.Error(w, "Token already used", http.StatusUnauthorized)
		return
	}

	if err := datastore.MarkUploadTokenUsed(token); err != nil {
		log.Printf("Failed to mark pull token as used: %v", err)
		http.Error(w, "Token validation failed", http.StatusInternalServerError)
		return
	}

	osid, cliUploadID, ok := strings.Cut(pullToken.DeployConfig, ":")
	if !ok || osid == "" || cliUploadID == "" {
		log.Printf("Pull token has malformed deploy_config: %q", pullToken.DeployConfig)
		http.Error(w, "Token payload invalid", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()

	k8sClient, err := agentstream.NewK8sClient()
	if err != nil {
		log.Printf("Pull: failed to create K8s client: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	buildkitConfig, err := k8sClient.GetBuildKitConfig(ctx)
	if err != nil {
		log.Printf("Pull: failed to get BuildKit config: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	cfg := harborclient.Config{
		URL:      buildkitConfig.HarborURL,
		Username: buildkitConfig.HarborUsername,
		Password: buildkitConfig.HarborPassword,
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.tar.gz"`, osid, cliUploadID))

	pullCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := harborclient.PullArchive(pullCtx, cfg, osid, cliUploadID, w); err != nil {
		log.Printf("Pull: failed to stream archive osid=%s cliUploadID=%s: %v", osid, cliUploadID, err)
		// Headers already sent, can't change status; client will see truncated body.
		return
	}

	log.Printf("Pull: streamed archive osid=%s cliUploadID=%s, token=%s...", osid, cliUploadID, token[:8])
}
