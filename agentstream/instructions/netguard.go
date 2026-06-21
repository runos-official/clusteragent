package instructions

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// netguard provides conservative SSRF protection for the handlers that fetch
// attacker-influenceable URLs (WEB_REQUEST, WEB_REQUEST_FOLLOW). An inbound
// instruction chooses the URL, so without this guard a single control-plane
// message could make the agent read cloud instance-metadata
// (169.254.169.254 -> IAM credentials) or other host-local services and return
// the body to the caller.
//
// Policy (deliberately narrow so legitimate in-cluster traffic keeps working):
//   - the scheme must be http or https;
//   - the resolved IP actually dialed must not be loopback, link-local, or the
//     cloud metadata address.
//
// We do NOT block general RFC1918 / private ranges: calls to in-cluster pod,
// service and node IPs are routine and legitimate for this agent. The IP check
// lives in the dialer, so it runs for the initial request AND every redirect
// hop, and it dials the exact IP it validated (pinning) so DNS cannot rebind to
// a blocked address between the check and the connection. Mirrors the node
// agent's agentstream/netguard.go.

// metadataIP is the well-known cloud instance-metadata address. It is already
// covered by the link-local /16 block below, but is called out for clarity.
var metadataIP = net.ParseIP("169.254.169.254")

// isBlockedIP reports whether ip must never be dialed: loopback (127.0.0.0/8,
// ::1), link-local IPv4 (169.254.0.0/16, which includes the metadata IP) or IPv6
// (fe80::/10), or the cloud metadata IP. nil is treated as blocked.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	return ip.Equal(metadataIP)
}

// resolveHostIPs returns the IPs a host resolves to. A literal IP resolves to
// itself.
func resolveHostIPs(host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("host %q resolved to no addresses", host)
	}
	return ips, nil
}

// validateOutboundScheme rejects a URL whose scheme is not http/https (or that
// has no host) before a request is ever built, for a clear early error. The
// dialer is the authoritative IP gate; this is the cheap front-door check.
func validateOutboundScheme(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Hostname() == "" {
			return fmt.Errorf("URL has no host")
		}
		return nil
	default:
		return fmt.Errorf("URL scheme must be http or https, got %q", u.Scheme)
	}
}

// guardedDialContext resolves the host being dialed, refuses the connection if
// ANY resolved address is loopback/link-local/metadata, and dials the validated
// IP directly (pinning, to defeat DNS rebinding). Installed on the Transport so
// it runs for every hop, including redirects to a fresh host.
func guardedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := resolveHostIPs(host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return nil, fmt.Errorf("refusing connection to internal/metadata address %s (host %q)", ip, host)
		}
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

// newGuardedTransport returns an http.Transport that enforces the SSRF guard on
// every connection it opens. allowInsecure controls TLS certificate
// verification only (a legitimate opt-in for self-signed in-cluster endpoints);
// it does NOT relax the IP guard.
func newGuardedTransport(allowInsecure bool) *http.Transport {
	return &http.Transport{
		DialContext: guardedDialContext,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: allowInsecure, //nolint:gosec // explicit opt-in for self-signed in-cluster endpoints
		},
	}
}
