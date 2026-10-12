package agentstream

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"log"
)

// runosOIDArc is the private OID arc under which nodeward writes the version
// and type extensions of every agent certificate (nodeward uc/cert/version.go).
var runosOIDArc = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999}

// serverTLSConfig builds the agent's TLS config for a server it reaches with
// the agent CA as trust root. Go verifies the chain and the server name as
// usual. VerifyConnection then adds the T-50 check on top.
func serverTLSConfig(tlsData *TLSData, serverHost string) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(tlsData.TLSCert, tlsData.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load client cert: %v", err)
	}

	ca := x509.NewCertPool()
	if !ca.AppendCertsFromPEM(tlsData.CACert) {
		return nil, fmt.Errorf("failed to append CA cert to pool")
	}

	return &tls.Config{
		InsecureSkipVerify: false,
		ServerName:         serverHost,
		Certificates:       []tls.Certificate{cert},
		RootCAs:            ca,
		MinVersion:         tls.VersionTLS12,
		VerifyConnection:   verifyServerConnection,
	}, nil
}

// verifyServerConnection runs after Go's chain and name checks passed. It
// refuses a server leaf that could be an agent certificate (T-50, FPL315):
// the agent CA also signs agent certificates, so a stolen agent key must not
// be able to pose as the server.
func verifyServerConnection(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("server sent no certificate")
	}
	if err := checkServerLeaf(cs.PeerCertificates[0]); err != nil {
		log.Printf("ERROR refusing server certificate for %s: %v", cs.ServerName, err)
		return err
	}
	return nil
}

// checkServerLeaf refuses a leaf with ClientAuth or with any extension under
// the RunOS arc. Today's nodeward server certificate has neither.
func checkServerLeaf(leaf *x509.Certificate) error {
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageClientAuth {
			return fmt.Errorf("server certificate %q has ClientAuth, which only agent certificates carry", leaf.Subject.CommonName)
		}
	}
	for _, ext := range leaf.Extensions {
		if underArc(ext.Id, runosOIDArc) {
			return fmt.Errorf("server certificate %q has extension %s under the RunOS arc %s, which only agent certificates carry",
				leaf.Subject.CommonName, ext.Id, runosOIDArc)
		}
	}
	return nil
}

// underArc reports whether oid is arc itself or lies below it.
func underArc(oid, arc asn1.ObjectIdentifier) bool {
	if len(oid) < len(arc) {
		return false
	}
	return oid[:len(arc)].Equal(arc)
}
