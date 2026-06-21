package agentstream

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
)

// Bootstrap retry tuning. During cluster creation the dependencies the agent
// needs (the runos-config ConfigMap, the node-agent-tls Secret, a reachable
// Nodeward, working in-cluster DNS) come up asynchronously and out of order, so
// the bootstrap steps below routinely fail for the first few seconds to minutes.
// We retry those transient failures forever-bounded rather than crash-looping
// the pod with a raw Go fatal.
const (
	// bootstrapBaseDelay is the first retry delay for a bootstrap step.
	bootstrapBaseDelay = 1 * time.Second
	// bootstrapMaxDelay caps the bootstrap retry backoff.
	bootstrapMaxDelay = 30 * time.Second
	// bootstrapStepTimeout bounds a single attempt of a bootstrap step (one
	// ConfigMap Get, one Secret Get, one Nodeward dial). Each step gets its own
	// fresh timeout rather than sharing a single process-wide budget, so a slow
	// API server early on can't exhaust the budget for a later step.
	bootstrapStepTimeout = 30 * time.Second
	// bootstrapConnectTimeout bounds a single initial gRPC connect attempt.
	// Connect blocks until the transport is ready, so it gets a longer per-step
	// timeout than a plain API read.
	bootstrapConnectTimeout = 45 * time.Second
)

// errMalformedCert is the sentinel for the one bootstrap failure that is NOT
// retryable: a cluster-agent-tls Secret that exists but holds an unparseable
// cert/key. Retrying can't fix bad bytes already at rest; an operator has to
// delete the secret so it is regenerated. isRetryable classifies this as fatal.
var errMalformedCert = errors.New("cluster-agent-tls secret holds a malformed certificate or key")

// isRetryable classifies a bootstrap/reconnect error as worth retrying.
//
// Retryable (the dependency is not ready yet, or the network is briefly down):
//   - k8s NotFound (the ConfigMap/Secret the installer writes hasn't landed yet)
//   - gRPC Unavailable / DeadlineExceeded (Nodeward warming up or unreachable)
//   - context.DeadlineExceeded (our own per-step timeout fired)
//   - net errors: timeouts, DNS-not-resolvable, connection refused
//
// Fatal (operator-actionable, retrying won't help):
//   - errMalformedCert: bad cert bytes already at rest in the secret
//
// Anything we don't recognise is treated as retryable by default: at bootstrap
// the cost of an extra retry loop is trivial next to crash-looping the pod on a
// transient we failed to enumerate. Truly permanent conditions are caught at the
// call site (e.g. the malformed-cert sentinel) before they reach here.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Fatal sentinel: a malformed cert already at rest. Check first so it wins
	// even if it were ever wrapped alongside a retryable-looking cause.
	if errors.Is(err, errMalformedCert) {
		return false
	}

	// k8s typed errors: a not-yet-created ConfigMap/Secret is retryable; the
	// API server being briefly unavailable / timing out is retryable.
	if k8serrors.IsNotFound(err) ||
		k8serrors.IsServiceUnavailable(err) ||
		k8serrors.IsServerTimeout(err) ||
		k8serrors.IsTimeout(err) ||
		k8serrors.IsTooManyRequests(err) {
		return true
	}

	// gRPC status codes: Unavailable (server warming up / unreachable) and
	// DeadlineExceeded (slow to respond) are the transient connect failures.
	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.Unavailable, codes.DeadlineExceeded:
			return true
		}
	}

	// Our own per-step timeout firing.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// Wrapped net errors: i/o timeouts, DNS-not-resolvable, connection refused.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	// String fallback for transients that arrive as opaque strings (some gRPC
	// dial failures, the grpc.WithBlock connect path) and don't unwrap to a
	// typed net/grpc error. Kept narrow and last.
	msg := strings.ToLower(err.Error())
	for _, frag := range []string{
		"connection refused",
		"connection reset",
		"no such host",
		"i/o timeout",
		"context deadline exceeded",
		"transport: error while dialing",
		"server closed the stream",
		"stream was canceled",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}

	// Default: retry. Bootstrap should not crash-loop on an unclassified error.
	return true
}

// nextBackoff returns the delay before retry attempt n (1-based) using capped
// exponential backoff: base * 2^(n-1), clamped to bootstrapMaxDelay. The
// schedule is monotonic non-decreasing and never exceeds the cap. attempt <= 0
// is treated as the first attempt (returns base). No jitter: the schedule must
// be deterministic so the regression test can pin it exactly; jitter belongs in
// the live gRPC dialer (ConnectToServer), not here.
func nextBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Cap the shift so 1<<shift can't overflow on a long-running reconnect loop.
	// At maxShift the value is already far past bootstrapMaxDelay and clamped.
	const maxShift = 62
	shift := attempt - 1
	if shift > maxShift {
		shift = maxShift
	}
	delay := bootstrapBaseDelay * time.Duration(int64(1)<<uint(shift))
	// The shift can overflow time.Duration into negative at extreme attempts;
	// treat any non-positive (overflowed) result as "use the cap".
	if delay <= 0 || delay > bootstrapMaxDelay {
		delay = bootstrapMaxDelay
	}
	return delay
}
