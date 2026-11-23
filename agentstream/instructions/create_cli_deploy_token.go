package instructions

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	// HarborOsid is the Harbor service identity Conductor verified and
	// locked before minting a build_image token (story 390 criterion 5,
	// src/util/harborBuildImage/prepareHarborBuildImage.ts). Only meaningful
	// for Purpose=="build_image". Omitted by a caller that predates this
	// field (or by any non-build_image request); a plain json.Unmarshal
	// silently ignores an unrecognised field, so an older agent decoding a
	// newer Conductor's request tolerates it being absent from its own
	// struct, and a newer agent decoding an older request simply sees "".
	HarborOsid string `json:"harborOsid,omitempty"`

	// AuthorizationClassification selects explicit legacy or scoped storage.
	// Omission without a binding preserves the historical legacy request.
	AuthorizationClassification string                            `json:"authorizationClassification,omitempty"`
	AuthorizationBinding        *datastore.UploadAuthorityBinding `json:"authorizationBinding,omitempty"`
	// DurableJobID (story 339) is Conductor's durable job-authority id
	// (handbook 949 C3's "job: jobId" stage authority) for the deploy this
	// upload belongs to: a SIBLING of AuthorizationBinding, never stored
	// inside it (the binding's own jobId must stay null). OPTIONAL even on
	// a scoped request: Conductor's rollout of this field is independent
	// of, and currently behind, the scoped-archive-persist rollout story
	// 338 already consumes. Present and non-empty, it must be a canonical
	// lowercase UUID (datastore.CreateScopedUploadToken validates this);
	// absent, the later queue_admission/build_start/deployment_patch
	// stages simply have no job authority to recheck against yet and run
	// unchecked (legacy), same as before this story. Ignored for a legacy
	// request.
	DurableJobID string `json:"durableJobId,omitempty"`
}

// ScopedUploadAck identifies the immutable authority and target saved for a scoped upload.
type ScopedUploadAck struct {
	Version   string `json:"version"`
	BindingID string `json:"bindingId"`
	AccountID string `json:"aid"`
	ClusterID string `json:"cid"`
	AppID     string `json:"appId"`
	UploadID  string `json:"uploadId"`
}

// CreateCLIDeployTokenResponse holds the response after creating a token
type CreateCLIDeployTokenResponse struct {
	Success   bool             `json:"success"`
	Token     string           `json:"token,omitempty"`
	ExpiresAt string           `json:"expiresAt,omitempty"` // ISO8601 timestamp
	UploadURL string           `json:"uploadUrl,omitempty"` // /cli-deploy/{token}
	Message   string           `json:"message,omitempty"`
	ScopedAck *ScopedUploadAck `json:"scopedAck,omitempty"`
}

const (
	defaultExpirySeconds = 300 // 5 minutes
	maxExpirySeconds     = 600 // 10 minutes
	tokenBytes           = 32  // 256-bit token
)

var (
	createUploadToken       = datastore.CreateUploadToken
	createScopedUploadToken = datastore.CreateScopedUploadToken
	createBuildImageToken   = datastore.CreateBuildImageToken
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

		// Review finding 1079: criterion 6(d) reserves the explicit legacy
		// branch (skip the Harbor-identity check at upload) for a request
		// that OMITS harborOsid. req.HarborOsid alone cannot tell omission
		// apart from an explicitly sent null or empty string - Go's
		// json.Unmarshal maps all three to the same "" zero value - so a
		// caller that sent an invalid present identity (null or "") would
		// otherwise silently mint a token that skips the registry check.
		// harborOsidPresence re-decodes the raw payload to tell them apart.
		present, err := harborOsidPresence(jsonB64)
		if err != nil {
			response := CreateCLIDeployTokenResponse{Success: false, Message: "harborOsid must be a string"}
			jsonResponse, _ := commons.JsonB64Encode(response)
			return "CREATE_CLI_DEPLOY_TOKEN_RESPONSE", jsonResponse, nil
		}
		if present && req.HarborOsid == "" {
			response := CreateCLIDeployTokenResponse{Success: false, Message: "harborOsid must not be null or empty when present"}
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

	authorization, err := requestAuthorizationClassification(req)
	if err != nil {
		response := CreateCLIDeployTokenResponse{Success: false, Message: err.Error()}
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
	expiresAt := time.Now().UTC().Truncate(time.Second).Add(time.Duration(expirySeconds) * time.Second)
	if authorization == datastore.AuthorizationScoped {
		// The immutable binding fixes expiry before transport to the agent.
		bindingExpiry, expiryErr := time.Parse(time.RFC3339, req.AuthorizationBinding.ExpiresAt)
		bindingCreated, createdErr := time.Parse(time.RFC3339, req.AuthorizationBinding.CreatedAt)
		if expiryErr != nil || createdErr != nil || !bindingExpiry.After(time.Now()) ||
			req.ExpirySeconds < 0 || req.ExpirySeconds > int(time.Hour/time.Second) || (req.ExpirySeconds > 0 &&
			bindingExpiry.Sub(bindingCreated) != time.Duration(req.ExpirySeconds)*time.Second) {
			response := CreateCLIDeployTokenResponse{Success: false, Message: "Invalid authorization binding expiry"}
			jsonResponse, _ := commons.JsonB64Encode(response)
			return "CREATE_CLI_DEPLOY_TOKEN_RESPONSE", jsonResponse, nil
		}
		expiresAt = bindingExpiry
		if err := datastore.ValidateUploadAuthorityBinding(*req.AuthorizationBinding, datastore.UploadAuthorityTarget{
			AppID:    req.OSID,
			UploadID: req.CliUploadID,
			Purpose:  datastore.PurposeUpload,
		}, expiresAt); err != nil {
			response := CreateCLIDeployTokenResponse{Success: false, Message: "Invalid authorization binding"}
			jsonResponse, _ := commons.JsonB64Encode(response)
			return "CREATE_CLI_DEPLOY_TOKEN_RESPONSE", jsonResponse, nil
		}
	}

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
	var scopedAck *ScopedUploadAck
	if req.Purpose == datastore.PurposeBuildImage {
		uploadID := req.CliUploadID
		if uploadID == "" {
			uploadID = token[:16] // fall back to a stable id derived from the token
		}
		storeErr = createBuildImageToken(token, uploadID, req.Repo, req.Tags, req.HarborOsid, req.Dockerfile, req.BuildArgs, expiresAt)
	} else if authorization == datastore.AuthorizationScoped {
		binding := *req.AuthorizationBinding
		storeErr = createScopedUploadToken(token, req.OSID, req.CliUploadID, req.Dockerfile, req.BuildArgs, expiresAt, binding, req.DurableJobID)
		if storeErr == nil {
			scopedAck = &ScopedUploadAck{
				Version: "runos.scoped-upload-ack.v1", BindingID: binding.BindingID,
				AccountID: binding.AccountID, ClusterID: binding.ClusterID, AppID: binding.AppID, UploadID: binding.UploadID,
			}
		}
	} else {
		storeErr = createUploadToken(token, req.OSID, req.CliUploadID, req.Dockerfile, req.BuildArgs, expiresAt)
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
		ExpiresAt: expiresAt.Format(time.RFC3339Nano),
		UploadURL: fmt.Sprintf("/cli-deploy/%s", token),
		Message:   "CLI deploy token created successfully",
		ScopedAck: scopedAck,
	}

	jsonResponse, err := commons.JsonB64Encode(response)
	if err != nil {
		log.Printf("Error encoding response: %v", err)
		return "", "", err
	}

	return "CREATE_CLI_DEPLOY_TOKEN_RESPONSE", jsonResponse, nil
}

// harborOsidPresence reports whether the raw request payload sent a
// "harborOsid" key at all, independent of its value. commons.JsonB64Decode
// uses a plain json.Unmarshal into a typed string field, which maps an
// absent key, an explicit null, and an empty string all to the same zero
// value - so the typed decode alone cannot support criterion 6(d)'s
// "omitted means legacy, present-but-invalid does not" rule (review finding
// 1079). This re-decodes the same payload into a raw field map to recover
// that distinction without changing the typed request shape.
func harborOsidPresence(jsonB64 string) (present bool, err error) {
	var fields map[string]json.RawMessage
	if err := commons.JsonB64Decode(jsonB64, &fields); err != nil {
		return false, err
	}
	raw, ok := fields["harborOsid"]
	if !ok {
		return false, nil
	}
	// Confirm the present value actually decodes as a string or null; any
	// other JSON type is a malformed request, not a legacy omission.
	var v *string
	if err := json.Unmarshal(raw, &v); err != nil {
		return true, err
	}
	return true, nil
}

func requestAuthorizationClassification(req CreateCLIDeployTokenRequest) (string, error) {
	classification := req.AuthorizationClassification
	if classification == "" && req.AuthorizationBinding == nil {
		classification = datastore.AuthorizationLegacy
	}
	switch classification {
	case datastore.AuthorizationLegacy:
		if req.AuthorizationBinding != nil {
			return "", fmt.Errorf("legacy authorization cannot include a scoped binding")
		}
		return classification, nil
	case datastore.AuthorizationScoped:
		if req.Purpose != "" && req.Purpose != datastore.PurposeUpload {
			return "", fmt.Errorf("scoped upload authorization requires an empty purpose or upload")
		}
		if req.AuthorizationBinding == nil {
			return "", fmt.Errorf("scoped authorization requires a binding")
		}
		if req.CliUploadID == "" {
			return "", fmt.Errorf("scoped authorization requires cliUploadId")
		}
		return classification, nil
	default:
		return "", fmt.Errorf("unknown authorization classification")
	}
}
