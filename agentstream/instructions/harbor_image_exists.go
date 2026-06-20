package instructions

import (
	"log"

	"github.com/runos-official/clusteragent/commons"
)

// HarborImageExistsRequest is the input for HARBOR_IMAGE_EXISTS.
//
// The cluster agent reaches Harbor at the in-cluster service URL pulled
// from BuildKitConfig (same source the build pipeline uses), so this works
// regardless of whether conductor itself has a network path to Harbor.
type HarborImageExistsRequest struct {
	OSID string `json:"osid"`
	Tag  string `json:"tag"`
}

// HarborImageExistsResponse reports whether `<osid>:<tag>` is present.
// Failures (k8s/Harbor unreachable, unexpected status) return Success=false
// and let the caller decide how to react. Conductor's caller treats any
// non-success as "force a rebuild" so a transient Harbor blip never aborts
// a deploy — it just costs a redundant build.
type HarborImageExistsResponse struct {
	Success bool   `json:"success"`
	Exists  bool   `json:"exists"`
	Message string `json:"message,omitempty"`
}

// HarborImageExistsExecutor is wired in main.go (which imports both this
// package and webhook). The executor lives in webhook because it needs
// the agentstream K8sClient to fetch BuildKit/Harbor config; this
// indirection mirrors VcsBuildExecutor and avoids the import cycle.
var HarborImageExistsExecutor func(osid, tag string) (bool, error)

// HarborImageExists handles the HARBOR_IMAGE_EXISTS instruction.
//
// Sync request/response — typically <500ms (one HEAD against an in-cluster
// service). Errors are returned as Success=false rather than as gRPC
// errors so the caller can keep the deploy moving.
func HarborImageExists(jsonB64 string) (string, string, error) {
	const replyType = "HARBOR_IMAGE_EXISTS_RESPONSE"

	var req HarborImageExistsRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		log.Printf("HARBOR_IMAGE_EXISTS: decode error: %v", err)
		return "", "", err
	}

	if req.OSID == "" || req.Tag == "" {
		return replyHarborImageExists(replyType, HarborImageExistsResponse{
			Success: false,
			Message: "osid and tag are required",
		})
	}

	if HarborImageExistsExecutor == nil {
		return replyHarborImageExists(replyType, HarborImageExistsResponse{
			Success: false,
			Message: "Harbor image-exists executor not registered (cluster agent misconfigured)",
		})
	}

	exists, err := HarborImageExistsExecutor(req.OSID, req.Tag)
	if err != nil {
		log.Printf("HARBOR_IMAGE_EXISTS: %s:%s — %v", req.OSID, req.Tag, err)
		return replyHarborImageExists(replyType, HarborImageExistsResponse{
			Success: false,
			Message: err.Error(),
		})
	}

	return replyHarborImageExists(replyType, HarborImageExistsResponse{
		Success: true,
		Exists:  exists,
	})
}

func replyHarborImageExists(replyType string, payload HarborImageExistsResponse) (string, string, error) {
	encoded, err := commons.JsonB64Encode(payload)
	if err != nil {
		return "", "", err
	}
	return replyType, encoded, nil
}
