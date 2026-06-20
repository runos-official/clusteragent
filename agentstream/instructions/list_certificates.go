package instructions

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/runos-official/clusteragent/commons"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	cmclient "github.com/cert-manager/cert-manager/pkg/client/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

var (
	cmClientset     *cmclient.Clientset
	cmClientsetOnce sync.Once
	cmClientsetErr  error
)

func getCMClientset() (*cmclient.Clientset, error) {
	cmClientsetOnce.Do(func() {
		config, err := rest.InClusterConfig()
		if err != nil {
			cmClientsetErr = err
			return
		}
		cmClientset, cmClientsetErr = cmclient.NewForConfig(config)
	})
	return cmClientset, cmClientsetErr
}

type CertificateEntry struct {
	Name       string   `json:"name"`
	Namespace  string   `json:"namespace"`
	DNSNames   []string `json:"dns_names"`
	SecretName string   `json:"secret_name"`
	IssuerName string   `json:"issuer_name"`
	Ready      bool     `json:"ready"`
	Reason     string   `json:"reason"`
	Message    string   `json:"message"`
	NotBefore  string   `json:"not_before"`
	NotAfter   string   `json:"not_after"`
}

type ListCertificatesResponse struct {
	Certificates []CertificateEntry `json:"certificates"`
}

func ListCertificates(jsonB64 string) (string, string, error) {
	clientset, err := getCMClientset()
	if err != nil {
		log.Printf("Failed to get cert-manager clientset: %v", err)
		return "", "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	certList, err := clientset.CertmanagerV1().Certificates("").List(ctx, metav1.ListOptions{
		ResourceVersion: "0",
	})
	if err != nil {
		log.Printf("Failed to list certificates: %v", err)
		return "", "", err
	}

	entries := make([]CertificateEntry, 0, len(certList.Items))
	for _, cert := range certList.Items {
		entry := CertificateEntry{
			Name:       cert.Name,
			Namespace:  cert.Namespace,
			DNSNames:   cert.Spec.DNSNames,
			SecretName: cert.Spec.SecretName,
			IssuerName: cert.Spec.IssuerRef.Name,
			Ready:      false,
			Reason:     "Unknown",
			Message:    "",
		}

		if entry.DNSNames == nil {
			entry.DNSNames = []string{}
		}

		for _, cond := range cert.Status.Conditions {
			if cond.Type == cmv1.CertificateConditionReady {
				entry.Ready = cond.Status == cmmeta.ConditionTrue
				entry.Reason = cond.Reason
				entry.Message = cond.Message
				break
			}
		}

		if cert.Status.NotBefore != nil {
			entry.NotBefore = cert.Status.NotBefore.UTC().Format(time.RFC3339)
		}
		if cert.Status.NotAfter != nil {
			entry.NotAfter = cert.Status.NotAfter.UTC().Format(time.RFC3339)
		}

		entries = append(entries, entry)
	}

	resp := ListCertificatesResponse{Certificates: entries}
	respB64, err := commons.JsonB64Encode(resp)
	if err != nil {
		return "", "", err
	}

	return "LIST_CERTIFICATES_RESPONSE", respB64, nil
}
