package datastore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

const (
	AuthorizationLegacy = "legacy"
	AuthorizationScoped = "scoped"

	CredentialReferenceVersion    = "runos.credential-reference.v1"
	UploadAuthorityBindingVersion = "runos.upload-authority-binding.v1"
)

var (
	resourceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	publicIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)
	uuidPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	scopePattern      = regexp.MustCompile(`^acct/[a-z0-9][a-z0-9-]{0,62}(/cluster/[a-z0-9][a-z0-9-]{0,62}(/ns/[a-z0-9][a-z0-9-]{0,62}(/(svc|app)/[a-z0-9][a-z0-9-]{0,62})?|/node/[a-z0-9][a-z0-9-]{0,62})?|/(server|module)/[a-z0-9][a-z0-9-]{0,62}|/settings/account)?$`)
	newBearerPattern  = regexp.MustCompile(`runos_(pat|key)_[A-Za-z0-9][A-Za-z0-9_.:-]*\.[A-Za-z0-9_-]{43}`)
	roidBearerPattern = regexp.MustCompile(`rk_[A-Za-z0-9_-]{16,}`)
)

// CredentialReference is the public, verified credential identity from the
// scoped credential contract. The reference never contains a bearer secret.
type CredentialReference struct {
	Version              string `json:"version"`
	CredentialType       string `json:"credentialType"`
	CredentialID         string `json:"credentialId"`
	PrincipalKind        string `json:"principalKind"`
	PrincipalID          string `json:"principalId"`
	CustodialAccountID   string `json:"custodialAccountId,omitempty"`
	LegacyClassification string `json:"legacyClassification"`
}

// UploadAuthorityBinding is the immutable version 1 authorization binding.
// JobID must stay nil because queue admission creates separate job authority.
type UploadAuthorityBinding struct {
	Version       string              `json:"version"`
	BindingID     string              `json:"bindingId"`
	Credential    CredentialReference `json:"credential"`
	AccountID     string              `json:"accountId"`
	ClusterID     string              `json:"clusterId"`
	AppID         string              `json:"appId"`
	Action        string              `json:"action"`
	Scope         string              `json:"scope"`
	UploadID      string              `json:"uploadId"`
	JobID         *string             `json:"jobId"`
	Purpose       string              `json:"purpose"`
	DecisionID    string              `json:"decisionId"`
	PolicyVersion int64               `json:"policyVersion"`
	CreatedAt     string              `json:"createdAt"`
	ExpiresAt     string              `json:"expiresAt"`
}

// UnmarshalJSON enforces required fields that Go zero values cannot preserve.
func (b *UploadAuthorityBinding) UnmarshalJSON(data []byte) error {
	type bindingAlias UploadAuthorityBinding
	var decoded bindingAlias
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if err := requireJSONEnd(decoder); err != nil {
		return err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	required := []string{
		"version", "bindingId", "credential", "accountId", "clusterId",
		"appId", "action", "scope", "uploadId", "jobId", "purpose",
		"decisionId", "policyVersion", "createdAt", "expiresAt",
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return fmt.Errorf("authorization binding requires %s", name)
		}
	}
	if string(fields["jobId"]) != "null" {
		return fmt.Errorf("authorization binding jobId must be null")
	}
	*b = UploadAuthorityBinding(decoded)
	return nil
}

// UploadAuthorityTarget supplies target values that the current stage knows.
// Empty values are unavailable at that stage and are not compared.
type UploadAuthorityTarget struct {
	AccountID string
	ClusterID string
	AppID     string
	UploadID  string
	Purpose   string
}

// MatchTarget compares each available target value with the immutable binding.
func (b UploadAuthorityBinding) MatchTarget(target UploadAuthorityTarget) error {
	checks := []struct {
		name string
		got  string
		want string
	}{
		{name: "accountId", got: target.AccountID, want: b.AccountID},
		{name: "clusterId", got: target.ClusterID, want: b.ClusterID},
		{name: "appId", got: target.AppID, want: b.AppID},
		{name: "uploadId", got: target.UploadID, want: b.UploadID},
		{name: "purpose", got: target.Purpose, want: b.Purpose},
	}
	for _, check := range checks {
		if check.got != "" && check.got != check.want {
			return fmt.Errorf("authorization binding %s mismatch", check.name)
		}
	}
	return nil
}

// ValidateUploadAuthorityBinding applies the shared schema and semantic rules.
func ValidateUploadAuthorityBinding(binding UploadAuthorityBinding, target UploadAuthorityTarget, tokenExpiresAt time.Time) error {
	if binding.Version != UploadAuthorityBindingVersion {
		return fmt.Errorf("unsupported authorization binding version %q", binding.Version)
	}
	if !uuidPattern.MatchString(binding.BindingID) {
		return fmt.Errorf("invalid authorization binding bindingId")
	}
	if err := validateCredentialReference(binding.Credential); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"accountId": binding.AccountID,
		"clusterId": binding.ClusterID,
		"appId":     binding.AppID,
	} {
		if !resourceIDPattern.MatchString(value) {
			return fmt.Errorf("invalid authorization binding %s", name)
		}
	}
	if binding.Action != "apps.deploy" {
		return fmt.Errorf("invalid authorization binding action %q", binding.Action)
	}
	if len(binding.Scope) < 6 || len(binding.Scope) > 1024 || !scopePattern.MatchString(binding.Scope) {
		return fmt.Errorf("invalid authorization binding scope")
	}
	wantScopePrefix := "acct/" + binding.AccountID + "/cluster/" + binding.ClusterID + "/"
	if !strings.HasPrefix(binding.Scope, wantScopePrefix) || !strings.HasSuffix(binding.Scope, "/app/"+binding.AppID) {
		return fmt.Errorf("authorization binding scope does not match accountId, clusterId, and appId")
	}
	if !uuidPattern.MatchString(binding.UploadID) {
		return fmt.Errorf("invalid authorization binding uploadId")
	}
	if binding.JobID != nil {
		return fmt.Errorf("authorization binding jobId must be null")
	}
	if binding.Purpose != PurposeUpload {
		return fmt.Errorf("invalid authorization binding purpose %q", binding.Purpose)
	}
	if err := validatePublicID("decisionId", binding.DecisionID); err != nil {
		return err
	}
	if binding.PolicyVersion < 0 {
		return fmt.Errorf("invalid authorization binding policyVersion")
	}
	createdAt, err := parseContractTime("createdAt", binding.CreatedAt)
	if err != nil {
		return err
	}
	expiresAt, err := parseContractTime("expiresAt", binding.ExpiresAt)
	if err != nil {
		return err
	}
	if !expiresAt.After(createdAt) {
		return fmt.Errorf("authorization binding expiresAt must follow createdAt")
	}
	if expiresAt.After(createdAt.Add(time.Hour)) {
		return fmt.Errorf("authorization binding expiry exceeds one hour")
	}
	if tokenExpiresAt.IsZero() || tokenExpiresAt.After(expiresAt) {
		return fmt.Errorf("token expiry exceeds authorization binding expiry")
	}
	if err := binding.MatchTarget(target); err != nil {
		return err
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return fmt.Errorf("marshal authorization binding: %w", err)
	}
	if containsBearerSecret(string(encoded)) {
		return fmt.Errorf("authorization binding contains a bearer secret")
	}
	return nil
}

func validateCredentialReference(ref CredentialReference) error {
	if ref.Version != CredentialReferenceVersion {
		return fmt.Errorf("unsupported credential reference version %q", ref.Version)
	}
	if err := validatePublicID("credentialId", ref.CredentialID); err != nil {
		return err
	}
	if err := validatePublicID("principalId", ref.PrincipalID); err != nil {
		return err
	}

	type expectedReference struct {
		principal string
		legacy    string
		custodian bool
	}
	expected := map[string]expectedReference{
		"personal_pat":                  {principal: "person", legacy: "none"},
		"account_api_key":               {principal: "account_api_key", legacy: "none", custodian: true},
		"legacy_conductor_personal_pat": {principal: "person", legacy: "conductor_personal"},
		"legacy_conductor_account_key":  {principal: "account_api_key", legacy: "conductor_account_role", custodian: true},
		"legacy_roid_personal_pat":      {principal: "person", legacy: "roid_personal"},
		"legacy_roid_account_key":       {principal: "account_api_key", legacy: "roid_account_role", custodian: true},
	}
	want, ok := expected[ref.CredentialType]
	if !ok {
		return fmt.Errorf("invalid credentialType %q", ref.CredentialType)
	}
	if ref.PrincipalKind != want.principal {
		return fmt.Errorf("invalid credential reference principalKind")
	}
	if ref.LegacyClassification != want.legacy {
		return fmt.Errorf("invalid credential reference legacyClassification")
	}
	if want.custodian {
		if !resourceIDPattern.MatchString(ref.CustodialAccountID) {
			return fmt.Errorf("invalid credential reference custodialAccountId")
		}
	} else if ref.CustodialAccountID != "" {
		return fmt.Errorf("credential reference custodialAccountId is not allowed")
	}
	return nil
}

func validatePublicID(name, value string) error {
	if len(value) < 1 || len(value) > 128 || !publicIDPattern.MatchString(value) {
		return fmt.Errorf("invalid credential reference %s", name)
	}
	if strings.Contains(value, "runos_pat_") || strings.Contains(value, "runos_key_") || strings.HasPrefix(value, "rk_") {
		return fmt.Errorf("invalid credential reference %s", name)
	}
	return nil
}

func parseContractTime(name, value string) (time.Time, error) {
	if !strings.HasSuffix(value, "Z") {
		return time.Time{}, fmt.Errorf("authorization binding %s must use UTC RFC 3339", name)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid authorization binding %s: %w", name, err)
	}
	return parsed, nil
}

func containsBearerSecret(value string) bool {
	return newBearerPattern.MatchString(value) || roidBearerPattern.MatchString(value)
}

func decodeUploadAuthorityBinding(value string) (UploadAuthorityBinding, error) {
	var binding UploadAuthorityBinding
	decoder := json.NewDecoder(bytes.NewBufferString(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil {
		return binding, fmt.Errorf("decode authorization binding: %w", err)
	}
	if err := requireJSONEnd(decoder); err != nil {
		return binding, fmt.Errorf("decode authorization binding: %w", err)
	}
	return binding, nil
}

func requireJSONEnd(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing data")
		}
		return err
	}
	return nil
}
