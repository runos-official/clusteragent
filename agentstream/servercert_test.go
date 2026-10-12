package agentstream

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const testServerHost = "nodeward.test.invalid"

// testCA is a throwaway CA that signs the leaves in these tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// leafSpec describes one leaf. The defaults match today's nodeward L2SEC
// server certificate: ServerAuth only and no RunOS extension.
type leafSpec struct {
	dnsNames []string
	eku      []x509.ExtKeyUsage
	extra    []pkix.Extension
}

func (ca *testCA) leaf(t *testing.T, spec leafSpec) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "leaf"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           spec.eku,
		BasicConstraintsValid: true,
		DNSNames:              spec.dnsNames,
		ExtraExtensions:       spec.extra,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// Agent certificate extensions, as nodeward's uc/cert/version.go writes them.
var (
	testAgentVersionExt = pkix.Extension{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1, 1}, Value: []byte{2}}
	testAgentTypeExt    = pkix.Extension{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1, 2}, Value: []byte{1}}
)

// handshake runs one TLS handshake: the agent side uses serverTLSConfig, the
// server side presents serverCert. It returns the agent side's error.
func handshake(t *testing.T, agentCA *testCA, serverHost string, serverCert, serverKey []byte) error {
	t.Helper()
	agentCert, agentKey := agentCA.leaf(t, leafSpec{
		eku:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		extra: []pkix.Extension{testAgentVersionExt, testAgentTypeExt},
	})
	cfg, err := serverTLSConfig(&TLSData{TLSCert: agentCert, TLSKey: agentKey, CACert: agentCA.pem}, serverHost)
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}
	if cfg.InsecureSkipVerify {
		t.Fatal("serverTLSConfig must never skip verification")
	}

	pair, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	// A loopback listener, not net.Pipe: a pipe has no buffer, so an alert
	// write on a refused handshake can block until the deadline.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	go func() {
		c, err := lis.Accept()
		if err != nil {
			return
		}
		srv := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{pair}})
		_ = srv.SetDeadline(time.Now().Add(10 * time.Second))
		_ = srv.Handshake()
		srv.Close()
	}()

	clientConn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	cli := tls.Client(clientConn, cfg)
	_ = cli.SetDeadline(time.Now().Add(10 * time.Second))
	return cli.Handshake()
}

// TestServerTLSConfigT50 pins the T-50 check (FPL315, F5 path): the agent
// refuses a server leaf that could be an agent certificate.
func TestServerTLSConfigT50(t *testing.T) {
	ca := newTestCA(t)
	other := newTestCA(t)
	serverNames := []string{testServerHost}

	cases := []struct {
		name    string
		signer  *testCA
		host    string
		spec    leafSpec
		wantErr string
	}{
		{
			name:   "today's nodeward server certificate passes",
			signer: ca,
			host:   testServerHost,
			spec:   leafSpec{dnsNames: serverNames, eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}},
		},
		{
			name:    "agent leaf (ServerAuth, ClientAuth, RunOS extensions) is refused",
			signer:  ca,
			host:    testServerHost,
			spec:    leafSpec{dnsNames: serverNames, eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, extra: []pkix.Extension{testAgentVersionExt, testAgentTypeExt}},
			wantErr: "ClientAuth",
		},
		{
			name:    "ClientAuth alone on a server leaf is refused",
			signer:  ca,
			host:    testServerHost,
			spec:    leafSpec{dnsNames: serverNames, eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}},
			wantErr: "ClientAuth",
		},
		{
			name:    "a RunOS extension alone on a server leaf is refused",
			signer:  ca,
			host:    testServerHost,
			spec:    leafSpec{dnsNames: serverNames, eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, extra: []pkix.Extension{testAgentTypeExt}},
			wantErr: "1.3.6.1.4.1.99999",
		},
		{
			name:    "any extension under the RunOS arc is refused",
			signer:  ca,
			host:    testServerHost,
			spec:    leafSpec{dnsNames: serverNames, eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, extra: []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 7}, Value: []byte{0}}}},
			wantErr: "1.3.6.1.4.1.99999",
		},
		{
			name:   "an extension on a sibling arc is not refused",
			signer: ca,
			host:   testServerHost,
			spec:   leafSpec{dnsNames: serverNames, eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, extra: []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 999990, 1}, Value: []byte{0}}}},
		},
		{
			name:    "the CA check stays: a leaf from another CA is refused",
			signer:  other,
			host:    testServerHost,
			spec:    leafSpec{dnsNames: serverNames, eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}},
			wantErr: "unknown authority",
		},
		{
			name:    "the server name check stays: a wrong name is refused",
			signer:  ca,
			host:    "other.test.invalid",
			spec:    leafSpec{dnsNames: serverNames, eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}},
			wantErr: "other.test.invalid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			certPEM, keyPEM := tc.signer.leaf(t, tc.spec)
			err := handshake(t, ca, tc.host, certPEM, keyPEM)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("handshake failed: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("handshake succeeded, want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("handshake error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestServerTLSConfigT50OverGRPC proves the check survives grpc-go's copy of
// the TLS config: a gRPC call to a server that presents an agent leaf fails,
// and a call to today's server certificate gets through the handshake.
func TestServerTLSConfigT50OverGRPC(t *testing.T) {
	ca := newTestCA(t)
	agentCert, agentKey := ca.leaf(t, leafSpec{
		eku:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		extra: []pkix.Extension{testAgentVersionExt, testAgentTypeExt},
	})
	agentTLS := &TLSData{TLSCert: agentCert, TLSKey: agentKey, CACert: ca.pem}

	call := func(t *testing.T, serverCert, serverKey []byte) error {
		t.Helper()
		pair, err := tls.X509KeyPair(serverCert, serverKey)
		if err != nil {
			t.Fatal(err)
		}
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}})))
		l2sec.RegisterNodewardServer(srv, l2sec.UnimplementedNodewardServer{})
		go func() { _ = srv.Serve(lis) }()
		defer srv.Stop()

		cfg, err := serverTLSConfig(agentTLS, testServerHost)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := grpc.NewClient("passthrough:///"+lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err = l2sec.NewNodewardClient(conn).GetControlPlaneNodes(ctx, &l2sec.GetControlPlaneNodesRequest{})
		return err
	}

	goodCert, goodKey := ca.leaf(t, leafSpec{dnsNames: []string{testServerHost}, eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err := call(t, goodCert, goodKey); status.Code(err) != codes.Unimplemented {
		t.Fatalf("today's server certificate: err = %v, want UNIMPLEMENTED from the server", err)
	}

	badCert, badKey := ca.leaf(t, leafSpec{
		dnsNames: []string{testServerHost},
		eku:      []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		extra:    []pkix.Extension{testAgentVersionExt, testAgentTypeExt},
	})
	err := call(t, badCert, badKey)
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "ClientAuth") {
		t.Fatalf("agent leaf as server: err = %v, want UNAVAILABLE naming ClientAuth", err)
	}
}
