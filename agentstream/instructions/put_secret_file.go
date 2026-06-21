package instructions

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/runos-official/clusteragent/commons"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// putSecretK8sTimeout bounds the Secret create/update API calls so a slow or
// unreachable API server can't hang the handler indefinitely.
const putSecretK8sTimeout = 30 * time.Second

// secretAlreadyExists reports whether a Secret Create error means the secret
// is already present (and so the agent should fall back to Update). It exists
// as a tiny pure helper so the create-vs-update branch can be unit-tested
// without a live cluster; it must key on the typed k8s error, not a substring
// of err.Error().
func secretAlreadyExists(err error) bool {
	return k8serrors.IsAlreadyExists(err)
}

type putSecretFileRequest struct {
	FileContents   string `json:"fileContents"`   // base64 encoded file contents
	Filename       string `json:"filename"`       // filename to use as secret key
	ExpectedSHA256 string `json:"expectedSHA256"` // SHA256 hash of decoded contents
	Namespace      string `json:"namespace"`      // Kubernetes namespace
	SecretName     string `json:"secretName"`     // Name of the secret to create/update
}

type putSecretFileResponse struct {
	Success    bool   `json:"success"`
	SizeBytes  int    `json:"sizeBytes"`
	SecretName string `json:"secretName,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Message    string `json:"message,omitempty"`
}

func PutSecretFile(jsonB64 string) (string, string, error) {
	var req putSecretFileRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		log.Printf("Error decoding message: %s", err)
		return "", "", err
	}

	// Log metadata only. The request payload carries the file's secret
	// contents (base64), so we must never log the raw payload or the decoded
	// bytes. Namespace + secret name + filename are safe identifiers.
	log.Printf("PutSecretFile called - namespace=%s secret=%s filename=%s", req.Namespace, req.SecretName, req.Filename)

	// Validate required fields
	if req.FileContents == "" {
		return "", "", fmt.Errorf("fileContents is required")
	}
	if req.ExpectedSHA256 == "" {
		return "", "", fmt.Errorf("expectedSHA256 is required")
	}
	if req.Namespace == "" {
		return "", "", fmt.Errorf("namespace is required")
	}
	if req.Filename == "" {
		return "", "", fmt.Errorf("filename is required")
	}
	if req.SecretName == "" {
		return "", "", fmt.Errorf("secretName is required")
	}

	// Decode base64 contents
	decodedContents, err := base64.StdEncoding.DecodeString(req.FileContents)
	if err != nil {
		log.Printf("Error decoding base64 file contents: %v", err)
		response := putSecretFileResponse{
			Success: false,
			Message: "Invalid base64 encoding",
		}
		jsonResponse, _ := commons.JsonB64Encode(response)
		return "PUT_SECRET_FILE_RESPONSE", jsonResponse, nil
	}

	// Check size limit (32KB)
	if len(decodedContents) > 32*1024 {
		log.Printf("File size exceeds 32KB limit: %d bytes", len(decodedContents))
		response := putSecretFileResponse{
			Success: false,
			Message: "File size exceeds 32KB limit",
		}
		jsonResponse, _ := commons.JsonB64Encode(response)
		return "PUT_SECRET_FILE_RESPONSE", jsonResponse, nil
	}

	// Validate SHA256
	actualHash := sha256.Sum256(decodedContents)
	actualHashString := hex.EncodeToString(actualHash[:])

	if actualHashString != req.ExpectedSHA256 {
		log.Printf("SHA256 mismatch. Expected: %s, Actual: %s", req.ExpectedSHA256, actualHashString)
		response := putSecretFileResponse{
			Success:   false,
			SizeBytes: len(decodedContents),
			Message:   "SHA256 hash validation failed",
		}
		jsonResponse, _ := commons.JsonB64Encode(response)
		return "PUT_SECRET_FILE_RESPONSE", jsonResponse, nil
	}

	// Get in-cluster Kubernetes config
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("Failed to get in-cluster config: %v", err)
		response := putSecretFileResponse{
			Success: false,
			Message: "Failed to get Kubernetes config",
		}
		jsonResponse, _ := commons.JsonB64Encode(response)
		return "PUT_SECRET_FILE_RESPONSE", jsonResponse, nil
	}

	clientset, err := k8s.NewForConfig(config)
	if err != nil {
		log.Printf("Failed to create Kubernetes clientset: %v", err)
		response := putSecretFileResponse{
			Success: false,
			Message: "Failed to create Kubernetes client",
		}
		jsonResponse, _ := commons.JsonB64Encode(response)
		return "PUT_SECRET_FILE_RESPONSE", jsonResponse, nil
	}

	// Bound the k8s write so a slow/unreachable API server can't hang the
	// handler (and the stream worker behind it) indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), putSecretK8sTimeout)
	defer cancel()

	// Create the secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.SecretName,
			Namespace: req.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "runos",
				"runos.io/secret-type":         "file",
			},
			Annotations: map[string]string{
				"runos.io/original-filename": req.Filename,
				"runos.io/sha256":            actualHashString,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			req.Filename: decodedContents,
		},
	}

	// Try to create the secret
	_, err = clientset.CoreV1().Secrets(req.Namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		// If secret already exists, update it
		if secretAlreadyExists(err) {
			_, err = clientset.CoreV1().Secrets(req.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
			if err != nil {
				log.Printf("Error updating secret: %v", err)
				response := putSecretFileResponse{
					Success: false,
					Message: fmt.Sprintf("Failed to update secret: %v", err),
				}
				jsonResponse, _ := commons.JsonB64Encode(response)
				return "PUT_SECRET_FILE_RESPONSE", jsonResponse, nil
			}
			log.Printf("Successfully updated secret: %s/%s", req.Namespace, req.SecretName)
		} else {
			log.Printf("Error creating secret: %v", err)
			response := putSecretFileResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to create secret: %v", err),
			}
			jsonResponse, _ := commons.JsonB64Encode(response)
			return "PUT_SECRET_FILE_RESPONSE", jsonResponse, nil
		}
	} else {
		log.Printf("Successfully created secret: %s/%s", req.Namespace, req.SecretName)
	}

	response := putSecretFileResponse{
		Success:    true,
		SizeBytes:  len(decodedContents),
		SecretName: req.SecretName,
		Namespace:  req.Namespace,
		Message:    fmt.Sprintf("Secret created successfully in namespace %s", req.Namespace),
	}

	jsonResponse, err := commons.JsonB64Encode(response)
	if err != nil {
		log.Printf("Error encoding response: %v", err)
		return "", "", err
	}

	return "PUT_SECRET_FILE_RESPONSE", jsonResponse, nil
}
