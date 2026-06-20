// Package dns01 implements a cert-manager ACME DNS01 solver webhook that
// satisfies Let's Encrypt challenges by writing TXT records through the RunOS
// control plane, enabling wildcard certificate issuance for the cluster domain.
package dns01

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/agentstream/instructions"
	"github.com/runos-official/clusteragent/commons"

	extapi "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/cert-manager/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	"github.com/cert-manager/cert-manager/pkg/acme/webhook/cmd"
)

const (
	// Retry configuration for fetching certificates after CleanUp
	certFetchMaxAttempts = 10              // Try up to 10 times
	certFetchRetryDelay  = 1 * time.Minute // Wait 1 minute between attempts

	// Timeout for cache requests to Nodeward
	cacheRequestTimeout = 30 * time.Second
)

// GroupName is set to match the group name in your ClusterIssuer
var GroupName = os.Getenv("GROUP_NAME")

func Start() {
	if GroupName == "" {
		// Default to your group if not specified via environment variable
		GroupName = "acme.runos.com"
	}

	// Register our custom DNS provider with the webhook server
	cmd.RunWebhookServer(GroupName,
		&runosDNSProviderSolver{},
	)
}

// runosDNSProviderSolver implements the provider-specific logic for DNS01 challenges
type runosDNSProviderSolver struct {
	k8sClient *kubernetes.Clientset
}

// runosDNSProviderConfig holds the configuration for the webhook
type runosDNSProviderConfig struct {
	// Add configuration fields that might be needed in the future
	// These will be decoded from the webhook.config section in the ClusterIssuer
}

// Name returns the name of the solver as specified in the ClusterIssuer
func (c *runosDNSProviderSolver) Name() string {
	return "runos-dns"
}

// Present handles the DNS01 challenge presentation
func (c *runosDNSProviderSolver) Present(ch *v1alpha1.ChallengeRequest) error {
	cfg, err := loadConfig(ch.Config)
	if err != nil {
		return err
	}

	domain := ch.DNSName
	log.Printf("DNS01 challenge for domain: %s (config: %v)", domain, cfg)

	// Send DNS-01 challenge key to Nodeward for DNS record creation
	if err := agentstream.SendToServer(instructions.Dns01ToServer(ch.Key)); err != nil {
		log.Printf("Error sending DNS01 key to server: %v", err)
		return fmt.Errorf("failed to send DNS01 key: %w", err)
	}

	return nil
}

// CleanUp handles the DNS01 challenge cleanup
func (c *runosDNSProviderSolver) CleanUp(ch *v1alpha1.ChallengeRequest) error {
	log.Printf("DNS01 cleanup for domain: %s", ch.DNSName)

	// After challenge validated, cache the new cert (with retries)
	go c.cacheNewCert(ch)

	return nil
}

// Initialize sets up the solver when the webhook starts
func (c *runosDNSProviderSolver) Initialize(kubeClientConfig *rest.Config, stopCh <-chan struct{}) error {
	log.Println("Initializing RunOS DNS webhook solver")

	client, err := kubernetes.NewForConfig(kubeClientConfig)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}
	c.k8sClient = client

	log.Println("RunOS DNS webhook solver initialized successfully")
	return nil
}

// cacheNewCert fetches a newly issued certificate from K8s and caches it in Nodeward
func (c *runosDNSProviderSolver) cacheNewCert(ch *v1alpha1.ChallengeRequest) {
	domain := ch.DNSName
	// Record when the challenge completed so we can distinguish new certs from old ones.
	// Subtract 5 minutes to account for clock skew and Let's Encrypt backdating NotBefore.
	issuedAfter := time.Now().Add(-5 * time.Minute)

	// Retry loop: try for up to 10 minutes (10 attempts * 1 minute)
	var tlsData *tlsDataInternal
	var err error

	for attempt := 1; attempt <= certFetchMaxAttempts; attempt++ {
		log.Printf("Attempting to fetch new cert for %s issued after %s (attempt %d/%d)",
			domain, issuedAfter.Format(time.RFC3339), attempt, certFetchMaxAttempts)

		tlsData, err = c.fetchCertFromK8s(ch, issuedAfter)
		if err == nil && tlsData != nil {
			log.Printf("Successfully fetched new cert for %s on attempt %d", domain, attempt)
			break
		}

		if attempt < certFetchMaxAttempts {
			log.Printf("New cert not ready for %s: %v. Retrying in %v...", domain, err, certFetchRetryDelay)
			time.Sleep(certFetchRetryDelay)
		}
	}

	if tlsData == nil {
		log.Printf("Failed to fetch cert for %s after %d attempts: %v", domain, certFetchMaxAttempts, err)
		return
	}

	// Send to Nodeward for caching
	if err := c.storeCertInCache(domain, tlsData); err != nil {
		log.Printf("Failed to cache cert for %s: %v", domain, err)
		return
	}

	log.Printf("Successfully cached cert for: %s", domain)
}

// tlsDataInternal holds TLS certificate data for internal use
type tlsDataInternal struct {
	TLSCert []byte
	TLSKey  []byte
	CACert  []byte
}

// fetchCertFromK8s fetches the certificate from Kubernetes after it's been issued.
// It searches all namespaces for TLS secrets that contain the domain in the certificate.
// Only certs with NotBefore >= issuedAfter are accepted, to avoid picking up stale certs during renewal.
func (c *runosDNSProviderSolver) fetchCertFromK8s(ch *v1alpha1.ChallengeRequest, issuedAfter time.Time) (*tlsDataInternal, error) {
	if c.k8sClient == nil {
		return nil, fmt.Errorf("kubernetes client not initialized")
	}

	domain := ch.DNSName
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Search all namespaces for TLS secrets
	secrets, err := c.k8sClient.CoreV1().Secrets("").List(ctx, metav1.ListOptions{
		FieldSelector: "type=kubernetes.io/tls",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list TLS secrets: %w", err)
	}

	// Look for a secret containing a certificate for this domain
	for _, secret := range secrets.Items {
		tlsCert, ok := secret.Data["tls.crt"]
		if !ok || len(tlsCert) == 0 {
			continue
		}

		// Check if this certificate covers our domain and is newly issued
		if c.certMatchesDomain(tlsCert, domain) {
			if !certIssuedAfter(tlsCert, issuedAfter) {
				log.Printf("Found cert for %s in %s/%s but it predates the challenge, skipping (waiting for renewed cert)",
					domain, secret.Namespace, secret.Name)
				continue
			}

			tlsKey, ok := secret.Data["tls.key"]
			if !ok || len(tlsKey) == 0 {
				continue
			}

			log.Printf("Found matching new certificate in secret %s/%s for domain %s", secret.Namespace, secret.Name, domain)

			return &tlsDataInternal{
				TLSCert: tlsCert,
				TLSKey:  tlsKey,
				CACert:  secret.Data["ca.crt"], // May be empty
			}, nil
		}
	}

	return nil, fmt.Errorf("no TLS secret found containing certificate for domain %s", domain)
}

// certIssuedAfter checks if a PEM-encoded certificate's NotBefore is at or after the given time.
// This is used to skip stale certs during renewal and only accept the newly issued one.
func certIssuedAfter(certPEM []byte, after time.Time) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}

	return !cert.NotBefore.Before(after)
}

// certMatchesDomain checks if a PEM-encoded certificate covers the given domain
func (c *runosDNSProviderSolver) certMatchesDomain(certPEM []byte, domain string) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}

	// Check if the domain matches the CN or any SAN
	if cert.Subject.CommonName == domain {
		return true
	}

	for _, san := range cert.DNSNames {
		if san == domain {
			return true
		}
		// Check wildcard match: *.example.com matches foo.example.com
		if strings.HasPrefix(san, "*.") {
			wildcardBase := san[2:] // Remove "*."
			if strings.HasSuffix(domain, wildcardBase) {
				// Ensure it's a direct subdomain match, not deeper nesting
				domainPrefix := strings.TrimSuffix(domain, wildcardBase)
				domainPrefix = strings.TrimSuffix(domainPrefix, ".")
				if !strings.Contains(domainPrefix, ".") {
					return true
				}
			}
		}
	}

	return false
}

// storeCertInCache sends the certificate to Nodeward for caching
func (c *runosDNSProviderSolver) storeCertInCache(domain string, tlsData *tlsDataInternal) error {
	// Base64 encode the certificate data
	tlsCertB64 := base64.StdEncoding.EncodeToString(tlsData.TLSCert)
	tlsKeyB64 := base64.StdEncoding.EncodeToString(tlsData.TLSKey)
	caCertB64 := ""
	if len(tlsData.CACert) > 0 {
		caCertB64 = base64.StdEncoding.EncodeToString(tlsData.CACert)
	}

	msg := instructions.StoreCertToServer(domain, tlsCertB64, tlsKeyB64, caCertB64)

	response, err := agentstream.SendAndWaitForResponseWithTimeout(msg, cacheRequestTimeout)
	if err != nil {
		return fmt.Errorf("failed to send cert to cache: %w", err)
	}

	if response.Type == "ERROR" {
		return fmt.Errorf("server returned error when caching cert")
	}

	var storeResponse instructions.StoreCertResponse
	if err := commons.JsonB64Decode(response.JsonB64, &storeResponse); err != nil {
		return fmt.Errorf("failed to decode store cert response: %w", err)
	}

	if !storeResponse.Success {
		return fmt.Errorf("failed to cache cert: %s", storeResponse.Message)
	}

	return nil
}

// loadConfig decodes the JSON configuration
func loadConfig(cfgJSON *extapi.JSON) (runosDNSProviderConfig, error) {
	cfg := runosDNSProviderConfig{}
	// Handle the case where no configuration has been provided
	if cfgJSON == nil {
		return cfg, nil
	}
	if err := json.Unmarshal(cfgJSON.Raw, &cfg); err != nil {
		return cfg, fmt.Errorf("error decoding solver config: %v", err)
	}

	return cfg, nil
}
