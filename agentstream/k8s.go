package agentstream

import (
	"context"
	"fmt"
	"log"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// Namespace where the app is deployed
	Namespace = "runos"

	// ConfigMap names
	RunosConfigMap = "runos-config"

	// Secret names
	ClusterAgentTLSSecret = "cluster-agent-tls"
	NodeAgentTLSSecret    = "node-agent-tls"
)

// K8sClient provides a wrapper for Kubernetes client operations. The field
// is the kubernetes.Interface a real *kubernetes.Clientset satisfies, not
// the concrete type, so tests can substitute client-go's fake Clientset
// (review finding 1074's regression needs this to exercise a torn
// ConfigMap/Secret read without a live cluster).
type K8sClient struct {
	clientset kubernetes.Interface
}

// NewK8sClient creates a new Kubernetes client using in-cluster config
func NewK8sClient() (*K8sClient, error) {
	// Get in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	// Create clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes clientset: %w", err)
	}

	return &K8sClient{
		clientset: clientset,
	}, nil
}

// RunosConfig holds the configuration values from the runos-config ConfigMap
type RunosConfig struct {
	AID           string
	Server        string
	InstallerURL  string
	ClusterDomain string
}

// GetRunosConfig retrieves all values from the runos-config ConfigMap
func (c *K8sClient) GetRunosConfig(ctx context.Context) (*RunosConfig, error) {
	cm, err := c.clientset.CoreV1().ConfigMaps(Namespace).Get(ctx, RunosConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get ConfigMap %s: %w", RunosConfigMap, err)
	}

	config := &RunosConfig{
		AID:           cm.Data["aid"],
		Server:        cm.Data["server"],
		InstallerURL:  cm.Data["installer_url"],
		ClusterDomain: cm.Data["cd"],
	}

	return config, nil
}

// TLSData holds TLS certificate data
type TLSData struct {
	TLSCert []byte
	TLSKey  []byte
	CACert  []byte
}

// GetClusterAgentTLS retrieves TLS data from the cluster-agent-tls Secret
func (c *K8sClient) GetClusterAgentTLS(ctx context.Context) (*TLSData, error) {
	secret, err := c.clientset.CoreV1().Secrets(Namespace).Get(ctx, ClusterAgentTLSSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get Secret %s: %w", ClusterAgentTLSSecret, err)
	}

	tlsData := &TLSData{
		TLSCert: secret.Data["tls.crt"],
		TLSKey:  secret.Data["tls.key"],
		CACert:  secret.Data["ca.crt"],
	}

	return tlsData, nil
}

// SetClusterAgentTLS creates or updates the cluster-agent-tls Secret with the provided TLS data
func (c *K8sClient) SetClusterAgentTLS(ctx context.Context, tlsData *TLSData) error {
	// Check if secret already exists
	exists, err := c.ClusterAgentTLSExists(ctx)
	if err != nil {
		return fmt.Errorf("failed to check if Secret %s exists: %w", ClusterAgentTLSSecret, err)
	}

	// Create data map for secret
	data := map[string][]byte{
		"tls.crt": tlsData.TLSCert,
		"tls.key": tlsData.TLSKey,
		"ca.crt":  tlsData.CACert,
	}

	// Create secret object
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClusterAgentTLSSecret,
			Namespace: Namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: data,
	}

	// Create or update secret
	if exists {
		_, err = c.clientset.CoreV1().Secrets(Namespace).Update(ctx, secret, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update Secret %s: %w", ClusterAgentTLSSecret, err)
		}
	} else {
		_, err = c.clientset.CoreV1().Secrets(Namespace).Create(ctx, secret, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create Secret %s: %w", ClusterAgentTLSSecret, err)
		}
	}

	log.Println("Successfully set cluster agent TLS credentials")

	return nil
}

// ClusterAgentTLSExists checks if the cluster-agent-tls Secret exists
func (c *K8sClient) ClusterAgentTLSExists(ctx context.Context) (bool, error) {
	_, err := c.clientset.CoreV1().Secrets(Namespace).Get(ctx, ClusterAgentTLSSecret, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("error checking if Secret %s exists: %w", ClusterAgentTLSSecret, err)
	}
	return true, nil
}

// GetNodeAgentTLS retrieves TLS data from the node-agent-tls Secret
func (c *K8sClient) GetNodeAgentTLS(ctx context.Context) (*TLSData, error) {
	secret, err := c.clientset.CoreV1().Secrets(Namespace).Get(ctx, NodeAgentTLSSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get Secret %s: %w", NodeAgentTLSSecret, err)
	}

	tlsData := &TLSData{
		TLSCert: secret.Data["tls.crt"],
		TLSKey:  secret.Data["tls.key"],
		CACert:  secret.Data["ca.crt"],
	}

	return tlsData, nil
}

// DeleteNodeAgentTLS deletes the node-agent-tls Secret
func (c *K8sClient) DeleteNodeAgentTLS(ctx context.Context) error {
	err := c.clientset.CoreV1().Secrets(Namespace).Delete(ctx, NodeAgentTLSSecret, metav1.DeleteOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil // Already deleted or doesn't exist
		}
		return fmt.Errorf("failed to delete Secret %s: %w", NodeAgentTLSSecret, err)
	}
	return nil
}

// FindNamespaceByLabels searches for a namespace matching the given labels
func (c *K8sClient) FindNamespaceByLabels(ctx context.Context, labels map[string]string) (string, error) {
	// Build label selector
	selector := metav1.LabelSelector{
		MatchLabels: labels,
	}
	labelSelector, err := metav1.LabelSelectorAsSelector(&selector)
	if err != nil {
		return "", fmt.Errorf("failed to build label selector: %w", err)
	}

	// List namespaces with label selector
	namespaces, err := c.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector.String(),
	})
	if err != nil {
		return "", fmt.Errorf("failed to list namespaces: %w", err)
	}

	if len(namespaces.Items) == 0 {
		return "", nil // No matching namespace found
	}

	// Return the first matching namespace
	return namespaces.Items[0].Name, nil
}

// GetNamespaceLabels retrieves all labels from a namespace
func (c *K8sClient) GetNamespaceLabels(ctx context.Context, namespaceName string) (map[string]string, error) {
	ns, err := c.clientset.CoreV1().Namespaces().Get(ctx, namespaceName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get namespace %s: %w", namespaceName, err)
	}
	return ns.Labels, nil
}

// CreateNamespaceWithLabels creates a new namespace with the specified labels
func (c *K8sClient) CreateNamespaceWithLabels(ctx context.Context, namespaceName string, labels map[string]string) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   namespaceName,
			Labels: labels,
		},
	}

	_, err := c.clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create namespace %s: %w", namespaceName, err)
	}

	log.Printf("Created namespace: %s with labels: %v", namespaceName, labels)
	return nil
}

// BuildKitConfig holds the build configuration (Harbor registry endpoint +
// credentials) read from the buildkit ConfigMap/Secret in the runos
// namespace. The daemon address that used to live here is gone: builds run
// against ephemeral per-build pods the agent creates itself (buildkitclient).
type BuildKitConfig struct {
	HarborURL      string
	HarborCoreURL  string
	ClusterDomain  string
	HarborUsername string
	HarborPassword string
	// HarborOsid is the Harbor service's osid, read from the buildkit
	// Secret's harbor-osid key (story 390, src/util/builds/ensureBuildConfig.ts
	// in Conductor). Conductor locks this identity before minting a
	// build_image token and sends it back as harborOsid on the token; the
	// upload path compares the two so a Harbor retarget between mint and
	// upload cannot silently redirect the push (FCR 2487, finding 1029/1030).
	// Empty when the Secret predates that key or names no Harbor.
	HarborOsid string
}

// GetBuildKitConfig retrieves configuration from the buildkit ConfigMap and
// Secret with one read. Every caller except the one bound build-image
// upload path uses this: pull, legacy build-image uploads, VCS builds, and
// image-existence checks (review finding 1269 - see GetBuildKitConfigVerified
// for why those callers must NOT get the double-read-and-compare behavior).
func (c *K8sClient) GetBuildKitConfig(ctx context.Context) (*BuildKitConfig, error) {
	return c.readBuildKitConfigOnce(ctx)
}

// GetBuildKitConfigVerified is GetBuildKitConfig's double-read,
// compare-and-refuse variant (review finding 1074).
//
// Conductor writes the ConfigMap and Secret as two separate, non-atomic
// Kubernetes API calls (ensureBuildConfig.ts: ensureConfigMap, then
// ensureSecret) whenever it provisions or repairs the build config,
// including a Harbor retarget. A single pass of two independent Get calls
// can therefore observe a torn pairing mid-write: the endpoint (ConfigMap's
// harbor-url/harbor-core-url) already naming the replacement Harbor while
// the identity and credentials (Secret's harbor-osid/harbor-username/
// harbor-password) still name the original one, or vice versa. Reading
// twice and requiring every field to agree detects that in-flight write and
// refuses rather than silently trusting an inconsistent pairing - the
// Harbor identity comparison at upload (criterion 6(b)) is only as
// trustworthy as the single coherent configuration revision it is compared
// against.
//
// Review finding 1269: applying this to every GetBuildKitConfig caller was
// too broad. A pull request calls this AFTER consuming its single-use
// token, so an unrelated field changing between the two reads (a harbor-
// osid rotation pull never looks at) burned the token and answered 500 for
// something pull never needed to check. A legacy build-image upload
// (criterion 6(d): no locked identity to verify) hit the same refusal
// before ever reaching its explicit legacy branch. Use this ONLY for a
// build-image upload whose token carries a bound, non-empty harborOsid;
// every other caller keeps GetBuildKitConfig's plain single read.
func (c *K8sClient) GetBuildKitConfigVerified(ctx context.Context) (*BuildKitConfig, error) {
	first, err := c.readBuildKitConfigOnce(ctx)
	if err != nil {
		return nil, err
	}
	second, err := c.readBuildKitConfigOnce(ctx)
	if err != nil {
		return nil, err
	}
	if *first != *second {
		return nil, fmt.Errorf("buildkit configuration changed between reads; refusing an inconsistent ConfigMap/Secret pairing")
	}
	return second, nil
}

func (c *K8sClient) readBuildKitConfigOnce(ctx context.Context) (*BuildKitConfig, error) {
	cm, err := c.clientset.CoreV1().ConfigMaps(Namespace).Get(ctx, "buildkit", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get ConfigMap buildkit: %w", err)
	}

	secret, err := c.clientset.CoreV1().Secrets(Namespace).Get(ctx, "buildkit", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get Secret buildkit: %w", err)
	}

	return &BuildKitConfig{
		HarborURL:      cm.Data["harbor-url"],
		HarborCoreURL:  cm.Data["harbor-core-url"],
		ClusterDomain:  cm.Data["cluster-domain"],
		HarborUsername: string(secret.Data["harbor-username"]),
		HarborPassword: string(secret.Data["harbor-password"]),
		HarborOsid:     string(secret.Data["harbor-osid"]),
	}, nil
}

// GetClientset returns the underlying Kubernetes clientset
func (c *K8sClient) GetClientset() kubernetes.Interface {
	return c.clientset
}
