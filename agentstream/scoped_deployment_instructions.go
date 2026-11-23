package agentstream

import (
	"errors"
	"log"

	"github.com/runos-official/clusteragent/agentstream/instructions"
	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/datastore"
)

const getScopedDeploymentCapabilities = "GET_SCOPED_DEPLOYMENT_CAPABILITIES"

// Older agents reject these instruction names before inspecting their payloads.
// The actual operations retain their established response types and target validation.
var scopedDeploymentHandlers = map[string]func(string) (string, string, error){
	"CREATE_SCOPED_CLI_DEPLOY_TOKEN": instructions.CreateCLIDeployToken,
	"SCOPED_VCS_FETCH_SOURCE":        instructions.VcsFetchSource,
	"SCOPED_VCS_BUILD":               instructions.VcsBuild,
}

func handleScopedDeploymentInstruction(req *l2sec.ToClusterAgent, lt *Lifetime) bool {
	handler, scoped := scopedDeploymentHandlers[req.Type]
	if !scoped && req.Type != getScopedDeploymentCapabilities {
		return false
	}
	if !lt.Live() || lt != CurrentLifetime() {
		return true
	}
	kind, body, err := scopedDeploymentReply(req, handler)
	if err != nil {
		kind = "ERROR"
		body, _ = commons.JsonB64Encode(map[string]string{"errMsg": err.Error()})
	}
	if lt != CurrentLifetime() {
		return true
	}
	if err := sendOnLifetime(lt, &l2sec.FromClusterAgent{Tag: req.Tag, Type: kind, JsonB64: body}); err != nil {
		log.Printf("Could not reply to scoped deployment instruction on its receiving stream: %v", err)
	}
	return true
}

func scopedDeploymentReply(req *l2sec.ToClusterAgent, handler func(string) (string, string, error)) (string, string, error) {
	if instructions.RequestJobStagePermit == nil || instructions.VcsBuildExecutor == nil {
		return "", "", errors.New("scoped deployment stage callbacks are unavailable")
	}
	if req.Type == getScopedDeploymentCapabilities {
		// This reports the agent's wiring. Each actual mutation still needs its
		// own fresh decision from the authority over an eligible connection.
		caps := struct {
			Version         string `json:"version"`
			StageRecheck    bool   `json:"stageRecheck"`
			ScopedUploadAck string `json:"scopedUploadAck"`
		}{"runos.scoped-deployment-capabilities.v2", true, instructions.ScopedUploadAckVersion}
		payload, err := commons.JsonB64Encode(caps)
		return getScopedDeploymentCapabilities + "_RESPONSE", payload, err
	}
	var envelope struct {
		Classification string `json:"authorizationClassification"`
	}
	if err := commons.JsonB64Decode(req.JsonB64, &envelope); err != nil || envelope.Classification != datastore.AuthorizationScoped {
		return "", "", errors.New("scoped classification is required for this deployment instruction")
	}
	return handler(req.JsonB64)
}
