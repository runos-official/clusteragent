package agentstream

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// genSelfSignedCertPEM returns a freshly generated self-signed certificate
// rendered as a PEM "CERTIFICATE" block, along with its NotAfter.
func genSelfSignedCertPEM(t *testing.T) (certPEM []byte, notAfter time.Time) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}

	notAfter = time.Now().Add(48 * time.Hour).Truncate(time.Second)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-cluster-agent"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return certPEM, notAfter
}

// TestParseCertExpiry pins the three outcomes of ParseCertExpiry:
//   - a valid self-signed cert PEM returns that cert's NotAfter
//   - non-PEM input fails at pem.Decode (nil block)
//   - a valid PEM block whose bytes are not a certificate fails at x509 parse
func TestParseCertExpiry(t *testing.T) {
	certPEM, wantNotAfter := genSelfSignedCertPEM(t)

	t.Run("happy path returns NotAfter", func(t *testing.T) {
		got, err := ParseCertExpiry(&TLSData{TLSCert: certPEM})
		if err != nil {
			t.Fatalf("ParseCertExpiry: unexpected error: %v", err)
		}
		if !got.Equal(wantNotAfter) {
			t.Errorf("NotAfter = %s, want %s", got, wantNotAfter)
		}
	})

	t.Run("non-PEM input errors on decode", func(t *testing.T) {
		_, err := ParseCertExpiry(&TLSData{TLSCert: []byte("this is not a PEM block")})
		if err == nil {
			t.Fatal("expected an error on non-PEM input, got nil")
		}
	})

	t.Run("valid PEM that is not a cert errors on x509 parse", func(t *testing.T) {
		// A well-formed PEM block whose contents are not a DER certificate.
		notACert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a der cert")})
		_, err := ParseCertExpiry(&TLSData{TLSCert: notACert})
		if err == nil {
			t.Fatal("expected an x509 parse error, got nil")
		}
	})
}
