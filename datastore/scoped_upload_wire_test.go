package datastore_test

import (
	"testing"
	"time"

	"github.com/runos-official/clusteragent/agentstream/instructions"
	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/datastore"
)

func TestScopedUploadWireMintAndLookup(t *testing.T) {
	datastore.SetupBuildTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	const jobID = "00000000-0000-4000-8000-000000000005"
	binding := datastore.UploadAuthorityBinding{
		Version: datastore.UploadAuthorityBindingVersion, BindingID: "00000000-0000-4000-8000-000000000001",
		Credential: datastore.CredentialReference{Version: datastore.CredentialReferenceVersion, CredentialType: "account_api_key", CredentialID: "key-fixture", PrincipalKind: "account_api_key", PrincipalID: "key-fixture", CustodialAccountID: "accta", LegacyClassification: "none"},
		AccountID:  "accta", ClusterID: "abc", AppID: "m95nx", Action: "apps.deploy", Scope: "acct/accta/cluster/abc/ns/app-m95nx/app/m95nx",
		UploadID: "00000000-0000-4000-8000-000000000002", Purpose: datastore.PurposeUpload, DecisionID: "decision-fixture", PolicyVersion: 1,
		CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(5 * time.Minute).Format(time.RFC3339),
	}
	wire, err := commons.JsonB64Encode(instructions.CreateCLIDeployTokenRequest{OSID: "app-m95nx", CliUploadID: binding.UploadID, AuthorizationClassification: datastore.AuthorizationScoped, AuthorizationBinding: &binding, DurableJobID: jobID})
	if err != nil {
		t.Fatal(err)
	}
	_, payload, err := instructions.CreateCLIDeployToken(wire)
	if err != nil {
		t.Fatal(err)
	}
	var reply instructions.CreateCLIDeployTokenResponse
	if err := commons.JsonB64Decode(payload, &reply); err != nil {
		t.Fatal(err)
	}
	if !reply.Success || reply.ScopedAck == nil || reply.ScopedAck.AppID != binding.AppID || reply.ScopedAck.Version != instructions.ScopedUploadAckVersion {
		t.Fatalf("mint failed or returned incorrect ACK: %s", reply.Message)
	}
	row, err := datastore.GetUploadableToken(reply.Token)
	if err != nil {
		t.Fatal(err)
	}
	if row.DeployConfig != "app-m95nx:"+binding.UploadID || row.AuthorizationBinding == nil || *row.AuthorizationBinding != binding || row.DurableJobID != jobID {
		t.Fatal("durable upload lost runtime OSID or scoped authority")
	}
}
