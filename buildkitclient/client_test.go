package buildkitclient

import (
	"strings"
	"testing"
	"time"
)

// TestBuildTimeout pins the per-build timeout resolution (obj-59 S107):
// unset -> generous default; a valid positive duration overrides; an invalid
// or non-positive value falls back to the default. This is the bound that
// turns a wedged/unreachable daemon into a failed (retryable) job instead of
// one stuck at JobStatusBusy forever.
func TestBuildTimeout(t *testing.T) {
	if got := buildTimeout(); got != defaultBuildTimeout {
		t.Errorf("unset: got %s, want default %s", got, defaultBuildTimeout)
	}

	t.Setenv("BUILDKIT_BUILD_TIMEOUT", "90m")
	if got := buildTimeout(); got != 90*time.Minute {
		t.Errorf("override: got %s, want 90m", got)
	}

	for _, bad := range []string{"nonsense", "0", "-5m", ""} {
		t.Setenv("BUILDKIT_BUILD_TIMEOUT", bad)
		if got := buildTimeout(); got != defaultBuildTimeout {
			t.Errorf("invalid %q: got %s, want default %s", bad, got, defaultBuildTimeout)
		}
	}
}

func TestStripRegistryProtocol(t *testing.T) {
	cases := map[string]string{
		"https://harbor.example.com":  "harbor.example.com",
		"http://harbor.example.com":   "harbor.example.com",
		"harbor.example.com":          "harbor.example.com",
		"https://harbor.example.com/": "harbor.example.com/",
	}
	for in, want := range cases {
		if got := StripRegistryProtocol(in); got != want {
			t.Errorf("StripRegistryProtocol(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTargetImageRefs pins the app-less build-image ref construction (obj-47):
// every tag yields a fully-qualified `<registry>/runos-apps/<repo>:<tag>` ref,
// and the project is always runos-apps regardless of input.
func TestTargetImageRefs(t *testing.T) {
	refs := TargetImageRefs("harbor.example.com", "xgmi-vm-workspace", []string{"latest", "v1"})
	want := []string{
		"harbor.example.com/runos-apps/xgmi-vm-workspace:latest",
		"harbor.example.com/runos-apps/xgmi-vm-workspace:v1",
	}
	if len(refs) != len(want) {
		t.Fatalf("got %d refs, want %d: %v", len(refs), len(want), refs)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Errorf("ref[%d] = %q, want %q", i, refs[i], want[i])
		}
	}
	for _, r := range refs {
		if !strings.Contains(r, "/"+HarborProject+"/") {
			t.Errorf("ref %q is not under the fixed project %q", r, HarborProject)
		}
	}
}

func TestTargetImageRefs_Empty(t *testing.T) {
	if refs := TargetImageRefs("harbor.example.com", "repo", nil); len(refs) != 0 {
		t.Fatalf("expected no refs for no tags, got %v", refs)
	}
}

// TestImageOutputArg pins the buildctl --output construction (obj-47 S81):
// a single ref stays unquoted (byte-for-byte unchanged for app deploys), and
// a multi-ref name list is CSV-quoted so BuildKit doesn't split it on the
// comma between refs.
func TestImageOutputArg(t *testing.T) {
	single := imageOutputArg([]string{"reg/runos-apps/app:sha1"})
	if single != "type=image,name=reg/runos-apps/app:sha1,push=true" {
		t.Errorf("single-ref output = %q, want unquoted form", single)
	}

	multi := imageOutputArg([]string{
		"reg/runos-apps/tool:latest",
		"reg/runos-apps/tool:v1",
	})
	want := `type=image,"name=reg/runos-apps/tool:latest,reg/runos-apps/tool:v1",push=true`
	if multi != want {
		t.Errorf("multi-ref output = %q, want %q", multi, want)
	}
	// The name field must be quoted (so BuildKit's CSV parser keeps both refs
	// in one field) and the comma between refs must sit inside the quotes.
	if !strings.Contains(multi, `"name=`) || !strings.HasSuffix(multi, `",push=true`) {
		t.Errorf("multi-ref output %q is not CSV-quoted around the name list", multi)
	}
}
