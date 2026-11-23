package instructions

import (
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/datastore"
)

func TestScopedUploadAcknowledgement(t *testing.T) {
	previousScoped, previousLegacy := createScopedUploadToken, createUploadToken
	t.Cleanup(func() { createScopedUploadToken, createUploadToken = previousScoped, previousLegacy })
	binding := instructionBinding()
	request := CreateCLIDeployTokenRequest{
		OSID: "app-" + binding.AppID, CliUploadID: binding.UploadID, DurableJobID: "00000000-0000-4000-8000-000000000005",
		AuthorizationClassification: datastore.AuthorizationScoped, AuthorizationBinding: &binding,
	}
	var saved datastore.UploadAuthorityBinding
	var saveErr error
	createScopedUploadToken = func(_ string, _ string, _ string, _ string, _ []datastore.BuildArg, _ time.Time, b datastore.UploadAuthorityBinding, _ string) error {
		if saveErr == nil {
			saved = b
		}
		return saveErr
	}
	createUploadToken = func(string, string, string, string, []datastore.BuildArg, time.Time) error { return nil }
	mint := func(request CreateCLIDeployTokenRequest, wantSuccess bool) map[string]string {
		t.Helper()
		encoded, err := commons.JsonB64Encode(request)
		if err != nil {
			t.Fatal(err)
		}
		_, response, err := CreateCLIDeployToken(encoded)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Success   bool
			ScopedAck map[string]string
		}
		if err := commons.JsonB64Decode(response, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Success != wantSuccess {
			t.Fatalf("mint success = %t, want %t", decoded.Success, wantSuccess)
		}
		if decoded.ScopedAck == nil {
			var fields map[string]any
			if err := commons.JsonB64Decode(response, &fields); err != nil {
				t.Fatal(err)
			}
			if _, exists := fields["scopedAck"]; exists {
				t.Fatal("absent acknowledgement must omit the wire field")
			}
		}
		return decoded.ScopedAck
	}
	got := mint(request, true)
	want := map[string]string{"version": "runos.scoped-upload-ack.v2", "bindingId": saved.BindingID, "aid": saved.AccountID, "cid": saved.ClusterID, "appId": saved.AppID, "uploadId": saved.UploadID}
	if !maps.Equal(got, want) {
		t.Fatalf("scoped acknowledgement = %v, want %v", got, want)
	}
	saveErr = errors.New("storage unavailable")
	if mint(request, false) != nil {
		t.Fatal("failed insert returned an acknowledgement")
	}
	saveErr = nil
	for _, osid := range []string{binding.AppID, "svc-" + binding.AppID, "app-other", "app-app-" + binding.AppID, "app-", "app-APPAA"} {
		request.OSID = osid
		if mint(request, false) != nil {
			t.Fatalf("invalid OSID %q returned an acknowledgement", osid)
		}
	}
	request.OSID = "app-" + binding.AppID
	binding.Scope = "acct/accta/cluster/abc/ns/app-other/app/" + binding.AppID
	if mint(request, false) != nil {
		t.Fatal("foreign namespace returned an acknowledgement")
	}
	if mint(CreateCLIDeployTokenRequest{OSID: binding.AppID}, true) != nil {
		t.Fatal("legacy mint returned a scoped acknowledgement")
	}
}
