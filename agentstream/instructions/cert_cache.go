package instructions

import (
	"fmt"
	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"github.com/runos-official/clusteragent/commons"
	"log"
)

// GetCachedCertRequest is the request structure for getting a cached certificate
type GetCachedCertRequest struct {
	Domain string `json:"domain"`
}

// GetCachedCertResponse is the response from Nodeward for a cached cert request
type GetCachedCertResponse struct {
	Success   bool   `json:"success"`              // True if cert was found or just provisioned
	Found     bool   `json:"found"`                // True if cert exists in cache (for backwards compat)
	Pending   bool   `json:"pending"`              // True if cert provisioning is in progress
	ErrorCode string `json:"error_code,omitempty"` // Error code if success is false
	Message   string `json:"message,omitempty"`    // Human-readable message
	TLSCert   string `json:"tlsCert"`              // Base64 encoded PEM
	TLSKey    string `json:"tlsKey"`               // Base64 encoded PEM
	CACert    string `json:"caCert"`               // Base64 encoded PEM (may be empty)
}

// Cert cache error codes from Nodeward
const (
	CertErrorCached            = "CACHED"              // Cert exists and is valid
	CertErrorAlreadyInProgress = "ALREADY_IN_PROGRESS" // Another request is running
	CertErrorDNSPropagation    = "DNS_PROPAGATION_FAILED"
	CertErrorACME              = "ACME_ERROR"
	CertErrorTimeout           = "TIMEOUT"
	CertErrorStorage           = "STORAGE_ERROR"
	CertErrorInvalidRequest    = "INVALID_REQUEST"
)

// StoreCertRequest is the request structure for caching a new certificate
type StoreCertRequest struct {
	Domain  string `json:"domain"`
	TLSCert string `json:"tlsCert"` // Base64 encoded PEM
	TLSKey  string `json:"tlsKey"`  // Base64 encoded PEM
	CACert  string `json:"caCert"`  // Base64 encoded PEM (may be empty)
}

// StoreCertResponse is the response from Nodeward after storing a cert
type StoreCertResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// GetCachedCertToServer creates a message to request a cached certificate from Nodeward
func GetCachedCertToServer(domain string) *l2sec.FromClusterAgent {
	request := GetCachedCertRequest{Domain: domain}

	log.Printf("GetCachedCertToServer requesting cert for domain: %s", domain)

	message, err := commons.JsonB64Encode(request)
	if err != nil {
		fmt.Println("Error encoding GetCachedCert message: ", err)
	}

	return &l2sec.FromClusterAgent{
		Type:    "GetCachedCert",
		JsonB64: message,
	}
}

// StoreCertToServer creates a message to store a certificate in Nodeward's cache
func StoreCertToServer(domain, tlsCert, tlsKey, caCert string) *l2sec.FromClusterAgent {
	request := StoreCertRequest{
		Domain:  domain,
		TLSCert: tlsCert,
		TLSKey:  tlsKey,
		CACert:  caCert,
	}

	log.Printf("StoreCertToServer caching cert for domain: %s", domain)

	message, err := commons.JsonB64Encode(request)
	if err != nil {
		fmt.Println("Error encoding StoreCert message: ", err)
	}

	return &l2sec.FromClusterAgent{
		Type:    "StoreCert",
		JsonB64: message,
	}
}
