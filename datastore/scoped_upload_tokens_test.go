package datastore

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

func testUploadAuthorityBinding(now time.Time) UploadAuthorityBinding {
	return UploadAuthorityBinding{
		Version:   UploadAuthorityBindingVersion,
		BindingID: "00000000-0000-4000-8000-000000000002",
		Credential: CredentialReference{
			Version:              CredentialReferenceVersion,
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
		Purpose:       PurposeUpload,
		DecisionID:    "decision-fixture",
		PolicyVersion: 12,
		CreatedAt:     now.Format(time.RFC3339),
		ExpiresAt:     now.Add(5 * time.Minute).Format(time.RFC3339),
	}
}

func TestScopedUploadToken_RoundTripAndReplayMetadata(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	binding := testUploadAuthorityBinding(now)
	const raw = "scoped-upload-token-fixture"

	if err := CreateScopedUploadToken(raw, "appaa", binding.UploadID, "Dockerfile", nil, now.Add(5*time.Minute), binding); err != nil {
		t.Fatalf("CreateScopedUploadToken: %v", err)
	}

	got, err := GetUploadToken(raw)
	if err != nil {
		t.Fatalf("GetUploadToken: %v", err)
	}
	if got.AuthorizationClassification != AuthorizationScoped {
		t.Fatalf("classification = %q, want %q", got.AuthorizationClassification, AuthorizationScoped)
	}
	if got.AuthorizationBinding == nil {
		t.Fatal("authorization binding is nil")
	}
	if encoded, _ := json.Marshal(got.AuthorizationBinding); !strings.Contains(string(encoded), binding.BindingID) {
		t.Fatalf("binding does not contain bindingId %q: %s", binding.BindingID, encoded)
	}

	var before UploadTokenModel
	if err := testDB(t).Where("token_hash = ?", hashToken(raw)).First(&before).Error; err != nil {
		t.Fatalf("read row before use: %v", err)
	}
	for name, value := range map[string]string{
		"token_hash":                   before.TokenHash,
		"deploy_config":                before.DeployConfig,
		"authorization_classification": before.AuthorizationClassification,
		"authorization_version":        before.AuthorizationBindingVersion,
		"authorization_binding":        before.AuthorizationBinding,
	} {
		if strings.Contains(value, raw) || containsBearerSecret(value) {
			t.Fatalf("stored column %s contains a raw token or credential secret", name)
		}
	}
	if err := MarkUploadTokenUsed(raw); err != nil {
		t.Fatalf("MarkUploadTokenUsed: %v", err)
	}
	var after UploadTokenModel
	if err := testDB(t).Where("token_hash = ?", hashToken(raw)).First(&after).Error; err != nil {
		t.Fatalf("read row after use: %v", err)
	}
	if !after.Used {
		t.Fatal("used state is false after consumption")
	}
	if after.AuthorizationBinding != before.AuthorizationBinding || after.AuthorizationClassification != before.AuthorizationClassification {
		t.Fatal("single-use mutation changed immutable authorization data")
	}
}

func TestUploadAuthorityBinding_ValidationMatrix(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	accountSecret := "runos_" + "key_00000000-0000-4000-8000-000000000001." + strings.Repeat("a", 43)
	personalSecret := "runos_" + "pat_00000000-0000-4000-8000-000000000001." + strings.Repeat("b", 43)
	legacySecret := "rk_" + strings.Repeat("c", 32)

	tests := []struct {
		name    string
		mutate  func(*UploadAuthorityBinding)
		target  UploadAuthorityTarget
		expires time.Time
		want    string
	}{
		{name: "unsupported version", mutate: func(b *UploadAuthorityBinding) { b.Version = "runos.upload-authority-binding.v99" }, want: "version"},
		{name: "account mismatch", target: UploadAuthorityTarget{AccountID: "acctb"}, want: "accountId"},
		{name: "cluster mismatch", target: UploadAuthorityTarget{ClusterID: "def"}, want: "clusterId"},
		{name: "app mismatch", target: UploadAuthorityTarget{AppID: "appbb"}, want: "appId"},
		{name: "upload mismatch", target: UploadAuthorityTarget{UploadID: "00000000-0000-4000-8000-000000000009"}, want: "uploadId"},
		{name: "action mismatch", mutate: func(b *UploadAuthorityBinding) { b.Action = "apps.logs.read" }, want: "action"},
		{name: "purpose mismatch", target: UploadAuthorityTarget{Purpose: PurposePull}, want: "purpose"},
		{name: "token expiry after binding", expires: now.Add(6 * time.Minute), want: "expiry"},
		{name: "account key bearer", mutate: func(b *UploadAuthorityBinding) { b.Credential.CredentialID = accountSecret }, want: "credentialId"},
		{name: "personal token bearer", mutate: func(b *UploadAuthorityBinding) { b.Credential.CredentialID = personalSecret }, want: "credentialId"},
		{name: "legacy key bearer", mutate: func(b *UploadAuthorityBinding) { b.Credential.CredentialID = legacySecret }, want: "credentialId"},
		{name: "non-null job", mutate: func(b *UploadAuthorityBinding) { value := "00000000-0000-4000-8000-000000000004"; b.JobID = &value }, want: "jobId"},
		{name: "negative policy", mutate: func(b *UploadAuthorityBinding) { b.PolicyVersion = -1 }, want: "policyVersion"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			binding := testUploadAuthorityBinding(now)
			if tc.mutate != nil {
				tc.mutate(&binding)
			}
			target := tc.target
			if target.AppID == "" {
				target.AppID = binding.AppID
			}
			if target.UploadID == "" {
				target.UploadID = binding.UploadID
			}
			if target.Purpose == "" {
				target.Purpose = PurposeUpload
			}
			expires := tc.expires
			if expires.IsZero() {
				expires = now.Add(5 * time.Minute)
			}
			err := ValidateUploadAuthorityBinding(binding, target, expires)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateUploadAuthorityBinding() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestUploadAuthorityBinding_RequiresExplicitNullAndPolicyVersion(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	binding := testUploadAuthorityBinding(now)
	encoded, err := json.Marshal(binding)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	for _, missing := range []string{"jobId", "policyVersion"} {
		copyFields := make(map[string]any, len(fields))
		for key, value := range fields {
			copyFields[key] = value
		}
		delete(copyFields, missing)
		malformed, err := json.Marshal(copyFields)
		if err != nil {
			t.Fatalf("marshal without %s: %v", missing, err)
		}
		var got UploadAuthorityBinding
		if err := json.Unmarshal(malformed, &got); err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("missing %s error = %v", missing, err)
		}
	}

	fields["secret"] = "redacted"
	malformed, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal extra secret field: %v", err)
	}
	var got UploadAuthorityBinding
	if err := json.Unmarshal(malformed, &got); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("extra secret field error = %v", err)
	}
}

func TestUploadToken_RejectsInconsistentAuthorizationRows(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	bindingJSON, err := json.Marshal(testUploadAuthorityBinding(now))
	if err != nil {
		t.Fatalf("marshal binding: %v", err)
	}

	rows := []struct {
		name           string
		classification string
		version        string
		binding        string
	}{
		{name: "scoped without binding", classification: AuthorizationScoped, version: UploadAuthorityBindingVersion},
		{name: "legacy with binding", classification: AuthorizationLegacy, version: UploadAuthorityBindingVersion, binding: string(bindingJSON)},
		{name: "unknown classification", classification: "implicit", version: UploadAuthorityBindingVersion, binding: string(bindingJSON)},
		{name: "unsupported stored version", classification: AuthorizationScoped, version: "runos.upload-authority-binding.v99", binding: string(bindingJSON)},
	}

	for i, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			raw := "inconsistent-token-" + string(rune('a'+i))
			row := UploadTokenModel{
				TokenHash:                   hashToken(raw),
				DeployConfig:                "appaa:00000000-0000-4000-8000-000000000003",
				ExpiresAt:                   now.Add(5 * time.Minute),
				Purpose:                     PurposeUpload,
				AuthorizationClassification: tc.classification,
				AuthorizationBindingVersion: tc.version,
				AuthorizationBinding:        tc.binding,
			}
			if err := testDB(t).Create(&row).Error; err != nil {
				t.Fatalf("create row: %v", err)
			}
			if _, err := GetUploadToken(raw); err == nil {
				t.Fatal("GetUploadToken accepted an inconsistent authorization row")
			}
		})
	}
}

func TestUploadToken_MigratesOldRowsToExplicitLegacy(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	gdb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		NamingStrategy: schema.NamingStrategy{TablePrefix: tablePrefix},
	})
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if err := gdb.Exec(`CREATE TABLE cluster_agent_upload_tokens (
		id integer PRIMARY KEY AUTOINCREMENT,
		token_hash text NOT NULL UNIQUE,
		deploy_config text NOT NULL,
		dockerfile text NOT NULL DEFAULT '',
		build_args text NOT NULL DEFAULT '[]',
		build_target text NOT NULL DEFAULT '',
		expires_at datetime NOT NULL,
		used numeric NOT NULL DEFAULT false,
		created_at datetime DEFAULT CURRENT_TIMESTAMP,
		purpose text NOT NULL DEFAULT 'upload'
	)`).Error; err != nil {
		t.Fatalf("create old table: %v", err)
	}
	oldRows := []struct {
		raw          string
		deployConfig string
		purpose      string
		buildTarget  string
	}{
		{raw: "old-upload-token", deployConfig: "appaa:legacy-upload", purpose: PurposeUpload},
		{raw: "old-pull-token", deployConfig: "appaa:legacy-pull", purpose: PurposePull},
		{raw: "old-build-token", deployConfig: "fixture-repo:legacy-build", purpose: PurposeBuildImage, buildTarget: `{"repo":"fixture-repo","tags":["latest"]}`},
	}
	for _, row := range oldRows {
		if err := gdb.Exec(`INSERT INTO cluster_agent_upload_tokens
			(token_hash, deploy_config, build_target, expires_at, purpose) VALUES (?, ?, ?, ?, ?)`,
			hashToken(row.raw), row.deployConfig, row.buildTarget, time.Now().Add(5*time.Minute), row.purpose).Error; err != nil {
			t.Fatalf("insert old %s row: %v", row.purpose, err)
		}
	}
	if err := gdb.AutoMigrate(&UploadTokenModel{}); err != nil {
		t.Fatalf("migrate old table: %v", err)
	}

	prev := setHandle(gdb)
	t.Cleanup(func() { setHandle(prev) })
	lookups := []struct {
		raw    string
		lookup func(string) (*UploadToken, error)
	}{
		{raw: "old-upload-token", lookup: GetUploadToken},
		{raw: "old-pull-token", lookup: GetPullToken},
		{raw: "old-build-token", lookup: GetUploadableToken},
	}
	for _, check := range lookups {
		got, err := check.lookup(check.raw)
		if err != nil {
			t.Fatalf("read migrated row %s: %v", check.raw, err)
		}
		if got.AuthorizationClassification != AuthorizationLegacy || got.AuthorizationBinding != nil {
			t.Fatalf("migrated authorization = (%q, %#v), want explicit legacy without binding", got.AuthorizationClassification, got.AuthorizationBinding)
		}
	}
}

func TestUploadToken_PurposeIsolationWithExplicitLegacy(t *testing.T) {
	setupTestDB(t)
	expires := time.Now().Add(5 * time.Minute)
	if err := CreateUploadToken("upload-token", "appaa", "upload-a", "", nil, expires); err != nil {
		t.Fatalf("create upload token: %v", err)
	}
	if err := CreatePullToken("pull-token", "appaa", "upload-b", expires); err != nil {
		t.Fatalf("create pull token: %v", err)
	}
	if err := CreateBuildImageToken("build-token", "upload-c", "fixture-repo", []string{"latest"}, "", nil, expires); err != nil {
		t.Fatalf("create build token: %v", err)
	}

	for _, raw := range []string{"upload-token", "pull-token", "build-token"} {
		var row UploadTokenModel
		if err := testDB(t).Where("token_hash = ?", hashToken(raw)).First(&row).Error; err != nil {
			t.Fatalf("read %s: %v", raw, err)
		}
		if row.AuthorizationClassification != AuthorizationLegacy || row.AuthorizationBinding != "" {
			t.Fatalf("%s authorization = (%q, %q), want explicit legacy", raw, row.AuthorizationClassification, row.AuthorizationBinding)
		}
	}
	if _, err := GetPullToken("upload-token"); err == nil {
		t.Fatal("pull lookup accepted an upload token")
	}
	if _, err := GetUploadToken("build-token"); err == nil {
		t.Fatal("upload lookup accepted a build token")
	}
	if _, err := GetUploadableToken("pull-token"); err == nil {
		t.Fatal("uploadable lookup accepted a pull token")
	}
}

func TestUploadToken_ConcurrentSingleUse(t *testing.T) {
	setupTestDB(t)
	const raw = "concurrent-single-use-token"
	if err := CreatePullToken(raw, "appaa", "upload-a", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("CreatePullToken: %v", err)
	}

	const attempts = 8
	var wg sync.WaitGroup
	results := make(chan error, attempts)
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- MarkUploadTokenUsed(raw)
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful uses = %d, want 1", successes)
	}
}
