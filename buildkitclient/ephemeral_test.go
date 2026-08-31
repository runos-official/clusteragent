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
	want := BuildSettings{MaxConcurrentBuilds: 8, CPURequestMc: 250, CPULimitMc: 2000, MemoryRequestMb: 512, MemoryLimitMb: 4096, BuildCacheSizeGb: defaultBuildCacheSizeGb}
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

// The scratch volume is the reason FPL27 exists: an uncapped emptyDir on the
// node's kubelet disk let one build fill a node's root filesystem.
func TestBuildCacheVolume_NodeDiskByDefault(t *testing.T) {
	vol := buildCacheVolume(defaultBuildSettings())

	if vol.Name != buildCacheVolumeName {
		t.Fatalf("volume name: got %q, want %q", vol.Name, buildCacheVolumeName)
	}
	if vol.EmptyDir == nil {
		t.Fatal("default settings must keep the build cache on the node's disk")
	}
	if vol.Ephemeral != nil {
		t.Error("default settings must not provision a PVC")
	}
	if vol.EmptyDir.SizeLimit == nil {
		t.Fatal("the emptyDir must be capped; uncapped is what fills the node")
	}
	if got := vol.EmptyDir.SizeLimit.String(); got != "50Gi" {
		t.Errorf("size limit: got %q, want 50Gi", got)
	}
}

func TestBuildCacheVolume_SizeAppliesToTheNodeDiskToo(t *testing.T) {
	s := defaultBuildSettings()
	s.BuildCacheSizeGb = 120
	vol := buildCacheVolume(s)

	if vol.EmptyDir == nil || vol.EmptyDir.SizeLimit == nil {
		t.Fatal("expected a capped emptyDir")
	}
	if got := vol.EmptyDir.SizeLimit.String(); got != "120Gi" {
		t.Errorf("size limit: got %q, want 120Gi", got)
	}
}

// A generic ephemeral volume, not a hand-managed PVC: Kubernetes creates it
// with the pod and deletes it with the pod, so no leasing or reclaim exists
// to go wrong.
func TestBuildCacheVolume_DistributedUsesGenericEphemeralVolume(t *testing.T) {
	s := defaultBuildSettings()
	s.BuildCacheStorageClass = "linstor-buildkit"
	s.BuildCacheSizeGb = 80
	vol := buildCacheVolume(s)

	if vol.EmptyDir != nil {
		t.Error("distributed mode must not fall back to the node's disk")
	}
	if vol.Ephemeral == nil || vol.Ephemeral.VolumeClaimTemplate == nil {
		t.Fatal("distributed mode must use a generic ephemeral volume")
	}
	tmpl := vol.Ephemeral.VolumeClaimTemplate
	if tmpl.Spec.StorageClassName == nil || *tmpl.Spec.StorageClassName != "linstor-buildkit" {
		t.Errorf("storage class: got %v, want linstor-buildkit", tmpl.Spec.StorageClassName)
	}
	got := tmpl.Spec.Resources.Requests[corev1.ResourceStorage]
	if got.String() != "80Gi" {
		t.Errorf("requested size: got %q, want 80Gi", got.String())
	}
	// buildkitd needs exclusive access to /var/lib/buildkit; the volume is
	// never shared between build pods.
	if len(tmpl.Spec.AccessModes) != 1 || tmpl.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("access modes: got %v, want [ReadWriteOnce]", tmpl.Spec.AccessModes)
	}
}

// An empty class is the only "off" signal, so a half-written ConfigMap must
// fall back to the node's disk rather than ask for a StorageClass named "".
func TestParseBuildSettings_BlankStorageClassMeansNodeDisk(t *testing.T) {
	for _, data := range []map[string]string{
		{},
		{"buildCacheStorageClass": ""},
		{"buildCacheStorageClass": "   "},
	} {
		if got := parseBuildSettings(data).BuildCacheStorageClass; got != "" {
			t.Errorf("%v: got storage class %q, want empty", data, got)
		}
		if buildCacheVolume(parseBuildSettings(data)).EmptyDir == nil {
			t.Errorf("%v: expected the node's disk", data)
		}
	}
}

func TestParseBuildSettings_ReadsCacheKeys(t *testing.T) {
	got := parseBuildSettings(map[string]string{
		"buildCacheSizeGb":       "200",
		"buildCacheStorageClass": "linstor-buildkit",
	})
	if got.BuildCacheSizeGb != 200 {
		t.Errorf("size: got %d, want 200", got.BuildCacheSizeGb)
	}
	if got.BuildCacheStorageClass != "linstor-buildkit" {
		t.Errorf("class: got %q, want linstor-buildkit", got.BuildCacheStorageClass)
	}

	// An unparseable size keeps the default rather than provisioning a 0Gi volume.
	if got := parseBuildSettings(map[string]string{"buildCacheSizeGb": "nonsense"}).BuildCacheSizeGb; got != defaultBuildCacheSizeGb {
		t.Errorf("invalid size: got %d, want the default %d", got, defaultBuildCacheSizeGb)
	}
}

// The pod must mount whichever shape it got, under the same path either way.
func TestNewBuilderPod_MountsTheCacheVolumeInBothModes(t *testing.T) {
	for _, class := range []string{"", "linstor-buildkit"} {
		s := defaultBuildSettings()
		s.BuildCacheStorageClass = class
		pod := newBuilderPod("buildkit-build-x", BuilderMeta{JobID: "x"}, s)

		if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].Name != buildCacheVolumeName {
			t.Fatalf("class %q: unexpected volumes %+v", class, pod.Spec.Volumes)
		}
		mounts := pod.Spec.Containers[0].VolumeMounts
		if len(mounts) != 1 || mounts[0].Name != buildCacheVolumeName || mounts[0].MountPath != "/var/lib/buildkit" {
			t.Errorf("class %q: unexpected mounts %+v", class, mounts)
		}
	}
}
