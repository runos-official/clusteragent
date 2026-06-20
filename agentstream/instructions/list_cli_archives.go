package instructions

import (
	"context"
	"fmt"
	"log"

	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/harborclient"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const runosNamespace = "runos"

func loadHarborConfig(ctx context.Context) (harborclient.Config, error) {
	clientset, err := getK8sClientset()
	if err != nil {
		return harborclient.Config{}, fmt.Errorf("k8s clientset: %w", err)
	}

	cm, err := clientset.CoreV1().ConfigMaps(runosNamespace).Get(ctx, "buildkit", metav1.GetOptions{})
	if err != nil {
		return harborclient.Config{}, fmt.Errorf("get configmap buildkit: %w", err)
	}
	secret, err := clientset.CoreV1().Secrets(runosNamespace).Get(ctx, "buildkit", metav1.GetOptions{})
	if err != nil {
		return harborclient.Config{}, fmt.Errorf("get secret buildkit: %w", err)
	}

	return harborclient.Config{
		URL:      cm.Data["harbor-url"],
		Username: string(secret.Data["harbor-username"]),
		Password: string(secret.Data["harbor-password"]),
	}, nil
}

// ListCLIArchivesRequest holds the request for listing CLI archives.
type ListCLIArchivesRequest struct {
	OSID string `json:"osid"`
}

// ListCLIArchivesResponse holds the response listing archives stored in Harbor.
type ListCLIArchivesResponse struct {
	Success  bool                   `json:"success"`
	Archives []harborclient.Archive `json:"archives,omitempty"`
	Message  string                 `json:"message,omitempty"`
}

// ListCLIArchives handles the LIST_CLI_ARCHIVES instruction.
func ListCLIArchives(jsonB64 string) (string, string, error) {
	log.Printf("ListCLIArchives called")

	var req ListCLIArchivesRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		log.Printf("Error decoding message: %s", err)
		return "", "", err
	}

	if req.OSID == "" {
		response := ListCLIArchivesResponse{Success: false, Message: "osid is required"}
		jsonResponse, _ := commons.JsonB64Encode(response)
		return "LIST_CLI_ARCHIVES_RESPONSE", jsonResponse, nil
	}

	ctx := context.Background()

	harborCfg, err := loadHarborConfig(ctx)
	if err != nil {
		log.Printf("Error loading Harbor config: %v", err)
		response := ListCLIArchivesResponse{Success: false, Message: "Failed to load Harbor configuration"}
		jsonResponse, _ := commons.JsonB64Encode(response)
		return "LIST_CLI_ARCHIVES_RESPONSE", jsonResponse, nil
	}

	archives, err := harborclient.ListArchives(ctx, harborCfg, req.OSID)
	if err != nil {
		log.Printf("Error listing archives for osid=%s: %v", req.OSID, err)
		response := ListCLIArchivesResponse{Success: false, Message: err.Error()}
		jsonResponse, _ := commons.JsonB64Encode(response)
		return "LIST_CLI_ARCHIVES_RESPONSE", jsonResponse, nil
	}

	response := ListCLIArchivesResponse{Success: true, Archives: archives}
	jsonResponse, err := commons.JsonB64Encode(response)
	if err != nil {
		log.Printf("Error encoding response: %v", err)
		return "", "", err
	}
	return "LIST_CLI_ARCHIVES_RESPONSE", jsonResponse, nil
}
