package buildkitclient

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/runos-official/clusteragent/datastore"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Ephemeral per-build BuildKit daemons.
//
// Each build gets its own freshly-created buildkitd pod, used for exactly one
// build and deleted afterwards. There is no long-lived daemon, no Service, no
// StatefulSet: the wedge class that plagued the shared daemon (a leftover
// process holding :1234 / buildkitd.lock inside a live sandbox, CrashLooping
// in-place restarts forever) cannot occur because every build starts from a
// fresh sandbox by construction. Layer cache persistence comes from Harbor
// registry cache import/export (see Build), not from daemon-local disk.

const (
	// BuilderNamespace is where per-build buildkitd pods run. Same namespace
	// the legacy shared daemon used, so existing clusters need no new
	// namespace plumbing.
	BuilderNamespace = "buildkit"

	// builderPodPrefix names per-build pods `buildkit-build-<job-id>`.
	builderPodPrefix = "buildkit-build-"

	// builderTypeLabel marks pods this package owns; the startup sweep deletes
	// anything carrying it (a pod present at agent start is an orphan: its
	// build died with the previous agent process).
	builderTypeLabel = "buildkit-build"

	// defaultBuildkitImage is the buildkitd image for per-build pods.
	// Override with BUILDKIT_IMAGE.
	defaultBuildkitImage = "moby/buildkit:v0.30.0"

	// defaultMaxConcurrentBuilds bounds how many build pods run at once.
	// Builds beyond the limit wait (their job row stays busy and the wait is
	// logged). Override with BUILDKIT_MAX_CONCURRENT_BUILDS.
	defaultMaxConcurrentBuilds = 3

	// builderReadyTimeout bounds how long we wait for a fresh build pod to
	// schedule, pull its image, and pass its readiness probe.
	builderReadyTimeout = 3 * time.Minute

	// builderPollInterval is the pod-status poll cadence while waiting.
	builderPollInterval = 2 * time.Second

	// defaultBuildCacheSizeGb caps the per-build scratch volume when the
	// build-settings ConfigMap is absent. Mirrors conductor's
	// DEFAULT_BUILD_SETTINGS.buildCacheSizeGb; keep the two in sync.
	defaultBuildCacheSizeGb = 50

	// buildCacheVolumeName is the scratch volume's name in the pod spec. The
	// generic ephemeral volume's PVC is named <pod>-<this> by Kubernetes.
	buildCacheVolumeName = "buildkit-cache"

	// builderPort is buildkitd's gRPC listener inside the pod. Pods have
	// isolated network namespaces, so every concurrent build pod listens on
	// this same port on its own pod IP.
	builderPort = 1234
)

// buildkitImage returns the buildkitd image for per-build pods, honouring
// BUILDKIT_IMAGE when set.
func buildkitImage() string {
	if v := os.Getenv("BUILDKIT_IMAGE"); v != "" {
		return v
	}
	return defaultBuildkitImage
}

// maxConcurrentBuilds returns the env/default build-pod concurrency cap,
// honouring BUILDKIT_MAX_CONCURRENT_BUILDS when set to a positive integer.
// Used as the fallback when the cluster's build-settings ConfigMap doesn't
// set maxConcurrentBuilds (see ReadBuildSettings).
func maxConcurrentBuilds() int {
	if v := os.Getenv("BUILDKIT_MAX_CONCURRENT_BUILDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		log.Printf("buildkitclient: ignoring invalid BUILDKIT_MAX_CONCURRENT_BUILDS=%q, using default %d", v, defaultMaxConcurrentBuilds)
	}
	return defaultMaxConcurrentBuilds
}

// BuildSettings are the cluster-level per-build settings, edited in the
// console / CLI and delivered via the `build-settings` ConfigMap in the
// runos namespace (conductor pushes it on every settings PATCH). Read fresh
// before each build, so changes apply to the next build without a restart.
type BuildSettings struct {
	// MaxConcurrentBuilds bounds how many build pods run at once; excess
	// builds wait (visible in the build log).
	MaxConcurrentBuilds int
	// CPURequestMc is the per-pod CPU request in millicores. Set explicitly
	// and small: with only limits set, Kubernetes defaults requests to the
	// limits, so a build pod would reserve a full CPU of capacity.
	CPURequestMc int
	// CPULimitMc is the per-pod CPU limit in millicores.
	CPULimitMc int
	// MemoryRequestMb is the per-pod memory request in MB.
	MemoryRequestMb int
	// MemoryLimitMb is the per-pod memory limit in MB.
	MemoryLimitMb int
	// BuildCacheSizeGb caps the build's scratch volume. It applies in BOTH
	// modes: it sizes the LINSTOR volume, and it caps the emptyDir. The cap
	// is the point; before it existed a runaway build filled the node's root
	// filesystem instead of failing its own build.
	BuildCacheSizeGb int
	// BuildCacheStorageClass names the StorageClass to draw the scratch
	// volume from. Empty (the default) means the node's disk via emptyDir.
	// Conductor sends the class name rather than a flag, so there is no way
	// to be in "distributed" mode with nothing to ask for.
	BuildCacheStorageClass string
}

// defaultBuildSettings mirrors conductor's DEFAULT_BUILD_SETTINGS; they must
// stay in sync (these apply when the ConfigMap is absent or partial).
func defaultBuildSettings() BuildSettings {
	return BuildSettings{
		MaxConcurrentBuilds: maxConcurrentBuilds(),
		CPURequestMc:        100,
		CPULimitMc:          1000,
		MemoryRequestMb:     256,
		MemoryLimitMb:       2048,
		BuildCacheSizeGb:    defaultBuildCacheSizeGb,
		// Empty: the node's disk. A cluster that never edited its build
		// settings keeps the behaviour it had before this existed, except
		// for the size cap.
		BuildCacheStorageClass: "",
	}
}

// parseBuildSettings merges the ConfigMap's string values over the defaults.
// Invalid or missing values fall back per-field.
func parseBuildSettings(data map[string]string) BuildSettings {
	s := defaultBuildSettings()
	if n, err := strconv.Atoi(data["maxConcurrentBuilds"]); err == nil && n > 0 {
		s.MaxConcurrentBuilds = n
	}
	if n, err := strconv.Atoi(data["cpuRequestMc"]); err == nil && n > 0 {
		s.CPURequestMc = n
	}
	if n, err := strconv.Atoi(data["cpuLimitMc"]); err == nil && n > 0 {
		s.CPULimitMc = n
	}
	if n, err := strconv.Atoi(data["memoryRequestMb"]); err == nil && n > 0 {
		s.MemoryRequestMb = n
	}
	if n, err := strconv.Atoi(data["memoryLimitMb"]); err == nil && n > 0 {
		s.MemoryLimitMb = n
	}
	if n, err := strconv.Atoi(data["buildCacheSizeGb"]); err == nil && n > 0 {
		s.BuildCacheSizeGb = n
	}
	// Absent and empty both mean "node disk"; only a non-empty class switches
	// modes, so a truncated or half-written ConfigMap falls back rather than
	// asking for a StorageClass named "".
	s.BuildCacheStorageClass = strings.TrimSpace(data["buildCacheStorageClass"])
	// A request above its limit is rejected by the K8s API at pod create.
	// Conductor refuses such a PATCH, but clamp defensively in case the
	// ConfigMap was edited out-of-band.
	if s.CPURequestMc > s.CPULimitMc {
		s.CPURequestMc = s.CPULimitMc
	}
	if s.MemoryRequestMb > s.MemoryLimitMb {
		s.MemoryRequestMb = s.MemoryLimitMb
	}
	return s
}

// ReadBuildSettings fetches the cluster's build settings from the
// build-settings ConfigMap in the runos namespace, falling back to defaults
// when the ConfigMap is absent (clusters where settings were never edited).
func ReadBuildSettings(ctx context.Context, clientset kubernetes.Interface) BuildSettings {
	cm, err := clientset.CoreV1().ConfigMaps("runos").Get(ctx, "build-settings", metav1.GetOptions{})
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			log.Printf("buildkitclient: failed to read build-settings ConfigMap, using defaults: %v", err)
		}
		return defaultBuildSettings()
	}
	return parseBuildSettings(cm.Data)
}

var (
	buildSlotMu  sync.Mutex
	activeBuilds int
)

// acquireBuildSlot blocks until fewer than limit builds are active (or ctx
// expires). The limit comes from the cluster's build settings, read fresh
// per build, so a settings change applies to the next acquire; a plain
// poll-loop (not a fixed-size channel) keeps the cap dynamic.
func acquireBuildSlot(ctx context.Context, limit int) error {
	for {
		buildSlotMu.Lock()
		if activeBuilds < limit {
			activeBuilds++
			buildSlotMu.Unlock()
			return nil
		}
		buildSlotMu.Unlock()
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for a build slot: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// releaseBuildSlot frees a slot taken by acquireBuildSlot.
func releaseBuildSlot() {
	buildSlotMu.Lock()
	activeBuilds--
	buildSlotMu.Unlock()
}

// activeBuildCount reports how many builds currently hold a slot.
func activeBuildCount() int {
	buildSlotMu.Lock()
	defer buildSlotMu.Unlock()
	return activeBuilds
}

// sanitizeK8sName maps a value to the DNS-1123 subset pod names accept:
// lowercase alphanumerics and '-'. Other runes become '-'.
func sanitizeK8sName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// builderPodName derives the per-build pod name from the build job id.
// Job ids are UUIDs (CLI deploys) or `<sha>-<uuid8>` (VCS deploys), so the
// sanitized form is already unique per build attempt. Capped at 63 chars
// (DNS-1123 label limit) and trimmed of a trailing '-' the cap could expose.
func builderPodName(jobID string) string {
	name := builderPodPrefix + sanitizeK8sName(jobID)
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

// BuilderMeta identifies the build a pod belongs to. It lands on the pod as
// labels (queryable, sanitized) and annotations (free text) so manual
// inspection of the buildkit namespace shows what each pod is building:
//
//	kubectl get pods -n buildkit -L runos.com/osid,runos.com/commit
//	kubectl describe pod <pod> -n buildkit   (repo / branch / images)
type BuilderMeta struct {
	JobID  string
	OSID   string
	Commit string   // short commit / tag
	Repo   string   // credential-stripped clone URL; empty for non-VCS builds
	Branch string   // empty for non-VCS builds
	Images []string // fully-qualified push refs
}

// labelValue sanitizes a value for use as a K8s label value (63-char cap).
func labelValue(s string) string {
	v := sanitizeK8sName(s)
	if len(v) > 63 {
		v = v[:63]
	}
	return strings.Trim(v, "-")
}

// buildCacheVolume builds the pod's scratch volume for `/var/lib/buildkit`.
//
// Two shapes, one lifetime. Both are created with the pod and destroyed with
// it; neither survives a build, because layer-cache reuse comes from the
// Harbor registry cache (see Build), not from this directory.
//
//   - Node disk (default): an emptyDir, now with a sizeLimit. Exceeding the
//     limit gets the pod evicted, which fails one build instead of filling
//     the node's root filesystem and taking every workload on it down.
//   - Distributed: a GENERIC EPHEMERAL VOLUME. Kubernetes creates the PVC
//     when the pod is created and deletes it when the pod goes, so there is
//     no volume pool to lease, no orphan to reclaim, and nothing for the
//     startup sweep to do beyond deleting the pod as it already does. The
//     class conductor points us at is WaitForFirstConsumer with one replica,
//     so the volume is placed on whichever node the pod scheduled to and the
//     build's I/O stays local.
func buildCacheVolume(settings BuildSettings) corev1.Volume {
	size := resource.MustParse(fmt.Sprintf("%dGi", settings.BuildCacheSizeGb))

	if settings.BuildCacheStorageClass == "" {
		return corev1.Volume{
			Name:         buildCacheVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size}},
		}
	}

	storageClass := settings.BuildCacheStorageClass
	return corev1.Volume{
		Name: buildCacheVolumeName,
		VolumeSource: corev1.VolumeSource{
			Ephemeral: &corev1.EphemeralVolumeSource{
				VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app.kubernetes.io/managed-by": "runos",
							"runos.com/type":               builderTypeLabel,
						},
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: &storageClass,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: size},
						},
					},
				},
			},
		},
	}
}

// newBuilderPod assembles the per-build buildkitd pod: privileged (same
// trust level the shared daemon ran at), a per-build scratch volume,
// requests + limits from the cluster's build settings (requests explicit:
// omitting them would make Kubernetes default them to the limits), never
// restarted (a failed daemon fails the build; the next build gets a fresh
// pod), and a deadline backstop slightly beyond the build timeout so an
// orphaned pod self-terminates even if the agent dies before deleting it.
func newBuilderPod(name string, meta BuilderMeta, settings BuildSettings) *corev1.Pod {
	deadline := int64((buildTimeout() + 10*time.Minute).Seconds())
	grace := int64(5)

	// Labels are queryable but charset/length-restricted; identity fields go
	// there. Free-text context (repo URL, branch, image refs) goes into
	// annotations, omitting empty values.
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "runos",
		"runos.com/type":               builderTypeLabel,
		"runos.com/job-id":             labelValue(meta.JobID),
	}
	if meta.OSID != "" {
		labels["runos.com/osid"] = labelValue(meta.OSID)
	}
	if meta.Commit != "" {
		labels["runos.com/commit"] = labelValue(meta.Commit)
	}
	annotations := map[string]string{}
	if len(meta.Images) > 0 {
		annotations["runos.com/images"] = strings.Join(meta.Images, ",")
	}
	if meta.Repo != "" {
		annotations["runos.com/repo"] = meta.Repo
	}
	if meta.Branch != "" {
		annotations["runos.com/branch"] = meta.Branch
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   BuilderNamespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:         &deadline,
			TerminationGracePeriodSeconds: &grace,
			// The pod is privileged; it must not hold cluster credentials.
			AutomountServiceAccountToken: boolPtr(false),
			Containers: []corev1.Container{
				{
					Name:  "buildkitd",
					Image: buildkitImage(),
					Args:  []string{"--addr", fmt.Sprintf("tcp://0.0.0.0:%d", builderPort)},
					Ports: []corev1.ContainerPort{
						{Name: "grpc", ContainerPort: builderPort, Protocol: corev1.ProtocolTCP},
					},
					SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%dm", settings.CPURequestMc)),
							corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dMi", settings.MemoryRequestMb)),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%dm", settings.CPULimitMc)),
							corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dMi", settings.MemoryLimitMb)),
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: buildCacheVolumeName, MountPath: "/var/lib/buildkit"},
					},
					// BuildKit serves grpc.health.v1.Health on its gRPC
					// endpoint; ready means the TCP listener clients use is
					// actually answering. Fresh daemons on an empty cache come
					// up in a couple of seconds.
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							GRPC: &corev1.GRPCAction{Port: builderPort},
						},
						InitialDelaySeconds: 1,
						PeriodSeconds:       2,
						TimeoutSeconds:      5,
						FailureThreshold:    60,
					},
				},
			},
			Volumes: []corev1.Volume{buildCacheVolume(settings)},
		},
	}
}

func boolPtr(b bool) *bool { return &b }

// StartEphemeralBuilder creates a fresh buildkitd pod for one build and waits
// for it to become ready. It returns the daemon address (tcp://<podIP>:1234)
// and a cleanup func the caller must invoke (deletes the pod). On error the
// pod is already cleaned up.
func StartEphemeralBuilder(ctx context.Context, clientset kubernetes.Interface, meta BuilderMeta, settings BuildSettings) (string, func(), error) {
	name := builderPodName(meta.JobID)
	pods := clientset.CoreV1().Pods(BuilderNamespace)

	if err := ensureBuilderNamespace(ctx, clientset); err != nil {
		return "", nil, err
	}

	// A stale pod with this name can only be a leftover from a previous agent
	// process that died mid-build (job ids are unique per attempt). Replace it.
	if err := pods.Delete(ctx, name, metav1.DeleteOptions{}); err == nil {
		log.Printf("buildkitclient: deleted stale build pod %s before recreate", name)
		if err := waitForPodGone(ctx, clientset, name); err != nil {
			return "", nil, err
		}
	} else if !k8serrors.IsNotFound(err) {
		return "", nil, fmt.Errorf("failed to check for stale build pod %s: %w", name, err)
	}

	if _, err := pods.Create(ctx, newBuilderPod(name, meta, settings), metav1.CreateOptions{}); err != nil {
		return "", nil, fmt.Errorf("failed to create build pod %s: %w", name, err)
	}

	cleanup := func() {
		// Background context: the build ctx may already be expired (timeout
		// path), and the pod must still be deleted.
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := pods.Delete(dctx, name, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
			log.Printf("buildkitclient: failed to delete build pod %s: %v (deadline backstop will reap it)", name, err)
			return
		}
		log.Printf("buildkitclient: deleted build pod %s", name)
	}

	addr, err := waitForBuilderReady(ctx, clientset, name, meta.JobID)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return addr, cleanup, nil
}

// ensureBuilderNamespace creates the buildkit namespace if missing.
func ensureBuilderNamespace(ctx context.Context, clientset kubernetes.Interface) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: BuilderNamespace}}
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to ensure namespace %s: %w", BuilderNamespace, err)
	}
	return nil
}

// waitForPodGone polls until a deleted pod's object is actually removed, so a
// recreate with the same name can't race the deletion.
func waitForPodGone(ctx context.Context, clientset kubernetes.Interface, name string) error {
	for {
		if _, err := clientset.CoreV1().Pods(BuilderNamespace).Get(ctx, name, metav1.GetOptions{}); k8serrors.IsNotFound(err) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for stale build pod %s to terminate", name)
		case <-time.After(builderPollInterval):
		}
	}
}

// waitForBuilderReady polls the pod until it is Ready with a pod IP, surfacing
// blocking waiting reasons (ImagePullBackOff etc.) to the build log once each.
func waitForBuilderReady(ctx context.Context, clientset kubernetes.Interface, name, jobID string) (string, error) {
	deadline := time.Now().Add(builderReadyTimeout)
	var lastReason string

	for {
		pod, err := clientset.CoreV1().Pods(BuilderNamespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
				return "", fmt.Errorf("build pod %s terminated before becoming ready (phase %s)", name, pod.Status.Phase)
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != lastReason {
					lastReason = cs.State.Waiting.Reason
					datastore.InsertBuildKitLog(jobID, fmt.Sprintf("build pod %s: %s: %s", name, cs.State.Waiting.Reason, cs.State.Waiting.Message))
				}
			}
			if pod.Status.PodIP != "" && podIsReady(pod) {
				return fmt.Sprintf("tcp://%s:%d", pod.Status.PodIP, builderPort), nil
			}
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("build pod %s not ready after %s (last waiting reason: %s)", name, builderReadyTimeout, lastReason)
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("cancelled waiting for build pod %s: %w", name, ctx.Err())
		case <-time.After(builderPollInterval):
		}
	}
}

// podIsReady reports whether the pod's Ready condition is True.
func podIsReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// builderStateSummary reads the build pod's current state for failure
// forensics: pod phase plus each container's terminated exit code/reason
// (e.g. 137/OOMKilled, 137/Error for an external SIGKILL) or waiting reason.
// Must be called BEFORE the pod is deleted; cleanup destroys the evidence.
// Uses its own short context: the build context is often already expired on
// the timeout failure path.
func builderStateSummary(clientset kubernetes.Interface, jobID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pod, err := clientset.CoreV1().Pods(BuilderNamespace).Get(ctx, builderPodName(jobID), metav1.GetOptions{})
	if err != nil {
		return fmt.Sprintf("build pod state unavailable: %v", err)
	}
	return summarizePodState(pod)
}

// summarizePodState renders a pod's status as a one-line summary. Pure,
// unit-tested.
func summarizePodState(pod *corev1.Pod) string {
	parts := []string{fmt.Sprintf("phase=%s", pod.Status.Phase)}
	if pod.Status.Reason != "" {
		parts = append(parts, fmt.Sprintf("podReason=%s", pod.Status.Reason))
	}
	for _, cs := range pod.Status.ContainerStatuses {
		switch {
		case cs.State.Terminated != nil:
			t := cs.State.Terminated
			p := fmt.Sprintf("%s terminated exitCode=%d reason=%s", cs.Name, t.ExitCode, t.Reason)
			if t.Message != "" {
				p += fmt.Sprintf(" message=%q", t.Message)
			}
			parts = append(parts, p)
		case cs.State.Waiting != nil:
			parts = append(parts, fmt.Sprintf("%s waiting reason=%s", cs.Name, cs.State.Waiting.Reason))
		case cs.State.Running != nil:
			parts = append(parts, fmt.Sprintf("%s running since %s", cs.Name, cs.State.Running.StartedAt.Format(time.RFC3339)))
		}
	}
	return strings.Join(parts, "; ")
}

// StartupCleanup runs once at agent start:
//
//  1. Sweeps orphaned per-build pods. Builds die with the agent process
//     (buildctl runs in-agent), so any build pod present at startup belongs
//     to a dead build and is deleted.
//  2. Tears down the legacy shared buildkitd daemon (StatefulSet + Service +
//     config + watchdog RBAC) left behind on clusters installed before the
//     move to per-build pods. Idempotent; NotFound is the steady state.
func StartupCleanup(clientset kubernetes.Interface) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 1. Orphaned build pods.
	selector := "runos.com/type=" + builderTypeLabel
	if pods, err := clientset.CoreV1().Pods(BuilderNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector}); err == nil {
		for _, pod := range pods.Items {
			if err := clientset.CoreV1().Pods(BuilderNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
				log.Printf("buildkitclient: startup sweep failed to delete orphaned build pod %s: %v", pod.Name, err)
				continue
			}
			log.Printf("buildkitclient: startup sweep deleted orphaned build pod %s", pod.Name)
		}
	} else if !k8serrors.IsNotFound(err) {
		log.Printf("buildkitclient: startup sweep could not list build pods: %v", err)
	}

	// 2. Legacy shared-daemon resources.
	type deletion struct {
		kind string
		del  func() error
	}
	deletions := []deletion{
		{"StatefulSet buildkitd", func() error {
			return clientset.AppsV1().StatefulSets(BuilderNamespace).Delete(ctx, "buildkitd", metav1.DeleteOptions{})
		}},
		{"Service buildkitd", func() error {
			return clientset.CoreV1().Services(BuilderNamespace).Delete(ctx, "buildkitd", metav1.DeleteOptions{})
		}},
		{"ConfigMap buildkitd-config", func() error {
			return clientset.CoreV1().ConfigMaps(BuilderNamespace).Delete(ctx, "buildkitd-config", metav1.DeleteOptions{})
		}},
		{"ServiceAccount buildkitd-watchdog", func() error {
			return clientset.CoreV1().ServiceAccounts(BuilderNamespace).Delete(ctx, "buildkitd-watchdog", metav1.DeleteOptions{})
		}},
		{"Role buildkitd-watchdog", func() error {
			return clientset.RbacV1().Roles(BuilderNamespace).Delete(ctx, "buildkitd-watchdog", metav1.DeleteOptions{})
		}},
		{"RoleBinding buildkitd-watchdog", func() error {
			return clientset.RbacV1().RoleBindings(BuilderNamespace).Delete(ctx, "buildkitd-watchdog", metav1.DeleteOptions{})
		}},
	}
	for _, d := range deletions {
		if err := d.del(); err != nil {
			if !k8serrors.IsNotFound(err) {
				log.Printf("buildkitclient: legacy daemon teardown: failed to delete %s: %v", d.kind, err)
			}
			continue
		}
		log.Printf("buildkitclient: legacy daemon teardown: deleted %s", d.kind)
	}
}
