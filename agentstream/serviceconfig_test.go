package agentstream

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// TestServiceConfigParses pins that grpc-go accepts the service config.
// grpc-go ignores unknown keys, so this alone does not prove the policy
// applies. TestServiceConfigRetriesOnlyIdempotentMethods proves that.
func TestServiceConfigParses(t *testing.T) {
	conn, err := grpc.NewClient("passthrough:///192.0.2.1:9192",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(nodewardServiceConfig))
	if err != nil {
		t.Fatalf("grpc refused the service config: %v", err)
	}
	conn.Close()
}

// TestServiceConfigSchema decodes the config strictly, so a key at the wrong
// level (the old config nested everything under "serviceConfig") fails.
func TestServiceConfigSchema(t *testing.T) {
	type name struct {
		Service string `json:"service"`
		Method  string `json:"method"`
	}
	type retryPolicy struct {
		MaxAttempts          int      `json:"maxAttempts"`
		InitialBackoff       string   `json:"initialBackoff"`
		MaxBackoff           string   `json:"maxBackoff"`
		BackoffMultiplier    float64  `json:"backoffMultiplier"`
		RetryableStatusCodes []string `json:"retryableStatusCodes"`
	}
	type methodConfig struct {
		Name        []name       `json:"name"`
		RetryPolicy *retryPolicy `json:"retryPolicy"`
	}
	var cfg struct {
		MethodConfig []methodConfig `json:"methodConfig"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(nodewardServiceConfig)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("service config does not match the grpc schema: %v", err)
	}
	if len(cfg.MethodConfig) != 1 || cfg.MethodConfig[0].RetryPolicy == nil {
		t.Fatalf("want one methodConfig with a retryPolicy, got %+v", cfg)
	}
	rp := cfg.MethodConfig[0].RetryPolicy
	if rp.MaxAttempts != 4 || rp.InitialBackoff != "0.5s" || rp.MaxBackoff != "5s" || rp.BackoffMultiplier != 2 {
		t.Errorf("retry policy = %+v, want 4 attempts, 0.5s, 5s, x2", *rp)
	}
	if len(rp.RetryableStatusCodes) != 1 || rp.RetryableStatusCodes[0] != "UNAVAILABLE" {
		t.Errorf("retryable codes = %v, want [UNAVAILABLE]", rp.RetryableStatusCodes)
	}
	for _, n := range cfg.MethodConfig[0].Name {
		if n.Method == "" {
			t.Errorf("name %+v has no method: it would retry the whole service", n)
		}
		switch n.Method {
		case "RegenerateCertificate", "GetClusterAgentCredentials", "GetNodeRegistrationToken",
			"GetInstallCommands", "ManualSync", "UpdateStatus", "AddNodelog", "UpdateNodeInstallationStatus":
			t.Errorf("method %s mints or mutates and must not be retried", n.Method)
		}
	}
}

// countingNodeward answers UNAVAILABLE on every unary call and counts them.
type countingNodeward struct {
	l2sec.UnimplementedNodewardServer
	controlPlane atomic.Int32
	vpn          atomic.Int32
	regenerate   atomic.Int32
	credentials  atomic.Int32
}

func (s *countingNodeward) GetControlPlaneNodes(context.Context, *l2sec.GetControlPlaneNodesRequest) (*l2sec.GetControlPlaneNodesResponse, error) {
	s.controlPlane.Add(1)
	return nil, status.Error(codes.Unavailable, "test")
}

func (s *countingNodeward) GetInstallVpnCommands(context.Context, *l2sec.GetInstallVpnCommandsRequest) (*l2sec.InstallCommandList, error) {
	s.vpn.Add(1)
	return nil, status.Error(codes.Unavailable, "test")
}

func (s *countingNodeward) RegenerateCertificate(context.Context, *l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
	s.regenerate.Add(1)
	return nil, status.Error(codes.Unavailable, "test")
}

func (s *countingNodeward) GetClusterAgentCredentials(context.Context, *l2sec.GetClusterAgentCredentialsRequest) (*l2sec.GetClusterAgentCredentialsResponse, error) {
	s.credentials.Add(1)
	return nil, status.Error(codes.Unavailable, "test")
}

// TestServiceConfigRetriesOnlyIdempotentMethods proves the policy applies: an
// idempotent read gets 4 attempts, a minting call gets exactly 1.
func TestServiceConfigRetriesOnlyIdempotentMethods(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	fake := &countingNodeward{}
	l2sec.RegisterNodewardServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(nodewardServiceConfig))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := l2sec.NewNodewardClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := client.GetControlPlaneNodes(ctx, &l2sec.GetControlPlaneNodesRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("GetControlPlaneNodes err = %v, want UNAVAILABLE", err)
	}
	if _, err := client.GetInstallVpnCommands(ctx, &l2sec.GetInstallVpnCommandsRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("GetInstallVpnCommands err = %v, want UNAVAILABLE", err)
	}
	if _, err := client.RegenerateCertificate(ctx, &l2sec.RegenerateCertificateRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("RegenerateCertificate err = %v, want UNAVAILABLE", err)
	}
	if _, err := client.GetClusterAgentCredentials(ctx, &l2sec.GetClusterAgentCredentialsRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("GetClusterAgentCredentials err = %v, want UNAVAILABLE", err)
	}

	if got := fake.controlPlane.Load(); got != 4 {
		t.Errorf("GetControlPlaneNodes attempts = %d, want 4", got)
	}
	if got := fake.vpn.Load(); got != 4 {
		t.Errorf("GetInstallVpnCommands attempts = %d, want 4", got)
	}
	if got := fake.regenerate.Load(); got != 1 {
		t.Errorf("RegenerateCertificate attempts = %d, want 1 (it mints a certificate)", got)
	}
	if got := fake.credentials.Load(); got != 1 {
		t.Errorf("GetClusterAgentCredentials attempts = %d, want 1 (it mints a certificate)", got)
	}
}
