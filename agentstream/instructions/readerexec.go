package instructions

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
)

// Reader-exec surface for the read-only cluster assistant.
//
// The assistant runs ARBITRARY kubectl on behalf of the LLM. Rather than trust
// an argv allowlist, we run it inside a dedicated pod whose ONLY identity is a
// read-only ServiceAccount: no admin token is present, so kubectl can neither
// escalate (nothing stronger to steal via a local-file read) nor write (RBAC on
// the reader SA is the hard floor). The cluster agent (admin) only creates the
// RBAC + pod and execs into it; the exec'd kubectl authenticates as the pod's
// reader SA via the mounted token (tokenFile in the kubeconfig, so rotation is
// handled). One shared pod per cluster, lazily spawned and idle-reaped.
const (
	// Namespace the RunOS platform runs in. Redeclared here because the
	// instructions package cannot import agentstream (import cycle), mirroring
	// how newClientset is duplicated in run_oneshot_job.go.
	Namespace = "runos"

	readerSA           = "runos-assistant-reader"
	readerClusterRole  = "runos-assistant-reader"
	readerPod          = "runos-assistant-reader-exec"
	readerKubeconfigCM = "runos-assistant-reader-kubeconfig"
	readerContainer    = "kubectl"

	readerIdleTTL      = 15 * time.Minute
	readerReadyTimeout = 90 * time.Second
	readerReapInterval = 2 * time.Minute
)

// defaultReaderImage must contain both a `kubectl` binary and a `sleep` (to keep
// the container alive). Overridable for private registries / version pinning.
const defaultReaderImage = "alpine/k8s:1.34.1"

func readerImage() string {
	if v := os.Getenv("ASSISTANT_KUBECTL_IMAGE"); v != "" {
		return v
	}
	return defaultReaderImage
}

var (
	readerMu       sync.Mutex // serializes ensure + lastUsed access
	readerLastUsed time.Time
	readerReaper   sync.Once
)

func boolPtr(b bool) *bool { return &b }

// ensureReaderExecPod makes the reader RBAC + pod exist and Ready, and records
// the access time for the idle reaper. Serialized so concurrent requests do not
// race on creation.
func ensureReaderExecPod(ctx context.Context, clientset *k8s.Clientset) error {
	readerMu.Lock()
	defer readerMu.Unlock()

	readerLastUsed = time.Now()
	readerReaper.Do(func() { go reapReaderPod(clientset) })

	pod, err := clientset.CoreV1().Pods(Namespace).Get(ctx, readerPod, metav1.GetOptions{})
	if err == nil && pod.Status.Phase == corev1.PodRunning && podReady(pod) {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get reader pod: %w", err)
	}

	if apierrors.IsNotFound(err) {
		if err := ensureReaderRBAC(ctx, clientset); err != nil {
			return err
		}
		if err := ensureReaderKubeconfig(ctx, clientset); err != nil {
			return err
		}
		if _, err := clientset.CoreV1().Pods(Namespace).Create(ctx, buildReaderPod(), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create reader pod: %w", err)
		}
	}

	return waitReaderPodReady(ctx, clientset)
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func waitReaderPodReady(ctx context.Context, clientset *k8s.Clientset) error {
	deadline := time.Now().Add(readerReadyTimeout)
	for {
		pod, err := clientset.CoreV1().Pods(Namespace).Get(ctx, readerPod, metav1.GetOptions{})
		if err == nil && pod.Status.Phase == corev1.PodRunning && podReady(pod) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("reader-exec pod not ready within %s", readerReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// reapReaderPod deletes the shared reader pod after it has been idle, so an
// unused assistant leaves nothing running. RBAC + ConfigMap are cheap and kept.
func reapReaderPod(clientset *k8s.Clientset) {
	ticker := time.NewTicker(readerReapInterval)
	defer ticker.Stop()
	for range ticker.C {
		readerMu.Lock()
		idle := time.Since(readerLastUsed)
		readerMu.Unlock()
		if idle < readerIdleTTL {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := clientset.CoreV1().Pods(Namespace).Delete(ctx, readerPod, metav1.DeleteOptions{})
		cancel()
		if err != nil && !apierrors.IsNotFound(err) {
			log.Printf("reader-exec: reap delete failed: %v", err)
		} else if err == nil {
			log.Printf("reader-exec: reaped idle pod after %s", idle.Round(time.Second))
		}
	}
}

func buildReaderPod() *corev1.Pod {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "runos",
		"runos.com/type":               "assistant-reader-exec",
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: readerPod, Namespace: Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			ServiceAccountName:           readerSA,
			AutomountServiceAccountToken: boolPtr(true),
			RestartPolicy:                corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:    readerContainer,
				Image:   readerImage(),
				Command: []string{"sleep", "infinity"},
				Env:     []corev1.EnvVar{{Name: "KUBECONFIG", Value: "/kube/config"}},
				VolumeMounts: []corev1.VolumeMount{{
					Name: "kubeconfig", MountPath: "/kube", ReadOnly: true,
				}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("250m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "kubeconfig",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: readerKubeconfigCM},
					},
				},
			}},
		},
	}
}

// readerKubeconfig points kubectl at the in-cluster API using the pod's own
// mounted reader-SA token (tokenFile => re-read each call, so rotation is fine)
// and CA. No token value is embedded; the identity is the pod's reader SA.
const readerKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: incluster
  cluster:
    server: https://kubernetes.default.svc
    certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
contexts:
- name: incluster
  context:
    cluster: incluster
    user: reader
current-context: incluster
users:
- name: reader
  user:
    tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
`

func ensureReaderKubeconfig(ctx context.Context, clientset *k8s.Clientset) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: readerKubeconfigCM, Namespace: Namespace},
		Data:       map[string]string{"config": readerKubeconfig},
	}
	if _, err := clientset.CoreV1().ConfigMaps(Namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create reader kubeconfig configmap: %w", err)
	}
	return nil
}

// ensureReaderRBAC creates the read-only ServiceAccount, ClusterRole, and
// bindings if missing. The role is get/list/watch only and deliberately
// excludes core Secrets; it is bound alongside the built-in `view` role.
// Create-if-missing: to change the rules, delete the ClusterRole and let a new
// agent version recreate it.
func ensureReaderRBAC(ctx context.Context, clientset *k8s.Clientset) error {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: readerSA, Namespace: Namespace}}
	if _, err := clientset.CoreV1().ServiceAccounts(Namespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create reader SA: %w", err)
	}

	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: readerClusterRole},
		Rules: []rbacv1.PolicyRule{
			// Cluster-scoped core reads that the built-in `view` role misses. No
			// core "*" and no secrets.
			{APIGroups: []string{""}, Resources: []string{"nodes", "namespaces", "persistentvolumes", "componentstatuses"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"metrics.k8s.io"}, Resources: []string{"nodes", "pods"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{"apiextensions.k8s.io"}, Resources: []string{"customresourcedefinitions"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"apiregistration.k8s.io"}, Resources: []string{"apiservices"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"*"}, Verbs: []string{"get", "list", "watch"}},
			// RunOS operator / platform CRD groups (no Secret objects live here,
			// so "*" resources is safe and keeps this maintainable).
			{APIGroups: []string{
				"postgresql.cnpg.io", "moco.cybozu.com", "kafka.strimzi.io",
				"clickhouse.altinity.com", "clickhouse-keeper.altinity.com",
				"rabbitmq.com", "minio.min.io", "sts.min.io",
				"cert-manager.io", "acme.cert-manager.io", "acme.runos.com",
				"traefik.io", "traefik.containo.us", "monitoring.coreos.com",
				"linstor.linbit.com", "internal.linstor.linbit.com", "piraeus.io",
			}, Resources: []string{"*"}, Verbs: []string{"get", "list", "watch"}},
		},
	}
	if _, err := clientset.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create reader ClusterRole: %w", err)
	}

	saSubject := []rbacv1.Subject{{Kind: "ServiceAccount", Name: readerSA, Namespace: Namespace}}
	bindings := []*rbacv1.ClusterRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "runos-assistant-reader-view"},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "view"},
			Subjects:   saSubject,
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "runos-assistant-reader"},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: readerClusterRole},
			Subjects:   saSubject,
		},
	}
	for _, b := range bindings {
		if _, err := clientset.RbacV1().ClusterRoleBindings().Create(ctx, b, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create reader ClusterRoleBinding %s: %w", b.Name, err)
		}
	}
	return nil
}
