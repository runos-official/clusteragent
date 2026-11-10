package instructions

import (
	"testing"
	"time"

	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/datastore"
)

func instructionBinding() datastore.UploadAuthorityBinding {
	now := time.Now().UTC().Truncate(time.Second)
	return datastore.UploadAuthorityBinding{
		Version:   datastore.UploadAuthorityBindingVersion,
		BindingID: "00000000-0000-4000-8000-000000000002",
		Credential: datastore.CredentialReference{
			Version:              datastore.CredentialReferenceVersion,
			CredentialType:       "account_api_key",
			CredentialID:         "key-fixture",
			PrincipalKind:        "account_api_key",
			PrincipalID:          "key-fixture",
			CustodialAccountID:   "accta",
			LegacyClassification: "none",
		},
		AccountID:     "accta",
		ClusterID:     "abc",
		AppID:         "appaa",
		Action:        "apps.deploy",
		Scope:         "acct/accta/cluster/abc/ns/default/app/appaa",
		UploadID:      "00000000-0000-4000-8000-000000000003",
		Purpose:       datastore.PurposeUpload,
		DecisionID:    "decision-fixture",
		PolicyVersion: 12,
		CreatedAt:     now.Format(time.RFC3339),
		ExpiresAt:     now.Add(5 * time.Minute).Format(time.RFC3339),
	}
}

func TestCreateCLIDeployToken_SelectsLegacyOrScopedStorage(t *testing.T) {
	originalLegacy := createUploadToken
	originalScoped := createScopedUploadToken
	t.Cleanup(func() {
		createUploadToken = originalLegacy
		createScopedUploadToken = originalScoped
	})

	t.Run("legacy omission", func(t *testing.T) {
		legacyCalls := 0
		scopedCalls := 0
		createUploadToken = func(string, string, string, string, []datastore.BuildArg, time.Time) error {
			legacyCalls++
			return nil
		}
		createScopedUploadToken = func(string, string, string, string, []datastore.BuildArg, time.Time, datastore.UploadAuthorityBinding) error {
			scopedCalls++
			return nil
		}
		payload, err := commons.JsonB64Encode(CreateCLIDeployTokenRequest{OSID: "appaa", CliUploadID: "legacy-upload"})
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		_, response, err := CreateCLIDeployToken(payload)
		if err != nil {
			t.Fatalf("CreateCLIDeployToken: %v", err)
		}
		var decoded CreateCLIDeployTokenResponse
		if err := commons.JsonB64Decode(response, &decoded); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if !decoded.Success || legacyCalls != 1 || scopedCalls != 0 {
			t.Fatalf("success=%v legacyCalls=%d scopedCalls=%d", decoded.Success, legacyCalls, scopedCalls)
		}
	})

	t.Run("explicit scoped binding", func(t *testing.T) {
		legacyCalls := 0
		scopedCalls := 0
		binding := instructionBinding()
		createUploadToken = func(string, string, string, string, []datastore.BuildArg, time.Time) error {
			legacyCalls++
			return nil
		}
		createScopedUploadToken = func(_ string, appID, uploadID, _ string, _ []datastore.BuildArg, _ time.Time, got datastore.UploadAuthorityBinding) error {
			scopedCalls++
			if appID != binding.AppID || uploadID != binding.UploadID || got.BindingID != binding.BindingID {
				t.Fatalf("scoped store received wrong target or binding: app=%q upload=%q binding=%q", appID, uploadID, got.BindingID)
			}
			return nil
		}
		payload, err := commons.JsonB64Encode(CreateCLIDeployTokenRequest{
			OSID:                        binding.AppID,
			CliUploadID:                 binding.UploadID,
			ExpirySeconds:               300,
			AuthorizationClassification: datastore.AuthorizationScoped,
			AuthorizationBinding:        &binding,
		})
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		_, response, err := CreateCLIDeployToken(payload)
		if err != nil {
			t.Fatalf("CreateCLIDeployToken: %v", err)
		}
		var decoded CreateCLIDeployTokenResponse
		if err := commons.JsonB64Decode(response, &decoded); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if !decoded.Success || legacyCalls != 0 || scopedCalls != 1 {
			t.Fatalf("success=%v legacyCalls=%d scopedCalls=%d message=%q", decoded.Success, legacyCalls, scopedCalls, decoded.Message)
		}
	})
}

func TestCreateCLIDeployToken_RejectsAmbiguousAuthorization(t *testing.T) {
	binding := instructionBinding()
	tests := []CreateCLIDeployTokenRequest{
		{OSID: binding.AppID, CliUploadID: binding.UploadID, AuthorizationBinding: &binding},
		{OSID: binding.AppID, CliUploadID: binding.UploadID, AuthorizationClassification: datastore.AuthorizationScoped},
		{OSID: binding.AppID, CliUploadID: binding.UploadID, AuthorizationClassification: datastore.AuthorizationLegacy, AuthorizationBinding: &binding},
		{OSID: binding.AppID, CliUploadID: binding.UploadID, AuthorizationClassification: "implicit", AuthorizationBinding: &binding},
		{Purpose: datastore.PurposeBuildImage, Repo: "fixture-repo", Tags: []string{"latest"}, AuthorizationClassification: datastore.AuthorizationScoped, AuthorizationBinding: &binding},
	}
	for i, req := range tests {
		payload, err := commons.JsonB64Encode(req)
		if err != nil {
			t.Fatalf("case %d encode request: %v", i, err)
		}
		_, response, err := CreateCLIDeployToken(payload)
		if err != nil {
			t.Fatalf("case %d CreateCLIDeployToken: %v", i, err)
		}
		var decoded CreateCLIDeployTokenResponse
		if err := commons.JsonB64Decode(response, &decoded); err != nil {
			t.Fatalf("case %d decode response: %v", i, err)
		}
		if decoded.Success {
			t.Fatalf("case %d accepted ambiguous authorization", i)
		}
	}
}

func TestCreateCLIDeployToken_ScopedPurposeAndExpiry(t *testing.T) {
	originalScoped := createScopedUploadToken
	t.Cleanup(func() { createScopedUploadToken = originalScoped })
	tests := []struct {
		name          string
		purpose       string
		expirySeconds int
		delay         time.Duration
		wantSuccess   bool
	}{
		{name: "default purpose", wantSuccess: true},
		{name: "fractional expiry", delay: time.Millisecond, wantSuccess: true},
		{name: "upload purpose", purpose: datastore.PurposeUpload, expirySeconds: 300, wantSuccess: true},
		{name: "pull purpose", purpose: datastore.PurposePull},
		{name: "unknown purpose", purpose: "other"},
		{name: "shorter request expiry", expirySeconds: 60},
		{name: "longer request expiry", expirySeconds: 600},
		{name: "negative request expiry", expirySeconds: -1},
		{name: "excessive request expiry", expirySeconds: 3601},
		{name: "delayed request", expirySeconds: 300, delay: time.Minute, wantSuccess: true},
		{name: "expired binding", delay: 6 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			binding := instructionBinding()
			createdAt := time.Now().UTC().Truncate(time.Second).Add(-tc.delay)
			binding.CreatedAt = createdAt.Format(time.RFC3339Nano)
			binding.ExpiresAt = createdAt.Add(5 * time.Minute).Format(time.RFC3339Nano)
			calls := 0
			createScopedUploadToken = func(_ string, _ string, _ string, _ string, _ []datastore.BuildArg, expiry time.Time, got datastore.UploadAuthorityBinding) error {
				calls++
				if expiry.Format(time.RFC3339Nano) != binding.ExpiresAt || got != binding {
					t.Errorf("storage changed binding or expiry: expiry=%s want=%s", expiry, binding.ExpiresAt)
				}
				return nil
			}
			payload, err := commons.JsonB64Encode(CreateCLIDeployTokenRequest{
				OSID: binding.AppID, CliUploadID: binding.UploadID, Purpose: tc.purpose,
				ExpirySeconds: tc.expirySeconds, AuthorizationClassification: datastore.AuthorizationScoped,
				AuthorizationBinding: &binding,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, response, err := CreateCLIDeployToken(payload)
			if err != nil {
				t.Fatal(err)
			}
			var decoded CreateCLIDeployTokenResponse
			if err := commons.JsonB64Decode(response, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Success != tc.wantSuccess || (calls == 1) != tc.wantSuccess {
				t.Fatalf("success=%v calls=%d wantSuccess=%v message=%q", decoded.Success, calls, tc.wantSuccess, decoded.Message)
			}
			if tc.wantSuccess && decoded.ExpiresAt != binding.ExpiresAt {
				t.Fatalf("response expiry=%s want=%s", decoded.ExpiresAt, binding.ExpiresAt)
			}
		})
	}
}
