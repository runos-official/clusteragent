package agentstream

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// mintCA is an RSA CA like the nodeward level 2 CA.
type mintCA struct {
	cert *x509.Certificate
	key  *rsa.PrivateKey
	pem  []byte
}

var (
	mintCAOnce   sync.Once
	sharedMintCA *mintCA
)

func newMintCA(t *testing.T) *mintCA {
	t.Helper()
	mintCAOnce.Do(func() {
		sharedMintCA = makeMintCA(t, "Test L2 CA")
	})
	return sharedMintCA
}

func makeMintCA(t *testing.T, cn string) *mintCA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &mintCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

type leafOpts struct {
	notBefore time.Time
	notAfter  time.Time
	noClient  bool
}

// issue signs a leaf shaped like the nodeward agent leaf for pub.
func (ca *mintCA) issue(t *testing.T, pub crypto.PublicKey, o leafOpts) []byte {
	t.Helper()
	if o.notBefore.IsZero() {
		o.notBefore = time.Now()
	}
	if o.notAfter.IsZero() {
		o.notAfter = time.Now().Add(90 * 24 * time.Hour)
	}
	eku := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	if o.noClient {
		eku = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "RunOS L2Sec", Organization: []string{"RUNOS"}},
		NotBefore:    o.notBefore,
		NotAfter:     o.notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageKeyAgreement,
		ExtKeyUsage:  eku,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// presentedECDSA is a current leaf with a P-256 key, as after a CSR mint.
func presentedECDSA(t *testing.T, ca *mintCA) *TLSData {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &TLSData{
		TLSCert: ca.issue(t, &key.PublicKey, leafOpts{}),
		TLSKey:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		CACert:  ca.pem,
	}
}

// presentedRSA is a current leaf with an RSA key in PKCS#1, as nodeward
// mints it today.
func presentedRSA(t *testing.T, ca *mintCA) *TLSData {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &TLSData{
		TLSCert: ca.issue(t, &key.PublicKey, leafOpts{}),
		TLSKey:  pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		CACert:  ca.pem,
	}
}

func leafDER(t *testing.T, td *TLSData) []byte {
	t.Helper()
	b, _ := pem.Decode(td.TLSCert)
	if b == nil {
		t.Fatal("no leaf PEM")
	}
	return b.Bytes
}

// expectedProofInput builds the contract signing input by hand, so the test
// does not share code with the agent.
func expectedProofInput(method string, csrDER, leaf []byte, unix int64, nonce []byte) []byte {
	csrSum := sha256.Sum256(csrDER)
	leafSum := sha256.Sum256(leaf)
	return []byte("runos-mint-proof-v1\n" + method + "\n" + hex.EncodeToString(csrSum[:]) + "\n" +
		strconv.FormatInt(unix, 10) + "\n" + hex.EncodeToString(nonce) + "\n" + hex.EncodeToString(leafSum[:]))
}

// verifyProof checks a proof the way nodeward does (contract section 5).
func verifyProof(method string, csrDER, leaf []byte, p *l2sec.MintProof) error {
	if p == nil || len(p.GetNonce()) != 32 || len(p.GetSignature()) == 0 || len(p.GetSignature()) > 1024 {
		return fmt.Errorf("mint proof is missing or malformed")
	}
	cert, err := x509.ParseCertificate(leaf)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(expectedProofInput(method, csrDER, leaf, p.GetUnixSeconds(), p.GetNonce()))
	switch pub := cert.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(pub, digest[:], p.GetSignature()) {
			return fmt.Errorf("mint proof signature does not verify")
		}
	case *rsa.PublicKey:
		if err := rsa.VerifyPSS(pub, crypto.SHA256, digest[:], p.GetSignature(),
			&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}); err != nil {
			return fmt.Errorf("mint proof signature does not verify")
		}
	default:
		return fmt.Errorf("unsupported leaf key")
	}
	return nil
}

func parseTestCSR(t *testing.T, csrPem string) *x509.CertificateRequest {
	t.Helper()
	b, rest := pem.Decode([]byte(csrPem))
	if b == nil || b.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("csrPem is not exactly one CERTIFICATE REQUEST block")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return csr
}

func TestMintProofInputMatchesContract(t *testing.T) {
	nonce := bytes.Repeat([]byte{0xab}, 32)
	got := mintProofInput("/m/X", []byte("csr"), []byte("leaf"), 1700000000, nonce)
	want := expectedProofInput("/m/X", []byte("csr"), []byte("leaf"), 1700000000, nonce)
	if !bytes.Equal(got, want) {
		t.Fatalf("signing input\n got %q\nwant %q", got, want)
	}
	if bytes.HasSuffix(got, []byte("\n")) {
		t.Fatal("signing input has a trailing newline")
	}
	if n := bytes.Count(got, []byte("\n")); n != 5 {
		t.Fatalf("signing input has %d newlines, want 5", n)
	}
}

func TestNewAgentCSR(t *testing.T) {
	key, csrPem, csrDER, err := newAgentKeyAndCSR()
	if err != nil {
		t.Fatal(err)
	}
	csr := parseTestCSR(t, csrPem)
	if !bytes.Equal(csr.Raw, csrDER) {
		t.Fatal("csrDER does not match the PEM block")
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR self-signature: %v", err)
	}
	if csr.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Fatalf("signature algorithm = %v", csr.SignatureAlgorithm)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() || !pub.Equal(&key.PublicKey) {
		t.Fatal("CSR key is not the local P-256 key")
	}
}

func TestNewMintProofVerifies(t *testing.T) {
	ca := newMintCA(t)
	for name, td := range map[string]*TLSData{"ecdsa": presentedECDSA(t, ca), "rsa": presentedRSA(t, ca)} {
		t.Run(name, func(t *testing.T) {
			_, _, csrDER, err := newAgentKeyAndCSR()
			if err != nil {
				t.Fatal(err)
			}
			method := l2sec.Nodeward_RegenerateCertificate_FullMethodName
			p, err := newMintProof(td, method, csrDER, 1700000000)
			if err != nil {
				t.Fatal(err)
			}
			if p.GetUnixSeconds() != 1700000000 {
				t.Fatalf("unixSeconds = %d", p.GetUnixSeconds())
			}
			if err := verifyProof(method, csrDER, leafDER(t, td), p); err != nil {
				t.Fatal(err)
			}
			// The signature binds the method and the CSR.
			if verifyProof(l2sec.Nodeward_GetClusterAgentCredentials_FullMethodName, csrDER, leafDER(t, td), p) == nil {
				t.Fatal("proof verified for another method")
			}
			if verifyProof(method, append([]byte{0}, csrDER...), leafDER(t, td), p) == nil {
				t.Fatal("proof verified for another CSR")
			}
			p2, _ := newMintProof(td, method, csrDER, 1700000000)
			if bytes.Equal(p.GetNonce(), p2.GetNonce()) {
				t.Fatal("nonce repeated across calls")
			}
		})
	}
}

func TestNewMintProofRefusesOtherKeyTypes(t *testing.T) {
	ca := newMintCA(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	td := &TLSData{
		TLSCert: ca.issue(t, pub, leafOpts{}),
		TLSKey:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		CACert:  ca.pem,
	}
	if _, err := newMintProof(td, "/m", []byte("csr"), 1); err == nil {
		t.Fatal("ed25519 leaf accepted")
	}
}

func TestAcceptMintResponse(t *testing.T) {
	ca := newMintCA(t)
	otherCA := makeMintCA(t, "Other CA")
	now := time.Now()

	type tc struct {
		name    string
		res     func(key *ecdsa.PrivateKey) mintResponse
		wantErr bool
		ownKey  bool
	}
	cases := []tc{
		{name: "certificate path", ownKey: true, res: func(k *ecdsa.PrivateKey) mintResponse {
			return mintResponse{CertificatePem: string(ca.issue(t, &k.PublicKey, leafOpts{})), CaCert: string(ca.pem)}
		}},
		{name: "certificate path ignores legacy fields set by a bug", ownKey: true, res: func(k *ecdsa.PrivateKey) mintResponse {
			return mintResponse{CertificatePem: string(ca.issue(t, &k.PublicKey, leafOpts{})), CaCert: string(ca.pem),
				PrivateKey: "junk-key", PublicKey: "junk-cert"}
		}},
		{name: "not before slightly ahead of the local clock", ownKey: true, res: func(k *ecdsa.PrivateKey) mintResponse {
			return mintResponse{CertificatePem: string(ca.issue(t, &k.PublicKey, leafOpts{notBefore: now.Add(4 * time.Minute)})), CaCert: string(ca.pem)}
		}},
		{name: "legacy fallback", res: func(*ecdsa.PrivateKey) mintResponse {
			return mintResponse{PrivateKey: "legacy-key", PublicKey: "legacy-cert", CaCert: string(ca.pem)}
		}},
		{name: "both empty", wantErr: true, res: func(*ecdsa.PrivateKey) mintResponse {
			return mintResponse{CaCert: string(ca.pem)}
		}},
		{name: "legacy half set", wantErr: true, res: func(*ecdsa.PrivateKey) mintResponse {
			return mintResponse{PrivateKey: "legacy-key", CaCert: string(ca.pem)}
		}},
		{name: "certificate for another key", wantErr: true, res: func(*ecdsa.PrivateKey) mintResponse {
			other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			return mintResponse{CertificatePem: string(ca.issue(t, &other.PublicKey, leafOpts{})), CaCert: string(ca.pem)}
		}},
		{name: "no client auth", wantErr: true, res: func(k *ecdsa.PrivateKey) mintResponse {
			return mintResponse{CertificatePem: string(ca.issue(t, &k.PublicKey, leafOpts{noClient: true})), CaCert: string(ca.pem)}
		}},
		{name: "expired", wantErr: true, res: func(k *ecdsa.PrivateKey) mintResponse {
			return mintResponse{CertificatePem: string(ca.issue(t, &k.PublicKey, leafOpts{notBefore: now.Add(-48 * time.Hour), notAfter: now.Add(-time.Hour)})), CaCert: string(ca.pem)}
		}},
		{name: "signed by another CA", wantErr: true, res: func(k *ecdsa.PrivateKey) mintResponse {
			return mintResponse{CertificatePem: string(otherCA.issue(t, &k.PublicKey, leafOpts{})), CaCert: string(ca.pem)}
		}},
		{name: "no CA", wantErr: true, res: func(k *ecdsa.PrivateKey) mintResponse {
			return mintResponse{CertificatePem: string(ca.issue(t, &k.PublicKey, leafOpts{}))}
		}},
		{name: "two certificates", wantErr: true, res: func(k *ecdsa.PrivateKey) mintResponse {
			c := ca.issue(t, &k.PublicKey, leafOpts{})
			return mintResponse{CertificatePem: string(c) + string(c), CaCert: string(ca.pem)}
		}},
		{name: "not a certificate", wantErr: true, res: func(*ecdsa.PrivateKey) mintResponse {
			return mintResponse{CertificatePem: "garbage", CaCert: string(ca.pem)}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			res := c.res(key)
			td, err := acceptMintResponse(key, res, now)
			if c.wantErr {
				if err == nil || td != nil {
					t.Fatalf("want error and no credentials, got %v, %v", td, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !c.ownKey {
				if string(td.TLSKey) != res.PrivateKey || string(td.TLSCert) != res.PublicKey || string(td.CACert) != res.CaCert {
					t.Fatal("legacy fields not stored as today")
				}
				return
			}
			if string(td.TLSCert) != res.CertificatePem || string(td.CACert) != res.CaCert {
				t.Fatal("certificate or CA not stored")
			}
			b, _ := pem.Decode(td.TLSKey)
			if b == nil || b.Type != "PRIVATE KEY" {
				t.Fatal("key is not a PKCS#8 PRIVATE KEY block")
			}
			parsed, err := x509.ParsePKCS8PrivateKey(b.Bytes)
			if err != nil || !key.Equal(parsed) {
				t.Fatal("stored key is not the local key")
			}
			if _, err := tls.X509KeyPair(td.TLSCert, td.TLSKey); err != nil {
				t.Fatalf("stored pair does not load: %v", err)
			}
		})
	}
}

func TestServerTimeFromSkewRefusal(t *testing.T) {
	ok := status.Error(codes.FailedPrecondition, "mint proof time is outside the 5 minute window; server time 1700000123")
	if got, found := serverTimeFromSkewRefusal(ok); !found || got != 1700000123 {
		t.Fatalf("got %d, %v", got, found)
	}
	for _, err := range []error{
		nil,
		fmt.Errorf("mint proof time is outside the 5 minute window; server time 1"),
		status.Error(codes.PermissionDenied, "mint proof time is outside the 5 minute window; server time 1"),
		status.Error(codes.FailedPrecondition, "this agent must renew with a certificate signing request"),
		status.Error(codes.FailedPrecondition, "mint proof time is outside the 5 minute window; server time x"),
	} {
		if _, found := serverTimeFromSkewRefusal(err); found {
			t.Fatalf("treated %v as a skew refusal", err)
		}
	}
}

// fakeMintNodeward plays nodeward for the two minting calls.
type fakeMintNodeward struct {
	l2sec.UnimplementedNodewardServer
	t         *testing.T
	ca        *mintCA
	presented []byte // leaf DER the agent connects with
	mode      string // csr, legacy, refuse, skew-once, skew-always
	offset    time.Duration

	mu       sync.Mutex
	calls    int
	requests []string // method per call
	proofs   []*l2sec.MintProof
}

func (s *fakeMintNodeward) mint(method, csrPem string, proof *l2sec.MintProof) (mintResponse, error) {
	s.mu.Lock()
	s.calls++
	s.requests = append(s.requests, method)
	s.proofs = append(s.proofs, proof)
	call := s.calls
	s.mu.Unlock()

	switch s.mode {
	case "refuse":
		return mintResponse{}, status.Error(codes.InvalidArgument, "invalid certificate signing request: test")
	case "legacy":
		// An old nodeward ignores the unknown fields and makes the key.
		key, _ := rsa.GenerateKey(rand.Reader, 2048)
		return mintResponse{
			PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
			PublicKey:  string(s.ca.issue(s.t, &key.PublicKey, leafOpts{})),
			CaCert:     string(s.ca.pem),
		}, nil
	}

	if csrPem == "" {
		return mintResponse{}, status.Error(codes.InvalidArgument, "no csr")
	}
	csr := parseTestCSR(s.t, csrPem)
	if err := csr.CheckSignature(); err != nil {
		return mintResponse{}, status.Error(codes.InvalidArgument, "invalid certificate signing request: signature")
	}
	if err := verifyProof(method, csr.Raw, s.presented, proof); err != nil {
		return mintResponse{}, status.Error(codes.PermissionDenied, err.Error())
	}
	serverNow := time.Now().Add(s.offset).Unix()
	skew := serverNow - proof.GetUnixSeconds()
	if skew < 0 {
		skew = -skew
	}
	if s.mode == "skew-always" || (s.mode == "skew-once" && call == 1) || skew > 300 {
		return mintResponse{}, status.Errorf(codes.FailedPrecondition,
			"mint proof time is outside the 5 minute window; server time %d", serverNow)
	}
	return mintResponse{CertificatePem: string(s.ca.issue(s.t, csr.PublicKey, leafOpts{})), CaCert: string(s.ca.pem)}, nil
}

func (s *fakeMintNodeward) RegenerateCertificate(_ context.Context, r *l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
	res, err := s.mint(l2sec.Nodeward_RegenerateCertificate_FullMethodName, r.GetCsrPem(), r.GetProof())
	if err != nil {
		return nil, err
	}
	return &l2sec.RegenerateCertificateResponse{PrivateKey: res.PrivateKey, PublicKey: res.PublicKey,
		CaCert: res.CaCert, CertificatePem: res.CertificatePem}, nil
}

func (s *fakeMintNodeward) GetClusterAgentCredentials(_ context.Context, r *l2sec.GetClusterAgentCredentialsRequest) (*l2sec.GetClusterAgentCredentialsResponse, error) {
	res, err := s.mint(l2sec.Nodeward_GetClusterAgentCredentials_FullMethodName, r.GetCsrPem(), r.GetProof())
	if err != nil {
		return nil, err
	}
	return &l2sec.GetClusterAgentCredentialsResponse{PrivateKey: res.PrivateKey, PublicKey: res.PublicKey,
		CaCert: res.CaCert, CertificatePem: res.CertificatePem}, nil
}

func startFakeMint(t *testing.T, fake *fakeMintNodeward) l2sec.NodewardClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	l2sec.RegisterNodewardServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(nodewardServiceConfig))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return l2sec.NewNodewardClient(conn)
}

// captureLog sends the standard logger to a buffer for the test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func assertNoKeyMaterialLogged(t *testing.T, logs string) {
	t.Helper()
	for _, bad := range []string{"PRIVATE KEY", "CERTIFICATE REQUEST", "BEGIN"} {
		if strings.Contains(logs, bad) {
			t.Fatalf("log contains %q:\n%s", bad, logs)
		}
	}
}

type mintFn func(context.Context, l2sec.NodewardClient, *TLSData) (*TLSData, error)

func mintMethods() map[string]mintFn {
	return map[string]mintFn{
		l2sec.Nodeward_RegenerateCertificate_FullMethodName:      requestRenewedCertificate,
		l2sec.Nodeward_GetClusterAgentCredentials_FullMethodName: requestClusterAgentCredentials,
	}
}

func TestMintOverGRPCStoresOwnKey(t *testing.T) {
	ca := newMintCA(t)
	for method, fn := range mintMethods() {
		for name, presented := range map[string]*TLSData{"ecdsa leaf": presentedECDSA(t, ca), "rsa leaf": presentedRSA(t, ca)} {
			t.Run(method+" "+name, func(t *testing.T) {
				logs := captureLog(t)
				fake := &fakeMintNodeward{t: t, ca: ca, presented: leafDER(t, presented), mode: "csr"}
				client := startFakeMint(t, fake)
				td, err := fn(context.Background(), client, presented)
				if err != nil {
					t.Fatal(err)
				}
				if fake.calls != 1 || fake.requests[0] != method {
					t.Fatalf("calls = %d %v", fake.calls, fake.requests)
				}
				pair, err := tls.X509KeyPair(td.TLSCert, td.TLSKey)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := pair.PrivateKey.(*ecdsa.PrivateKey); !ok {
					t.Fatal("stored key is not ECDSA")
				}
				if bytes.Equal(td.TLSKey, presented.TLSKey) {
					t.Fatal("presented key reused")
				}
				if !bytes.Equal(td.CACert, ca.pem) {
					t.Fatal("CA not stored")
				}
				assertNoKeyMaterialLogged(t, logs.String())
			})
		}
	}
}

func TestMintOverGRPCLegacyFallback(t *testing.T) {
	ca := newMintCA(t)
	for method, fn := range mintMethods() {
		t.Run(method, func(t *testing.T) {
			logs := captureLog(t)
			presented := presentedRSA(t, ca)
			fake := &fakeMintNodeward{t: t, ca: ca, presented: leafDER(t, presented), mode: "legacy"}
			td, err := fn(context.Background(), startFakeMint(t, fake), presented)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := pem.Decode(td.TLSKey)
			if b == nil || b.Type != "RSA PRIVATE KEY" {
				t.Fatal("legacy key not stored")
			}
			if _, err := tls.X509KeyPair(td.TLSCert, td.TLSKey); err != nil {
				t.Fatal(err)
			}
			assertNoKeyMaterialLogged(t, logs.String())
		})
	}
}

func TestMintOverGRPCErrorNeverFallsBack(t *testing.T) {
	ca := newMintCA(t)
	for method, fn := range mintMethods() {
		t.Run(method, func(t *testing.T) {
			presented := presentedECDSA(t, ca)
			fake := &fakeMintNodeward{t: t, ca: ca, presented: leafDER(t, presented), mode: "refuse"}
			td, err := fn(context.Background(), startFakeMint(t, fake), presented)
			if err == nil || td != nil {
				t.Fatalf("want error, got %v %v", td, err)
			}
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("error lost its status: %v", err)
			}
			if fake.calls != 1 {
				t.Fatalf("calls = %d, want 1 (no retry, no legacy call)", fake.calls)
			}
		})
	}
}

func TestMintOverGRPCSkewRetriesOnce(t *testing.T) {
	ca := newMintCA(t)
	presented := presentedECDSA(t, ca)

	t.Run("agent clock behind by ten minutes", func(t *testing.T) {
		fake := &fakeMintNodeward{t: t, ca: ca, presented: leafDER(t, presented), mode: "csr", offset: 10 * time.Minute}
		td, err := requestRenewedCertificate(context.Background(), startFakeMint(t, fake), presented)
		if err != nil {
			t.Fatal(err)
		}
		if td == nil || fake.calls != 2 {
			t.Fatalf("calls = %d, want 2", fake.calls)
		}
		if bytes.Equal(fake.proofs[0].GetNonce(), fake.proofs[1].GetNonce()) {
			t.Fatal("retry reused the nonce")
		}
		want := time.Now().Add(10 * time.Minute).Unix()
		if d := fake.proofs[1].GetUnixSeconds() - want; d < -5 || d > 5 {
			t.Fatalf("retry unixSeconds off by %d s from server time", d)
		}
	})

	t.Run("second refusal is final", func(t *testing.T) {
		fake := &fakeMintNodeward{t: t, ca: ca, presented: leafDER(t, presented), mode: "skew-always"}
		td, err := requestClusterAgentCredentials(context.Background(), startFakeMint(t, fake), presented)
		if err == nil || td != nil {
			t.Fatal("want error")
		}
		if fake.calls != 2 {
			t.Fatalf("calls = %d, want 2", fake.calls)
		}
	})
}

// TestMintWireFieldNumbers pins the contract field numbers on the wire.
func TestMintWireFieldNumbers(t *testing.T) {
	proof := &l2sec.MintProof{UnixSeconds: 1, Nonce: []byte{1}, Signature: []byte{2}}
	check := func(name string, m proto.Message, want map[protowire.Number]bool) {
		t.Helper()
		b, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		got := map[protowire.Number]bool{}
		for len(b) > 0 {
			num, typ, n := protowire.ConsumeTag(b)
			if n < 0 {
				t.Fatal("bad tag")
			}
			b = b[n:]
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				t.Fatal("bad value")
			}
			b = b[m:]
			got[num] = true
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s fields = %v, want %v", name, got, want)
		}
	}
	check("MintProof", proof, map[protowire.Number]bool{1: true, 2: true, 3: true})
	check("RegenerateCertificateRequest", &l2sec.RegenerateCertificateRequest{CsrPem: "x", Proof: proof}, map[protowire.Number]bool{5: true, 10: true})
	check("GetClusterAgentCredentialsRequest", &l2sec.GetClusterAgentCredentialsRequest{CsrPem: "x", Proof: proof}, map[protowire.Number]bool{5: true, 10: true})
	check("RegenerateCertificateResponse", &l2sec.RegenerateCertificateResponse{CertificatePem: "x"}, map[protowire.Number]bool{20: true})
	check("GetClusterAgentCredentialsResponse", &l2sec.GetClusterAgentCredentialsResponse{CertificatePem: "x"}, map[protowire.Number]bool{75: true})
}
