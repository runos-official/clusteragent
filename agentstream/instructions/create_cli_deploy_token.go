package instructions

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/datastore"
)

// CreateCLIDeployTokenRequest holds the request for creating a CLI deploy
// token. It serves two flows, discriminated by Purpose:
//
//   - app deploy (Purpose empty / "upload"): the historical CLI-deploy
//     upload. OSID is required; the uploaded tarball becomes app OSID's
//     image and is rolled out.
//   - app-less build-image (Purpose "build_image", obj-47): the uploaded
//     tarball is a generic build context built and pushed to explicit
//     Harbor target ref(s) WITHOUT any app/deploy. Repo + Tags are required
//     and OSID is ignored; the project is fixed to `runos-apps` on the
//     build side, so only the repo + tag list travel on the wire.
type CreateCLIDeployTokenRequest struct {
	OSID          string               `json:"osid,omitempty"`          // Required for app deploys; ignored for build_image.
	CliUploadID   string               `json:"cliUploadId,omitempty"`   // Conductor-assigned upload identity. Optional for legacy callers.
	Dockerfile    string               `json:"dockerfile,omitempty"`    // Iter-27 I27-Y: path to the Dockerfile inside the tarball, relative to the tarball root. Empty defaults to "Dockerfile" at the root.
	BuildArgs     []datastore.BuildArg `json:"buildArgs,omitempty"`     // Effective Docker build args ([{key,value,source}]) merged + validated by conductor. Omitted (back-compat) when the deploy has none.
	ExpirySeconds int                  `json:"expirySeconds,omitempty"` // Default 300, max 600

	// Purpose selects the flow: "" / "upload" => app deploy (default,
	// back-compat); "build_image" => app-less build-and-push (obj-47).
	Purpose string `json:"purpose,omitempty"`
	// Repo + Tags are the explicit Harbor target for build_image tokens:
	// repo under the fixed `runos-apps` project, with one or more tags (the
	// same built image pushed to each). Required when Purpose=="build_image",
	// ignored otherwise.
	Repo string   `json:"repo,omitempty"`
	Tags []string `json:"tags,omitempty"`
}

// CreateCLIDeployTokenResponse holds the response after creating a token
type CreateCLIDeployTokenResponse struct {
	Success   bool   `json:"success"`
	Token     string `json:"token,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"` // ISO8601 timestamp
	UploadURL string `json:"uploadUrl,omitempty"` // /cli-deploy/{token}
	Message   string `json:"message,omitempty"`
}

const (
	defaultExpirySeconds = 300 // 5 minutes
	maxExpirySeconds     = 600 // 10 minutes
	tokenBytes           = 32  // 256-bit token
)

// CreateCLIDeployToken handles the CREATE_CLI_DEPLOY_TOKEN instruction
func CreateCLIDeployToken(jsonB64 string) (string, string, error) {
	log.Printf("CreateCLIDeployToken called")

	var req CreateCLIDeployTokenRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		log.Printf("Error decoding message: %s", err)
		return "", "", err
	}

	// Validate required fields per flow. build_image is app-less and needs
	// a target repo + at least one tag instead of an osid; the app-deploy
	// flow keeps its osid requirement.
	if req.Purpose == datastore.PurposeBuildImage {
		if req.Repo == "" || len(req.Tags) == 0 {
			response := CreateCLIDeployTokenResponse{
				Success: false,
				Message: "repo and at least one tag are required for a build_image token",
			}
			jsonResponse, _ := commons.JsonB64Encode(response)
			return "CREATE_CLI_DEPLOY_TOKEN_RESPONSE", jsonResponse, nil
		}
	} else if req.OSID == "" {
		response := CreateCLIDeployTokenResponse{
			Success: false,
			Message: "osid is required",
		}
		jsonResponse, _ := commons.JsonB64Encode(response)
		return "CREATE_CLI_DEPLOY_TOKEN_RESPONSE", jsonResponse, nil
	}

	// Set expiry time
	expirySeconds := req.ExpirySeconds
	if expirySeconds <= 0 {
		expirySeconds = defaultExpirySeconds
	}
	if expirySeconds > maxExpirySeconds {
		expirySeconds = maxExpirySeconds
	}
	expiresAt := time.Now().Add(time.Duration(expirySeconds) * time.Second)

	// Generate cryptographically secure token
	tokenBuf := make([]byte, tokenBytes)
	if _, err := rand.Read(tokenBuf); err != nil {
		log.Printf("Error generating token: %v", err)
		response := CreateCLIDeployTokenResponse{
			Success: false,
			Message: "Failed to generate secure token",
		}
		jsonResponse, _ := commons.JsonB64Encode(response)
		return "CREATE_CLI_DEPLOY_TOKEN_RESPONSE", jsonResponse, nil
	}
	token := hex.EncodeToString(tokenBuf)

	// Store token. For build_image (obj-47) the token carries the explicit
	// target repo + tag list (app-less); for app deploys it carries the
	// conductor-assigned cliUploadId that the upload handler reads back as
	// the Harbor tag + BuildKit jobID (legacy callers may omit it, in which
	// case the upload handler mints one at upload time).
	var storeErr error
	if req.Purpose == datastore.PurposeBuildImage {
		uploadID := req.CliUploadID
		if uploadID == "" {
			uploadID = token[:16] // fall back to a stable id derived from the token
		}
		storeErr = datastore.CreateBuildImageToken(token, uploadID, req.Repo, req.Tags, req.Dockerfile, req.BuildArgs, expiresAt)
	} else {
		storeErr = datastore.CreateUploadToken(token, req.OSID, req.CliUploadID, req.Dockerfile, req.BuildArgs, expiresAt)
	}
	if storeErr != nil {
		log.Printf("Error storing token: %v", storeErr)
		response := CreateCLIDeployTokenResponse{
			Success: false,
			Message: "Failed to store token",
		}
		jsonResponse, _ := commons.JsonB64Encode(response)
		return "CREATE_CLI_DEPLOY_TOKEN_RESPONSE", jsonResponse, nil
	}

	if req.Purpose == datastore.PurposeBuildImage {
		log.Printf("Created build-image token for repo=%s, tags=%v, dockerfile=%q, buildArgs=%d, expires=%s", req.Repo, req.Tags, req.Dockerfile, len(req.BuildArgs), expiresAt.Format(time.RFC3339))
	} else {
		log.Printf("Created CLI deploy token for osid=%s, dockerfile=%q, buildArgs=%d, expires=%s", req.OSID, req.Dockerfile, len(req.BuildArgs), expiresAt.Format(time.RFC3339))
	}

	response := CreateCLIDeployTokenResponse{
		Success:   true,
		Token:     token,
		ExpiresAt: expiresAt.Format(time.RFC3339),
		UploadURL: fmt.Sprintf("/cli-deploy/%s", token),
		Message:   "CLI deploy token created successfully",
	}

	jsonResponse, err := commons.JsonB64Encode(response)
	if err != nil {
		log.Printf("Error encoding response: %v", err)
		return "", "", err
	}

	return "CREATE_CLI_DEPLOY_TOKEN_RESPONSE", jsonResponse, nil
}
