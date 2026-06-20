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

// K8sClient provides a wrapper for Kubernetes client operations
type K8sClient struct {
	clientset *kubernetes.Clientset
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
}

// GetBuildKitConfig retrieves configuration from buildkit ConfigMap and Secret
func (c *K8sClient) GetBuildKitConfig(ctx context.Context) (*BuildKitConfig, error) {
	// Get ConfigMap
	cm, err := c.clientset.CoreV1().ConfigMaps(Namespace).Get(ctx, "buildkit", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get ConfigMap buildkit: %w", err)
	}

	// Get Secret
	secret, err := c.clientset.CoreV1().Secrets(Namespace).Get(ctx, "buildkit", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get Secret buildkit: %w", err)
	}

	config := &BuildKitConfig{
		HarborURL:      cm.Data["harbor-url"],
		HarborCoreURL:  cm.Data["harbor-core-url"],
		ClusterDomain:  cm.Data["cluster-domain"],
		HarborUsername: string(secret.Data["harbor-username"]),
		HarborPassword: string(secret.Data["harbor-password"]),
	}

	return config, nil
}

// GetClientset returns the underlying Kubernetes clientset
func (c *K8sClient) GetClientset() *kubernetes.Clientset {
	return c.clientset
}
