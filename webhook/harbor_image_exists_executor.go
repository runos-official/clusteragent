package webhook

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/runos-official/clusteragent/agentstream"
)

// harborProject is the fixed project namespace under which RunOS pushes app
// images. Mirrors the constant used by conductor and the build pipeline.
const harborProject = "runos-apps"

// harborImageExistsTimeout caps each individual HEAD attempt. Internal
// service calls are <100ms typically, so this is purely a defensive ceiling.
const harborImageExistsTimeout = 10 * time.Second

// HarborImageExists is the implementation of
// instructions.HarborImageExistsExecutor. Performs a HEAD against the
// Docker registry v2 manifest endpoint at the in-cluster Harbor URL pulled
// from BuildKitConfig — the same source the build pipeline uses. Returns
// true on 200, false on 404, error otherwise.
//
// Wired in main.go (mirrors RunVcsBuild). Lives in webhook (rather than
// the instructions package) so it can import agentstream without the
// agentstream → instructions cycle.
func HarborImageExists(osid, tag string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), harborImageExistsTimeout)
	defer cancel()

	k8sClient, err := agentstream.NewK8sClient()
	if err != nil {
		return false, fmt.Errorf("k8s client: %w", err)
	}

	cfg, err := k8sClient.GetBuildKitConfig(ctx)
	if err != nil {
		return false, fmt.Errorf("buildkit config: %w", err)
	}

	if cfg.HarborURL == "" {
		return false, fmt.Errorf("buildkit config has no harbor-url")
	}

	// HarborURL from the buildkit configmap is bare host[:port] (no scheme).
	// The registry's v2 API requires HTTPS. If the configmap ever ships with
	// a scheme already we strip first to avoid a double-prefix.
	base := strings.TrimSuffix(cfg.HarborURL, "/")
	base = strings.TrimPrefix(base, "https://")
	base = strings.TrimPrefix(base, "http://")
	url := fmt.Sprintf("https://%s/v2/%s/%s/manifests/%s", base, harborProject, osid, tag)

	auth := base64.StdEncoding.EncodeToString([]byte(cfg.HarborUsername + ":" + cfg.HarborPassword))

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+auth)
	// Harbor speaks both Docker and OCI manifest media types; accept either.
	req.Header.Set("Accept",
		"application/vnd.docker.distribution.manifest.v2+json, "+
			"application/vnd.oci.image.manifest.v1+json, "+
			"application/vnd.docker.distribution.manifest.list.v2+json, "+
			"application/vnd.oci.image.index.v1+json")

	// In-cluster Harbor commonly serves a self-signed or internal-CA cert.
	// We're hitting a service we own at a name we control, so skipping
	// verification is the established pattern (matches harborclient).
	httpClient := &http.Client{
		Timeout: harborImageExistsTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("HEAD %s: %w", url, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		log.Printf("HARBOR_IMAGE_EXISTS: %s/%s:%s present", harborProject, osid, tag)
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return false, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
