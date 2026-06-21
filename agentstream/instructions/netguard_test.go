package instructions

import (
	"context"
	"net"
	"testing"
)

// TestIsBlockedIP pins the SSRF IP gate: loopback, link-local (incl. the cloud
// metadata IP 169.254.169.254), and IPv6 link-local/loopback must be blocked;
// ordinary public AND in-cluster private (RFC1918) IPs must pass — we
// deliberately allow private ranges so legit pod/service/node traffic keeps
// working. nil is blocked. A regression that stops blocking metadata, or starts
// blocking RFC1918, breaks either security or in-cluster web requests.
func TestIsBlockedIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"169.254.169.254", true}, // cloud metadata
		{"169.254.1.1", true},     // link-local v4
		{"127.0.0.1", true},       // loopback v4
		{"::1", true},             // loopback v6
		{"fe80::1", true},         // link-local v6
		{"8.8.8.8", false},        // public
		{"1.1.1.1", false},        // public
		{"10.0.0.5", false},       // RFC1918 — allowed (in-cluster)
		{"172.16.0.1", false},     // RFC1918 — allowed
		{"192.168.1.10", false},   // RFC1918 — allowed
	}
	for _, c := range cases {
		if got := isBlockedIP(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("isBlockedIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
	if !isBlockedIP(nil) {
		t.Error("isBlockedIP(nil) = false, want true (an unresolvable/nil IP must be blocked)")
	}
}

// TestValidateOutboundScheme pins the front-door check: http/https URLs with a
// host pass; other schemes (file/ftp/gopher), a missing scheme, and a missing
// host are rejected before a request is built.
func TestValidateOutboundScheme(t *testing.T) {
	ok := []string{
		"http://example.com/x",
		"https://harbor.svc.cluster.local/v2/",
		"https://10.0.0.5:8443/path?q=1",
	}
	for _, u := range ok {
		if err := validateOutboundScheme(u); err != nil {
			t.Errorf("validateOutboundScheme(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{
		"ftp://example.com/x",
		"file:///etc/passwd",
		"gopher://example.com",
		"http://",       // no host
		"https://",      // no host
		"example.com/x", // no scheme
	}
	for _, u := range bad {
		if err := validateOutboundScheme(u); err == nil {
			t.Errorf("validateOutboundScheme(%q) = nil, want error", u)
		}
	}
}

// TestGuardedDialContextBlocksLiteralIPs pins that the dialer refuses a literal
// blocked address before opening any connection, so a URL or redirect pointing
// straight at the metadata/loopback/link-local IP is stopped at dial time
// (covers the redirect-to-metadata path, since every hop dials through here).
func TestGuardedDialContextBlocksLiteralIPs(t *testing.T) {
	blocked := []string{
		"169.254.169.254:80", // metadata
		"127.0.0.1:8080",     // loopback
		"[::1]:80",           // loopback v6
		"169.254.1.2:443",    // link-local
	}
	for _, addr := range blocked {
		if _, err := guardedDialContext(context.Background(), "tcp", addr); err == nil {
			t.Errorf("guardedDialContext(%q) = nil error, want refusal", addr)
		}
	}
}
