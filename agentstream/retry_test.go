package agentstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestIsRetryable pins the bootstrap/reconnect error classification: the
// transient conditions that hit during cluster creation (Nodeward warming up,
// the installer-written ConfigMap/Secret not yet present, DNS/network blips)
// must be RETRYABLE so the agent backs off instead of crash-looping, while the
// one operator-actionable condition (a malformed cert already at rest) must be
// FATAL so we surface a remediation hint instead of looping forever.
//
// Regression guard: a previous version had this whole chain as log.Fatalf,
// turning normal startup races into CrashLoopBackOff. If someone reclassifies
// gRPC Unavailable / k8s NotFound as fatal, or makes the malformed-cert
// sentinel retryable, this test fails.
func TestIsRetryable(t *testing.T) {
	secretsGR := schema.GroupResource{Group: "", Resource: "secrets"}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		// --- Retryable: dependency not ready / network transient ---
		{
			name: "gRPC Unavailable (Nodeward warming up / unreachable)",
			err:  status.Error(codes.Unavailable, "connection refused"),
			want: true,
		},
		{
			name: "gRPC DeadlineExceeded (server slow to respond)",
			err:  status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
			want: true,
		},
		{
			name: "k8s NotFound (ConfigMap/Secret not written yet)",
			err:  k8serrors.NewNotFound(secretsGR, "cluster-agent-tls"),
			want: true,
		},
		{
			name: "k8s ServiceUnavailable (API server warming up)",
			err:  k8serrors.NewServiceUnavailable("apiserver not ready"),
			want: true,
		},
		{
			name: "context deadline exceeded (our per-step timeout fired)",
			err:  context.DeadlineExceeded,
			want: true,
		},
		{
			name: "wrapped net timeout",
			err:  fmt.Errorf("dial: %w", timeoutNetErr{}),
			want: true,
		},
		{
			name: "DNS not-resolvable (in-cluster DNS not ready)",
			err:  &net.DNSError{Err: "no such host", Name: "nodeward.runos.svc", IsNotFound: true},
			want: true,
		},
		{
			name: "connection refused as opaque string (grpc dial path)",
			err:  errors.New("transport: error while dialing: dial tcp 10.0.0.1:9192: connect: connection refused"),
			want: true,
		},
		{
			name: "server closed the stream (reconnect path)",
			err:  errors.New("server closed the stream"),
			want: true,
		},
		{
			name: "unrecognised error defaults to retryable (don't crash-loop)",
			err:  errors.New("some brand new transient we never enumerated"),
			want: true,
		},
		{
			name: "errNotReadyYet (ConfigMap present but unpopulated)",
			err:  fmt.Errorf("runos-config present but 'server' not yet set: %w", errNotReadyYet),
			want: true,
		},

		// --- Fatal: operator-actionable, retrying can't help ---
		{
			name: "malformed-cert sentinel is FATAL",
			err:  errMalformedCert,
			want: false,
		},
		{
			name: "wrapped malformed-cert sentinel is FATAL",
			err:  fmt.Errorf("%w: x509: malformed certificate", errMalformedCert),
			want: false,
		},

		// --- nil ---
		{
			name: "nil is not retryable",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.err); got != tc.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// timeoutNetErr is a minimal net.Error whose Timeout() reports true, used to
// pin that a wrapped network timeout classifies as retryable.
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return true }

// TestNextBackoff pins the capped-exponential reconnect schedule:
//   - it is monotonic non-decreasing as the attempt count rises,
//   - it never exceeds bootstrapMaxDelay,
//   - it starts at bootstrapBaseDelay and doubles until clamped,
//   - it stays clamped (and never goes negative) at extreme attempt counts.
//
// Regression guard: replaces the old runWithReconnect that gave up after a
// fixed attempt count and called log.Fatalf. The agent now reconnects forever,
// so the backoff must stay bounded by the cap no matter how high the attempt
// number climbs (a naive `base << attempt` overflows time.Duration to a
// negative or tiny value, which would busy-loop reconnects).
func TestNextBackoff(t *testing.T) {
	t.Run("exact early schedule doubles from base", func(t *testing.T) {
		want := []time.Duration{
			bootstrapBaseDelay,     // attempt 1: base * 2^0
			bootstrapBaseDelay * 2, // attempt 2
			bootstrapBaseDelay * 4, // attempt 3
			bootstrapBaseDelay * 8, // attempt 4
		}
		for i, w := range want {
			attempt := i + 1
			if got := nextBackoff(attempt); got != w {
				t.Errorf("nextBackoff(%d) = %s, want %s", attempt, got, w)
			}
		}
	})

	t.Run("attempt <= 0 is treated as first attempt", func(t *testing.T) {
		for _, a := range []int{0, -1, -100} {
			if got := nextBackoff(a); got != bootstrapBaseDelay {
				t.Errorf("nextBackoff(%d) = %s, want base %s", a, got, bootstrapBaseDelay)
			}
		}
	})

	t.Run("monotonic non-decreasing and never exceeds the cap", func(t *testing.T) {
		var prev time.Duration
		for attempt := 1; attempt <= 100; attempt++ {
			got := nextBackoff(attempt)
			if got > bootstrapMaxDelay {
				t.Fatalf("nextBackoff(%d) = %s exceeds cap %s", attempt, got, bootstrapMaxDelay)
			}
			if got <= 0 {
				t.Fatalf("nextBackoff(%d) = %s is non-positive (overflow?)", attempt, got)
			}
			if got < prev {
				t.Fatalf("nextBackoff(%d) = %s decreased from previous %s", attempt, got, prev)
			}
			prev = got
		}
	})

	t.Run("clamps to exactly the cap once reached and stays there", func(t *testing.T) {
		// At extreme attempt counts the raw exponential overflows; the function
		// must clamp to the cap, never wrap to a negative/tiny value.
		for _, attempt := range []int{40, 62, 63, 64, 1000, 1 << 20} {
			got := nextBackoff(attempt)
			if got != bootstrapMaxDelay {
				t.Errorf("nextBackoff(%d) = %s, want cap %s", attempt, got, bootstrapMaxDelay)
			}
		}
	})
}
