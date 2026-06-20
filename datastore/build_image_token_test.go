package datastore

import (
	"testing"
	"time"
)

// TestBuildImageToken_RoundTrip pins the app-less build-image token (obj-47):
// CreateBuildImageToken persists the explicit target repo + tag list and an
// app-less identity, and GetUploadableToken reads them back with
// Purpose=build_image.
func TestBuildImageToken_RoundTrip(t *testing.T) {
	setupTestDB(t)

	args := []BuildArg{
		{Key: "BASE_TAG", Value: "3.20", Source: "cli"},
	}
	expires := time.Now().Add(5 * time.Minute)
	tags := []string{"latest", "v1"}
	if err := CreateBuildImageToken("tok-build", "upload-xyz", "xgmi-vm-workspace", tags, "docker/Dockerfile", args, expires); err != nil {
		t.Fatalf("CreateBuildImageToken: %v", err)
	}

	tok, err := GetUploadableToken("tok-build")
	if err != nil {
		t.Fatalf("GetUploadableToken: %v", err)
	}
	if tok.Purpose != PurposeBuildImage {
		t.Fatalf("purpose = %q, want %q", tok.Purpose, PurposeBuildImage)
	}
	if tok.BuildTarget == nil {
		t.Fatalf("BuildTarget is nil, want repo+tags")
	}
	if tok.BuildTarget.Repo != "xgmi-vm-workspace" {
		t.Fatalf("repo = %q, want xgmi-vm-workspace", tok.BuildTarget.Repo)
	}
	if len(tok.BuildTarget.Tags) != 2 || tok.BuildTarget.Tags[0] != "latest" || tok.BuildTarget.Tags[1] != "v1" {
		t.Fatalf("tags = %v, want [latest v1]", tok.BuildTarget.Tags)
	}
	if tok.Dockerfile != "docker/Dockerfile" {
		t.Fatalf("dockerfile = %q, want docker/Dockerfile", tok.Dockerfile)
	}
	if len(tok.BuildArgs) != 1 || tok.BuildArgs[0] != args[0] {
		t.Fatalf("build args round-trip mismatch: got %+v want %+v", tok.BuildArgs, args)
	}
	// deploy_config encodes the synthetic app-less identity as "<repo>:<uploadID>"
	// so the shared upload handler's osid:uploadID split yields osid=repo.
	if tok.DeployConfig != "xgmi-vm-workspace:upload-xyz" {
		t.Fatalf("deploy_config = %q, want xgmi-vm-workspace:upload-xyz", tok.DeployConfig)
	}
}

// TestBuildImageToken_PurposeIsolation confirms a build-image token is not
// returned by the app-deploy-only GetUploadToken lookup (and vice versa), so
// the two flows can't be confused at the upload endpoint.
func TestBuildImageToken_PurposeIsolation(t *testing.T) {
	setupTestDB(t)

	expires := time.Now().Add(5 * time.Minute)
	if err := CreateBuildImageToken("tok-bi", "up-1", "tool", []string{"latest"}, "", nil, expires); err != nil {
		t.Fatalf("CreateBuildImageToken: %v", err)
	}
	if _, err := GetUploadToken("tok-bi"); err == nil {
		t.Fatalf("GetUploadToken returned a build_image token, want not-found")
	}
	if _, err := GetUploadableToken("tok-bi"); err != nil {
		t.Fatalf("GetUploadableToken should find a build_image token: %v", err)
	}

	// An app-deploy token is reachable via GetUploadableToken too (shared endpoint).
	if err := CreateUploadToken("tok-app", "osid-1", "up-2", "", nil, expires); err != nil {
		t.Fatalf("CreateUploadToken: %v", err)
	}
	app, err := GetUploadableToken("tok-app")
	if err != nil {
		t.Fatalf("GetUploadableToken(app): %v", err)
	}
	if app.Purpose != PurposeUpload {
		t.Fatalf("app token purpose = %q, want %q", app.Purpose, PurposeUpload)
	}
	if app.BuildTarget != nil {
		t.Fatalf("app token BuildTarget = %+v, want nil", app.BuildTarget)
	}
}
