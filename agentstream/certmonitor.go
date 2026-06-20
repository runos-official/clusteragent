package agentstream

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/runos-official/clusteragent/agentstream/l2sec"
)

const (
	renewalThresholdDays = 30
	certCheckInterval    = 24 * time.Hour
)

// ParseCertExpiry parses the TLS certificate and returns its expiration time
func ParseCertExpiry(tlsData *TLSData) (time.Time, error) {
	block, _ := pem.Decode(tlsData.TLSCert)
	if block == nil {
		return time.Time{}, fmt.Errorf("failed to decode PEM block from TLS cert")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse certificate: %w", err)
	}

	return cert.NotAfter, nil
}

// LogCertStatus logs certificate details at startup
func LogCertStatus(tlsData *TLSData) {
	block, _ := pem.Decode(tlsData.TLSCert)
	if block == nil {
		log.Println("WARNING: Failed to decode certificate PEM block")
		return
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		log.Printf("WARNING: Failed to parse certificate: %v", err)
		return
	}

	now := time.Now()
	daysRemaining := int(cert.NotAfter.Sub(now).Hours() / 24)

	log.Println("=== Cluster Agent Certificate Status ===")
	log.Printf("  Subject:    %s", cert.Subject.CommonName)
	log.Printf("  Issuer:     %s", cert.Issuer.CommonName)
	log.Printf("  Not Before: %s", cert.NotBefore.Format(time.RFC3339))
	log.Printf("  Not After:  %s", cert.NotAfter.Format(time.RFC3339))

	if daysRemaining < 0 {
		log.Printf("  Status:     EXPIRED (%d days ago)", -daysRemaining)
	} else if daysRemaining <= renewalThresholdDays {
		log.Printf("  Status:     EXPIRING SOON (%d days remaining)", daysRemaining)
	} else {
		log.Printf("  Status:     Valid (%d days remaining)", daysRemaining)
	}

	log.Println("=========================================")
}

// startCertMonitor runs a daily check on the certificate and renews if expiring within threshold.
// Also listens for SIGUSR1 to trigger a manual renewal (e.g. kubectl exec -n runos <pod> -- kill -SIGUSR1 1).
func (sm *streamManager) startCertMonitor() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGUSR1)

	// Check immediately on start
	sm.checkAndRenew()

	ticker := time.NewTicker(certCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			sm.checkAndRenew()
		case <-sigChan:
			log.Println("Cert monitor: received SIGUSR1, forcing certificate renewal...")
			if err := sm.renewCert(); err != nil {
				log.Printf("Cert monitor: manual renewal failed: %v", err)
			}
		}
	}
}

func (sm *streamManager) checkAndRenew() {
	// Always log cert status on each daily check
	LogCertStatus(sm.clusterAgentTLS)

	expiry, err := ParseCertExpiry(sm.clusterAgentTLS)
	if err != nil {
		log.Printf("Cert monitor: failed to parse certificate: %v", err)
		return
	}

	daysRemaining := int(time.Until(expiry).Hours() / 24)

	if daysRemaining > renewalThresholdDays {
		return
	}

	if daysRemaining < 0 {
		log.Printf("Cert monitor: certificate has EXPIRED, attempting renewal...")
	} else {
		log.Printf("Cert monitor: certificate expiring within %d days, initiating renewal...", renewalThresholdDays)
	}

	if err := sm.renewCert(); err != nil {
		log.Printf("Cert monitor: renewal failed: %v", err)
	}
}

func (sm *streamManager) renewCert() error {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	// Connect using current credentials to request renewal
	client, conn, err := ConnectToServer(sm.clusterAgentTLS, sm.serverHost, ctx)
	if err != nil {
		return fmt.Errorf("failed to connect for renewal: %w", err)
	}
	defer conn.Close()

	log.Println("Cert monitor: requesting new certificate from Nodeward...")
	res, err := client.RegenerateCertificate(ctx, &l2sec.RegenerateCertificateRequest{})
	if err != nil {
		return fmt.Errorf("RegenerateCertificate RPC failed: %w", err)
	}

	newTLS := &TLSData{
		TLSCert: []byte(res.PublicKey),
		TLSKey:  []byte(res.PrivateKey),
		CACert:  []byte(res.CaCert),
	}

	// Update the Kubernetes secret
	if err := sm.k8s.SetClusterAgentTLS(ctx, newTLS); err != nil {
		return fmt.Errorf("failed to update cluster-agent-tls secret: %w", err)
	}

	// Update in-memory credentials
	sm.clusterAgentTLS = newTLS

	log.Println("Cert monitor: certificate renewed successfully")
	LogCertStatus(newTLS)

	return nil
}
