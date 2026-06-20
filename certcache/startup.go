// Package certcache mirrors the cluster-domain wildcard certificate from the
// control plane into the in-cluster Traefik Secret at startup and keeps it
// fresh, so ingress TLS termination has the current cert without a per-request
// round trip to the platform.
package certcache

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"time"

	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/agentstream/instructions"
	"github.com/runos-official/clusteragent/commons"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// Target secret location for cluster domain certificate
	clusterDomainSecretName      = "cluster-domain"
	clusterDomainSecretNamespace = "traefik"

	// RunOS namespace and ConfigMap
	runosNamespace = "runos"
	runosConfigMap = "runos-config"

	// Timeout for cache request
	cacheRequestTimeout = 30 * time.Second

	// Retry configuration for ALREADY_IN_PROGRESS responses
	// Nodeward's stale pending threshold is 10 minutes, so we wait up to that
	certRetryInterval    = 45 * time.Second // Retry every 45 seconds (within 30-60s recommendation)
	certMaxRetryDuration = 10 * time.Minute // Max wait time before giving up
)

// CheckAndRestoreClusterDomainCert checks if a cached certificate exists for the cluster domain
// and restores it to the traefik/cluster-domain secret if found.
// This should be called at startup after the agentstream connection is established.
func CheckAndRestoreClusterDomainCert() {
	// Get cluster domain from ConfigMap
	clusterDomain, err := getClusterDomainFromConfigMap()
	if err != nil {
		log.Printf("Failed to get cluster domain from ConfigMap: %v", err)
		return
	}

	if clusterDomain == "" {
		log.Printf("Cluster domain not set in ConfigMap, skipping cert cache check")
		return
	}

	log.Printf("Checking cache for cluster domain certificate: %s", clusterDomain)

	// Request cached cert from Nodeward
	cachedCert, err := requestCachedCert(clusterDomain)
	if err != nil {
		log.Printf("Failed to check cache for cluster domain cert: %v", err)
		return
	}

	if cachedCert == nil || !cachedCert.Found {
		log.Printf("No cached certificate found for cluster domain: %s", clusterDomain)
		return
	}

	log.Printf("Found cached certificate for cluster domain: %s", clusterDomain)

	// Store the cert in K8s
	if err := storeCertInK8s(cachedCert, clusterDomain); err != nil {
		log.Printf("Failed to store cached cluster domain cert: %v", err)
		return
	}

	log.Printf("Successfully restored cached certificate to %s/%s", clusterDomainSecretNamespace, clusterDomainSecretName)
}

func getClusterDomainFromConfigMap() (string, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return "", fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", fmt.Errorf("failed to create clientset: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cm, err := clientset.CoreV1().ConfigMaps(runosNamespace).Get(ctx, runosConfigMap, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get ConfigMap: %w", err)
	}

	return cm.Data["cd"], nil
}

func requestCachedCert(domain string) (*instructions.GetCachedCertResponse, error) {
	startTime := time.Now()

	for {
		msg := instructions.GetCachedCertToServer(domain)

		response, err := agentstream.SendAndWaitForResponseWithTimeout(msg, cacheRequestTimeout)
		if err != nil {
			return nil, err
		}

		if response.Type == "ERROR" {
			return nil, nil // Treat as cache miss
		}

		var certResponse instructions.GetCachedCertResponse
		if err := commons.JsonB64Decode(response.JsonB64, &certResponse); err != nil {
			return nil, err
		}

		// Handle pending flag (Nodeward sends pending: true when provisioning is in-flight)
		if certResponse.Pending {
			elapsed := time.Since(startTime)
			if elapsed >= certMaxRetryDuration {
				log.Printf("Cert provisioning still pending after %v, giving up wait - cert-manager will provision if needed", elapsed)
				return nil, nil
			}
			log.Printf("Cert provisioning pending for %s, waiting %v before retry (elapsed: %v)",
				domain, certRetryInterval, elapsed.Round(time.Second))
			time.Sleep(certRetryInterval)
			continue
		}

		// Handle response based on error code
		switch certResponse.ErrorCode {
		case "", instructions.CertErrorCached:
			// Success - cert is available (empty error code means success for backwards compat)
			if certResponse.Found || certResponse.Success {
				return &certResponse, nil
			}
			// No cert found, no provisioning in progress
			return nil, nil

		case instructions.CertErrorAlreadyInProgress:
			// Another provisioning is in progress, wait and retry
			elapsed := time.Since(startTime)
			if elapsed >= certMaxRetryDuration {
				log.Printf("Cert provisioning still in progress after %v, giving up wait - cert-manager will provision if needed", elapsed)
				return nil, nil // Return nil to allow cert-manager to take over
			}
			log.Printf("Cert provisioning in progress for %s, waiting %v before retry (elapsed: %v)",
				domain, certRetryInterval, elapsed.Round(time.Second))
			time.Sleep(certRetryInterval)
			continue // Retry

		case instructions.CertErrorInvalidRequest:
			// Don't retry invalid requests
			return nil, fmt.Errorf("invalid cert request: %s", certResponse.Message)

		case instructions.CertErrorDNSPropagation, instructions.CertErrorACME,
			instructions.CertErrorTimeout, instructions.CertErrorStorage:
			// These indicate provisioning failed - return nil to allow cert-manager to provision
			log.Printf("Cert cache returned error %s: %s", certResponse.ErrorCode, certResponse.Message)
			return nil, nil

		default:
			// Unknown error code, treat as cache miss
			log.Printf("Unknown cert cache error code: %s", certResponse.ErrorCode)
			return nil, nil
		}
	}
}

func storeCertInK8s(cert *instructions.GetCachedCertResponse, clusterDomain string) error {
	// Create K8s client
	config, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	// Decode the certificate data from base64
	tlsCert, err := base64.StdEncoding.DecodeString(cert.TLSCert)
	if err != nil {
		return err
	}

	tlsKey, err := base64.StdEncoding.DecodeString(cert.TLSKey)
	if err != nil {
		return err
	}

	var caCert []byte
	if cert.CACert != "" {
		caCert, err = base64.StdEncoding.DecodeString(cert.CACert)
		if err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Build the secret data
	secretData := map[string][]byte{
		"tls.crt": tlsCert,
		"tls.key": tlsKey,
	}
	if len(caCert) > 0 {
		secretData["ca.crt"] = caCert
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterDomainSecretName,
			Namespace: clusterDomainSecretNamespace,
			Labels: map[string]string{
				"controller.cert-manager.io/fao": "true",
			},
			Annotations: map[string]string{
				"cert-manager.io/issuer-name":      "letsencrypt-prod-runos",
				"cert-manager.io/issuer-kind":      "ClusterIssuer",
				"cert-manager.io/issuer-group":     "",
				"cert-manager.io/certificate-name": "cluster-domain",
				"cert-manager.io/common-name":      "*." + clusterDomain,
				"cert-manager.io/alt-names":        "*." + clusterDomain,
				"cert-manager.io/ip-sans":          "",
				"cert-manager.io/uri-sans":         "",
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: secretData,
	}

	// Ensure the namespace exists
	_, err = clientset.CoreV1().Namespaces().Get(ctx, clusterDomainSecretNamespace, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: clusterDomainSecretNamespace,
				},
			}
			_, err = clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("failed to create namespace %s: %w", clusterDomainSecretNamespace, err)
			}
			log.Printf("Created namespace %s", clusterDomainSecretNamespace)
		} else {
			return fmt.Errorf("failed to check namespace: %w", err)
		}
	}

	// Try to create or update the secret
	_, err = clientset.CoreV1().Secrets(clusterDomainSecretNamespace).Get(ctx, clusterDomainSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			_, err = clientset.CoreV1().Secrets(clusterDomainSecretNamespace).Create(ctx, secret, metav1.CreateOptions{})
			if err != nil {
				return err
			}
			log.Printf("Created TLS secret %s/%s from cache", clusterDomainSecretNamespace, clusterDomainSecretName)
		} else {
			return err
		}
	} else {
		_, err = clientset.CoreV1().Secrets(clusterDomainSecretNamespace).Update(ctx, secret, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		log.Printf("Updated TLS secret %s/%s from cache", clusterDomainSecretNamespace, clusterDomainSecretName)
	}

	return nil
}
