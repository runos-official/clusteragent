package agentstream

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/runos-official/clusteragent/agentstream/instructions"
	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"github.com/runos-official/clusteragent/commons"
)

func TestScopedDeploymentInstructions(t *testing.T) {
	oldPermit, oldExecutor := instructions.RequestJobStagePermit, instructions.VcsBuildExecutor
	t.Cleanup(func() { instructions.RequestJobStagePermit, instructions.VcsBuildExecutor = oldPermit, oldExecutor })
	instructions.RequestJobStagePermit = func(context.Context, string, string) error { t.Fatal("preflight requested a permit"); return nil }
	instructions.VcsBuildExecutor = func(instructions.VcsBuildExecutorInput) { t.Fatal("validation started a build") }
	stream := withFakeStream(t)
	lt := CurrentLifetime()
	call := func(name string, fields map[string]any) *l2sec.FromClusterAgent {
		t.Helper()
		req := &l2sec.ToClusterAgent{Type: name, Tag: uuid.NewString(), JsonB64: b64(t, fields)}
		prior := len(stream.sent)
		handleInstruction(req, lt)
		if len(stream.sent) != prior+1 || stream.sent[prior].Tag != req.Tag {
			t.Fatal("missing correlated response")
		}
		return stream.sent[prior]
	}
	const get = "GET_SCOPED_DEPLOYMENT_CAPABILITIES"
	answer := call(get, map[string]any{})
	var caps struct {
		Version         string
		StageRecheck    bool
		ScopedUploadAck string
	}
	if err := commons.JsonB64Decode(answer.JsonB64, &caps); err != nil {
		t.Fatal(err)
	}
	if answer.Type != get+"_RESPONSE" || caps.Version != "runos.scoped-deployment-capabilities.v1" || !caps.StageRecheck || caps.ScopedUploadAck != "runos.scoped-upload-ack.v1" {
		t.Fatalf("unexpected capabilities: %v %+v", answer.Type, caps)
	}
	for kind, response := range map[string]string{
		"CREATE_SCOPED_CLI_DEPLOY_TOKEN": "CREATE_CLI_DEPLOY_TOKEN_RESPONSE", "SCOPED_VCS_FETCH_SOURCE": "VCS_FETCH_SOURCE_RESPONSE", "SCOPED_VCS_BUILD": "VCS_BUILD_RESPONSE",
	} {
		for _, fields := range []map[string]any{{}, {"authorizationClassification": "legacy"}, {"authorizationClassification": "unsupported"}} {
			reply := call(kind, fields)
			var msg struct{ ErrMsg string }
			if err := commons.JsonB64Decode(reply.JsonB64, &msg); err != nil {
				t.Fatal(err)
			}
			if reply.Type != "ERROR" || !strings.Contains(msg.ErrMsg, "scoped classification") {
				t.Fatalf("%s failed to require scoped classification: %s %s", kind, reply.Type, msg.ErrMsg)
			}
		}
		reply := call(kind, map[string]any{"authorizationClassification": "scoped"})
		var status struct {
			Success bool
			Message string
		}
		if err := commons.JsonB64Decode(reply.JsonB64, &status); err != nil {
			t.Fatal(err)
		}
		if reply.Type != response || status.Success || status.Message == "" {
			t.Fatalf("%s did not retain normal target validation", kind)
		}
	}
	instructions.RequestJobStagePermit = nil
	if reply := call(get, map[string]any{}); reply.Type != "ERROR" {
		t.Fatal("unwired stage callback advertised capability")
	}
	instructions.RequestJobStagePermit = func(context.Context, string, string) error { return nil }
	instructions.VcsBuildExecutor = nil
	if reply := call(get, map[string]any{}); reply.Type != "ERROR" {
		t.Fatal("missing VCS executor advertised capability")
	}
	replacement := withFakeStream(t)
	last := len(stream.sent)
	handleInstruction(&l2sec.ToClusterAgent{Type: get, Tag: uuid.NewString(), JsonB64: b64(t, map[string]any{})}, lt)
	lt.end()
	handleInstruction(&l2sec.ToClusterAgent{Type: get, Tag: uuid.NewString(), JsonB64: b64(t, map[string]any{})}, lt)
	if len(stream.sent) != last || len(replacement.sent) != 0 {
		t.Fatal("ended stream emitted a response or crossed into replacement")
	}
}
