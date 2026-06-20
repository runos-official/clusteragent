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

// CreateCLIPullTokenRequest holds the request for creating a CLI pull token.
type CreateCLIPullTokenRequest struct {
	OSID          string `json:"osid"`
	CliUploadID   string `json:"cliUploadId"`
	ExpirySeconds int    `json:"expirySeconds,omitempty"`
}

// CreateCLIPullTokenResponse holds the response after creating a pull token.
type CreateCLIPullTokenResponse struct {
	Success     bool   `json:"success"`
	Token       string `json:"token,omitempty"`
	ExpiresAt   string `json:"expiresAt,omitempty"`
	DownloadURL string `json:"downloadUrl,omitempty"`
	Message     string `json:"message,omitempty"`
}

// CreateCLIPullToken handles the CREATE_CLI_PULL_TOKEN instruction.
func CreateCLIPullToken(jsonB64 string) (string, string, error) {
	log.Printf("CreateCLIPullToken called")

	var req CreateCLIPullTokenRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		log.Printf("Error decoding message: %s", err)
		return "", "", err
	}

	if req.OSID == "" || req.CliUploadID == "" {
		response := CreateCLIPullTokenResponse{
			Success: false,
			Message: "osid and cliUploadId are required",
		}
		jsonResponse, _ := commons.JsonB64Encode(response)
		return "CREATE_CLI_PULL_TOKEN_RESPONSE", jsonResponse, nil
	}

	expirySeconds := req.ExpirySeconds
	if expirySeconds <= 0 {
		expirySeconds = defaultExpirySeconds
	}
	if expirySeconds > maxExpirySeconds {
		expirySeconds = maxExpirySeconds
	}
	expiresAt := time.Now().Add(time.Duration(expirySeconds) * time.Second)

	tokenBuf := make([]byte, tokenBytes)
	if _, err := rand.Read(tokenBuf); err != nil {
		log.Printf("Error generating token: %v", err)
		response := CreateCLIPullTokenResponse{
			Success: false,
			Message: "Failed to generate secure token",
		}
		jsonResponse, _ := commons.JsonB64Encode(response)
		return "CREATE_CLI_PULL_TOKEN_RESPONSE", jsonResponse, nil
	}
	token := hex.EncodeToString(tokenBuf)

	if err := datastore.CreatePullToken(token, req.OSID, req.CliUploadID, expiresAt); err != nil {
		log.Printf("Error storing pull token: %v", err)
		response := CreateCLIPullTokenResponse{
			Success: false,
			Message: "Failed to store token",
		}
		jsonResponse, _ := commons.JsonB64Encode(response)
		return "CREATE_CLI_PULL_TOKEN_RESPONSE", jsonResponse, nil
	}

	log.Printf("Created CLI pull token for osid=%s cliUploadID=%s, expires=%s", req.OSID, req.CliUploadID, expiresAt.Format(time.RFC3339))

	response := CreateCLIPullTokenResponse{
		Success:     true,
		Token:       token,
		ExpiresAt:   expiresAt.Format(time.RFC3339),
		DownloadURL: fmt.Sprintf("/cli-pull/%s", token),
		Message:     "CLI pull token created successfully",
	}

	jsonResponse, err := commons.JsonB64Encode(response)
	if err != nil {
		log.Printf("Error encoding response: %v", err)
		return "", "", err
	}

	return "CREATE_CLI_PULL_TOKEN_RESPONSE", jsonResponse, nil
}
