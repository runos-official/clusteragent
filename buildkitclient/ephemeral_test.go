package buildkitclient

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestBuilderPodName pins the per-build pod naming: DNS-1123-safe, derived
// from the job id (unique per build attempt), capped at 63 chars with no
// trailing '-' the cap could expose.
func TestBuilderPodName(t *testing.T) {
	cases := map[string]string{
		// CLI deploy: uuid job id passes through lowercased.
		"3F2504E0-4f89-11D3-9A0C-0305E82C3301": "buildkit-build-3f2504e0-4f89-11d3-9a0c-0305e82c3301",
		// VCS deploy: sha-uuid8.
		"a1b2c3d4e5f60718293a4b5c6d7e8f9012345678-abcd1234": "buildkit-build-a1b2c3d4e5f60718293a4b5c6d7e8f9012345678-abcd123",
		// Unsafe runes become '-'.
		"job_id.with/chars": "buildkit-build-job-id-with-chars",
	}
	for in, want := range cases {
		got := builderPodName(in)
		if got != want {
			t.Errorf("builderPodName(%q) = %q, want %q", in, got, want)
		}
		if len(got) > 63 {
			t.Errorf("builderPodName(%q) length %d exceeds 63", in, len(got))
		}
		if strings.HasSuffix(got, "-") {
			t.Errorf("builderPodName(%q) = %q has trailing '-'", in, got)
		}
	}
}

// TestCacheRefForPushRef pins the registry-cache ref derivation: same
// repository as the push ref with a fixed `buildcache` tag, and a registry
// host:port colon is never mistaken for the tag separator.
func TestCacheRefForPushRef(t *testing.T) {
	cases := map[string]string{
		"harbor.example.com/runos-apps/app-ab12c:deadbeef":      "harbor.example.com/runos-apps/app-ab12c:buildcache",
		"harbor.example.com:8890/runos-apps/app-ab12c:deadbeef": "harbor.example.com:8890/runos-apps/app-ab12c:buildcache",
		"harbor.example.com:8890/runos-apps/untagged":           "harbor.example.com:8890/runos-apps/untagged:buildcache",
	}
	for in, want := range cases {
		if got := cacheRefForPushRef(in); got != want {
			t.Errorf("cacheRefForPushRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMaxConcurrentBuilds pins the concurrency-cap resolution: unset ->
// default; a valid positive integer overrides; invalid or non-positive values
// fall back to the default.
func TestMaxConcurrentBuilds(t *testing.T) {
	if got := maxConcurrentBuilds(); got != defaultMaxConcurrentBuilds {
		t.Errorf("unset: got %d, want default %d", got, defaultMaxConcurrentBuilds)
	}

	t.Setenv("BUILDKIT_MAX_CONCURRENT_BUILDS", "8")
	if got := maxConcurrentBuilds(); got != 8 {
		t.Errorf("override: got %d, want 8", got)
	}

	for _, bad := range []string{"nonsense", "0", "-2", ""} {
		t.Setenv("BUILDKIT_MAX_CONCURRENT_BUILDS", bad)
		if got := maxConcurrentBuilds(); got != defaultMaxConcurrentBuilds {
			t.Errorf("invalid %q: got %d, want default %d", bad, got, defaultMaxConcurrentBuilds)
		}
	}
}

// TestBuildkitImage pins the image resolution: unset -> pinned default;
// BUILDKIT_IMAGE overrides.
func TestBuildkitImage(t *testing.T) {
	if got := buildkitImage(); got != defaultBuildkitImage {
		t.Errorf("unset: got %q, want default %q", got, defaultBuildkitImage)
	}
	t.Setenv("BUILDKIT_IMAGE", "moby/buildkit:v0.31.0")
	if got := buildkitImage(); got != "moby/buildkit:v0.31.0" {
		t.Errorf("override: got %q, want moby/buildkit:v0.31.0", got)
	}
}

// TestParseBuildSettings pins the ConfigMap merge: valid values override the
// defaults per-field; missing or invalid values fall back per-field. The
// defaults must mirror conductor's DEFAULT_BUILD_SETTINGS (3 / 1000 / 2048).
func TestParseBuildSettings(t *testing.T) {
	def := defaultBuildSettings()
	if def.MaxConcurrentBuilds != 3 || def.CPURequestMc != 100 || def.CPULimitMc != 1000 || def.MemoryRequestMb != 256 || def.MemoryLimitMb != 2048 {
		t.Fatalf("defaults drifted from conductor's DEFAULT_BUILD_SETTINGS: %+v", def)
	}

	// Empty map -> defaults.
	if got := parseBuildSettings(map[string]string{}); got != def {
		t.Errorf("empty: got %+v, want defaults %+v", got, def)
	}

	// Full override.
	got := parseBuildSettings(map[string]string{
		"maxConcurrentBuilds": "8",
		"cpuRequestMc":        "250",
		"cpuLimitMc":          "2000",
		"memoryRequestMb":     "512",
		"memoryLimitMb":       "4096",
	})
	want := BuildSettings{MaxConcurrentBuilds: 8, CPURequestMc: 250, CPULimitMc: 2000, MemoryRequestMb: 512, MemoryLimitMb: 4096}
	if got != want {
		t.Errorf("full: got %+v, want %+v", got, want)
	}

	// Partial + invalid values fall back per-field.
	got = parseBuildSettings(map[string]string{
		"maxConcurrentBuilds": "nonsense",
		"cpuLimitMc":          "-5",
		"memoryLimitMb":       "8192",
	})
	want = def
	want.MemoryLimitMb = 8192
	if got != want {
		t.Errorf("partial: got %+v, want %+v", got, want)
	}

	// Out-of-band request > limit is clamped to the limit (K8s would
	// otherwise reject every build pod).
	got = parseBuildSettings(map[string]string{
		"cpuRequestMc":    "4000",
		"cpuLimitMc":      "2000",
		"memoryRequestMb": "9000",
		"memoryLimitMb":   "4096",
	})
	if got.CPURequestMc != 2000 || got.MemoryRequestMb != 4096 {
		t.Errorf("clamp: got %+v, want requests clamped to limits", got)
	}
}

// TestParseBuildSettingsEnvFallback pins that the env override still applies
// as the concurrency default when the ConfigMap doesn't set it.
func TestParseBuildSettingsEnvFallback(t *testing.T) {
	t.Setenv("BUILDKIT_MAX_CONCURRENT_BUILDS", "6")
	if got := parseBuildSettings(map[string]string{}); got.MaxConcurrentBuilds != 6 {
		t.Errorf("env fallback: got %d, want 6", got.MaxConcurrentBuilds)
	}
	// ConfigMap value wins over env.
	if got := parseBuildSettings(map[string]string{"maxConcurrentBuilds": "2"}); got.MaxConcurrentBuilds != 2 {
		t.Errorf("cm over env: got %d, want 2", got.MaxConcurrentBuilds)
	}
}

// TestBuilderPodMetadata pins the inspection metadata on per-build pods:
// identity in (sanitized) labels, free-text context in annotations, empty
// fields omitted.
func TestBuilderPodMetadata(t *testing.T) {
	meta := BuilderMeta{
		JobID:  "a1b2c3d4-uuid",
		OSID:   "app-mn4pq",
		Commit: "deadbee",
		Repo:   "https://github.com/acme/shop.git",
		Branch: "main",
		Images: []string{"harbor.example.com/runos-apps/app-mn4pq:deadbeef"},
	}
	pod := newBuilderPod("buildkit-build-a1b2c3d4-uuid", meta, defaultBuildSettings())

	wantLabels := map[string]string{
		"runos.com/osid":   "app-mn4pq",
		"runos.com/commit": "deadbee",
		"runos.com/job-id": "a1b2c3d4-uuid",
		"runos.com/type":   builderTypeLabel,
	}
	for k, want := range wantLabels {
		if got := pod.Labels[k]; got != want {
			t.Errorf("label %s = %q, want %q", k, got, want)
		}
	}
	wantAnnotations := map[string]string{
		"runos.com/repo":   "https://github.com/acme/shop.git",
		"runos.com/branch": "main",
		"runos.com/images": "harbor.example.com/runos-apps/app-mn4pq:deadbeef",
	}
	for k, want := range wantAnnotations {
		if got := pod.Annotations[k]; got != want {
			t.Errorf("annotation %s = %q, want %q", k, got, want)
		}
	}

	// Non-VCS build: repo/branch annotations omitted, osid/commit still set.
	cliPod := newBuilderPod("buildkit-build-x", BuilderMeta{JobID: "x", OSID: "app-ab12c", Commit: "x"}, defaultBuildSettings())
	if _, ok := cliPod.Annotations["runos.com/repo"]; ok {
		t.Error("repo annotation should be omitted when empty")
	}
	if _, ok := cliPod.Annotations["runos.com/branch"]; ok {
		t.Error("branch annotation should be omitted when empty")
	}
}

// TestSummarizePodState pins the failure-forensics line read before a failed
// build's pod is deleted: terminated containers surface exit code + reason
// (the OOMKilled / external-SIGKILL evidence), waiting containers surface the
// waiting reason.
func TestSummarizePodState(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "buildkitd",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"},
					},
				},
			},
		},
	}
	got := summarizePodState(pod)
	want := "phase=Failed; buildkitd terminated exitCode=137 reason=OOMKilled"
	if got != want {
		t.Errorf("terminated: got %q, want %q", got, want)
	}

	waiting := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "buildkitd",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"},
					},
				},
			},
		},
	}
	if got := summarizePodState(waiting); got != "phase=Pending; buildkitd waiting reason=ImagePullBackOff" {
		t.Errorf("waiting: got %q", got)
	}
}
