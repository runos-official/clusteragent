package agentstream

// Certificate issuance from a certificate signing request (FPL315).
//
// The agent makes its own ECDSA P-256 key, sends only a CSR, and proves that
// it holds the key of the leaf it connects with. Nodeward answers with the
// certificate only. An old nodeward ignores the new fields and answers with a
// key it made, as before; the agent then stores that answer as today.
// Fallback is decided by the response content only, never by an error.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	mintProofLabel     = "runos-mint-proof-v1"
	mintProofNonceSize = 32
	// skewRefusalPrefix starts the nodeward refusal for a proof time outside
	// its window. The message ends with "server time <unix seconds>".
	skewRefusalPrefix = "mint proof time is outside the 5 minute window"
	skewServerTimeTag = "server time "
)

// mintResponse holds the fields of either minting response.
type mintResponse struct {
	CertificatePem string
	PrivateKey     string
	PublicKey      string
	CaCert         string
}

// mintCall sends one minting request with the given CSR and proof.
type mintCall func(ctx context.Context, csrPem string, proof *l2sec.MintProof) (mintResponse, error)

// legacyMintLogged makes the old-nodeward notice appear once per process.
var legacyMintLogged atomic.Bool

// requestClusterAgentCredentials gets the cluster agent its first
// credentials. presented is the node agent leaf the connection uses, and its
// key signs the proof.
func requestClusterAgentCredentials(ctx context.Context, client l2sec.NodewardClient, presented *TLSData) (*TLSData, error) {
	return mintWithCSR(ctx, l2sec.Nodeward_GetClusterAgentCredentials_FullMethodName, presented, time.Now,
		func(ctx context.Context, csrPem string, proof *l2sec.MintProof) (mintResponse, error) {
			res, err := client.GetClusterAgentCredentials(ctx, &l2sec.GetClusterAgentCredentialsRequest{CsrPem: csrPem, Proof: proof})
			if err != nil {
				return mintResponse{}, err
			}
			return mintResponse{CertificatePem: res.GetCertificatePem(), PrivateKey: res.GetPrivateKey(),
				PublicKey: res.GetPublicKey(), CaCert: res.GetCaCert()}, nil
		})
}

// requestRenewedCertificate renews the cluster agent certificate. presented
// is the current cluster agent leaf, and its key signs the proof.
func requestRenewedCertificate(ctx context.Context, client l2sec.NodewardClient, presented *TLSData) (*TLSData, error) {
	return mintWithCSR(ctx, l2sec.Nodeward_RegenerateCertificate_FullMethodName, presented, time.Now,
		func(ctx context.Context, csrPem string, proof *l2sec.MintProof) (mintResponse, error) {
			res, err := client.RegenerateCertificate(ctx, &l2sec.RegenerateCertificateRequest{CsrPem: csrPem, Proof: proof})
			if err != nil {
				return mintResponse{}, err
			}
			return mintResponse{CertificatePem: res.GetCertificatePem(), PrivateKey: res.GetPrivateKey(),
				PublicKey: res.GetPublicKey(), CaCert: res.GetCaCert()}, nil
		})
}

// mintWithCSR makes a fresh key and CSR, sends them with a proof, and checks
// the answer. One retry is allowed: a clock skew refusal carries the server
// time, and the agent retries once with that time and a new nonce.
func mintWithCSR(ctx context.Context, method string, presented *TLSData, now func() time.Time, call mintCall) (*TLSData, error) {
	key, csrPem, csrDER, err := newAgentKeyAndCSR()
	if err != nil {
		return nil, err
	}
	log.Printf("Requesting a certificate for a local key (public key sha256 %s)", publicKeyID(&key.PublicKey))

	proof, err := newMintProof(presented, method, csrDER, now().Unix())
	if err != nil {
		return nil, err
	}
	res, err := call(ctx, csrPem, proof)
	if serverTime, skewed := serverTimeFromSkewRefusal(err); skewed {
		refusedAt := now()
		log.Printf("Nodeward refused the proof time (server time %d, local time %d); retrying once with the server time",
			serverTime, refusedAt.Unix())
		proof, perr := newMintProof(presented, method, csrDER, serverTime+int64(now().Sub(refusedAt).Seconds()))
		if perr != nil {
			return nil, perr
		}
		res, err = call(ctx, csrPem, proof)
	}
	if err != nil {
		return nil, err
	}
	return acceptMintResponse(key, res, now())
}

// newAgentKeyAndCSR makes a P-256 key and a CSR for it. Nodeward ignores the
// CSR subject, so a fixed name is enough.
func newAgentKeyAndCSR() (*ecdsa.PrivateKey, string, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", nil, fmt.Errorf("generate agent key: %w", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: "runos-agent"},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}, key)
	if err != nil {
		return nil, "", nil, fmt.Errorf("create certificate signing request: %w", err)
	}
	csrPem := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	return key, csrPem, der, nil
}

// mintProofInput builds the six line signing input of the contract, with no
// trailing newline.
func mintProofInput(method string, csrDER, leafDER []byte, unixSeconds int64, nonce []byte) []byte {
	csrSum := sha256.Sum256(csrDER)
	leafSum := sha256.Sum256(leafDER)
	return []byte(strings.Join([]string{
		mintProofLabel,
		method,
		hex.EncodeToString(csrSum[:]),
		strconv.FormatInt(unixSeconds, 10),
		hex.EncodeToString(nonce),
		hex.EncodeToString(leafSum[:]),
	}, "\n"))
}

// newMintProof signs the proof with the key of the presented leaf. The leaf
// key type picks the scheme: ECDSA SHA-256, or RSA-PSS SHA-256.
func newMintProof(presented *TLSData, method string, csrDER []byte, unixSeconds int64) (*l2sec.MintProof, error) {
	pair, err := tls.X509KeyPair(presented.TLSCert, presented.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("load the presented certificate for the mint proof: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return nil, errors.New("presented certificate has no leaf")
	}
	leafDER := pair.Certificate[0]
	nonce := make([]byte, mintProofNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("make mint proof nonce: %w", err)
	}
	digest := sha256.Sum256(mintProofInput(method, csrDER, leafDER, unixSeconds, nonce))

	var sig []byte
	switch k := pair.PrivateKey.(type) {
	case *ecdsa.PrivateKey:
		sig, err = ecdsa.SignASN1(rand.Reader, k, digest[:])
	case *rsa.PrivateKey:
		sig, err = rsa.SignPSS(rand.Reader, k, crypto.SHA256, digest[:],
			&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	default:
		return nil, fmt.Errorf("mint proof: unsupported key type %T", pair.PrivateKey)
	}
	if err != nil {
		return nil, fmt.Errorf("sign mint proof: %w", err)
	}
	return &l2sec.MintProof{UnixSeconds: unixSeconds, Nonce: nonce, Signature: sig}, nil
}

// serverTimeFromSkewRefusal returns the server time from a nodeward refusal
// of the proof time. Any other error returns false.
func serverTimeFromSkewRefusal(err error) (int64, bool) {
	if err == nil {
		return 0, false
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition || !strings.HasPrefix(st.Message(), skewRefusalPrefix) {
		return 0, false
	}
	i := strings.LastIndex(st.Message(), skewServerTimeTag)
	if i < 0 {
		return 0, false
	}
	v, perr := strconv.ParseInt(strings.TrimSpace(st.Message()[i+len(skewServerTimeTag):]), 10, 64)
	if perr != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// acceptMintResponse turns a minting response into credentials to store.
// It returns an error, and the caller keeps its old credentials, when the
// response is not usable.
func acceptMintResponse(key *ecdsa.PrivateKey, res mintResponse, now time.Time) (*TLSData, error) {
	if res.CertificatePem == "" {
		if res.PrivateKey == "" || res.PublicKey == "" {
			return nil, errors.New("nodeward returned neither a certificate nor a legacy key")
		}
		if legacyMintLogged.CompareAndSwap(false, true) {
			log.Println("nodeward returned a legacy key; CSR not supported")
		}
		return &TLSData{
			TLSCert: []byte(res.PublicKey),
			TLSKey:  []byte(res.PrivateKey),
			CACert:  []byte(res.CaCert),
		}, nil
	}

	if res.PrivateKey != "" || res.PublicKey != "" {
		log.Println("WARNING: nodeward returned a certificate and a legacy key; using the local key")
	}
	if err := checkMintedCertificate(key, res.CertificatePem, res.CaCert, now); err != nil {
		return nil, fmt.Errorf("nodeward certificate refused: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode agent key: %w", err)
	}
	return &TLSData{
		TLSCert: []byte(res.CertificatePem),
		TLSKey:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		CACert:  []byte(res.CaCert),
	}, nil
}

// checkMintedCertificate checks that certPem is one certificate for key,
// valid for client auth, not expired, and signed by caPem.
func checkMintedCertificate(key *ecdsa.PrivateKey, certPem, caPem string, now time.Time) error {
	block, rest := pem.Decode([]byte(certPem))
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("not a PEM certificate")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("more than one PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse certificate: %w", err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return errors.New("certificate is not for the local key")
	}
	hasClientAuth := false
	for _, u := range cert.ExtKeyUsage {
		if u == x509.ExtKeyUsageClientAuth {
			hasClientAuth = true
		}
	}
	if !hasClientAuth {
		return errors.New("certificate lacks client auth")
	}
	if !now.Before(cert.NotAfter) {
		return errors.New("certificate is expired")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(caPem)) {
		return errors.New("no CA certificate in the response")
	}
	// Nodeward sets NotBefore to its own clock. A local clock a little behind
	// must not refuse a fresh certificate, so the chain is checked at
	// NotBefore in that case. Expiry was checked above.
	at := now
	if at.Before(cert.NotBefore) {
		at = cert.NotBefore
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: at,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return fmt.Errorf("certificate does not verify against the CA: %w", err)
	}
	return nil
}

// publicKeyID is a short hash of the public key, safe to log.
func publicKeyID(pub crypto.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "unknown"
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:6])
}
