package agentstream

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"github.com/runos-official/clusteragent/siblings"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A rolling update runs the new agent pod before it stops the old one. Both
// pods connect with the same identity, and the server keeps only the newest
// connection. Before this change the old pod reconnected after every drop,
// so the two pods took the connection from each other until the old pod
// exited. These tests pin that the agent that lost the connection to a
// newer pod of its own Deployment steps aside instead.

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func resetHandoverState(t *testing.T) {
	t.Helper()
	displaced.Store(false)
	connectionHeld.Store(false)
	t.Cleanup(func() {
		displaced.Store(false)
		connectionHeld.Store(false)
	})
}

func replacedErr() error {
	return fmt.Errorf("error receiving instruction: %w", status.Error(codes.Aborted, "replaced by a newer connection"))
}

func TestIsReplaced(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{replacedErr(), true},
		{status.Error(codes.Aborted, "replaced by a newer connection"), true},
		{status.Error(codes.Aborted, "something else"), false},
		{status.Error(codes.Unavailable, "replaced by a newer connection"), false},
		{errors.New("heartbeat failed 3 times consecutively"), false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := isReplaced(tc.err); got != tc.want {
			t.Errorf("isReplaced(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func listOf(pods ...siblings.Pod) func(context.Context) ([]siblings.Pod, error) {
	return func(context.Context) ([]siblings.Pod, error) { return pods, nil }
}

func TestStepAsideAfterStreamEnd(t *testing.T) {
	self := siblings.Pod{Name: "agent-a", Created: t0, Running: true, Ready: true}
	newerReady := siblings.Pod{Name: "agent-b", Created: t0.Add(time.Minute), Running: true, Ready: true}
	newerStarting := siblings.Pod{Name: "agent-b", Created: t0.Add(time.Minute), Running: true}
	older := siblings.Pod{Name: "agent-0", Created: t0.Add(-time.Minute), Running: true, Ready: true}
	cases := []struct {
		name string
		err  error
		list func(context.Context) ([]siblings.Pod, error)
		want bool
	}{
		{"replaced, newer pod starting", replacedErr(), listOf(self, newerStarting), true},
		{"replaced, alone: someone else holds our identity", replacedErr(), listOf(self), false},
		{"replaced by an older pod", replacedErr(), listOf(self, older), false},
		{"heartbeats failed, newer pod ready", errors.New("heartbeat failed 3 times consecutively"), listOf(self, newerReady), true},
		{"heartbeats failed, newer pod not ready yet", errors.New("heartbeat failed 3 times consecutively"), listOf(self, newerStarting), false},
		{"pod list fails", replacedErr(), func(context.Context) ([]siblings.Pod, error) { return nil, errors.New("api down") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetHandoverState(t)
			h := &handover{self: "agent-a", list: tc.list}
			if got := h.stepAsideAfter(tc.err); got != tc.want {
				t.Fatalf("stepAsideAfter = %v, want %v", got, tc.want)
			}
			if Displaced() != tc.want {
				t.Fatalf("Displaced = %v, want %v", Displaced(), tc.want)
			}
		})
	}
}

// While aside, the agent comes back only after no newer pod has held the
// connection for resumeAfter checks in a row. A newer pod that went away (a
// second rollout replaced it) must not leave the cluster without an agent.
func TestWaitAsideResumesWhenNoNewerPodHolds(t *testing.T) {
	resetHandoverState(t)
	displaced.Store(true)
	self := siblings.Pod{Name: "agent-a", Created: t0, Running: true, Ready: true}
	newer := siblings.Pod{Name: "agent-b", Created: t0.Add(time.Minute), Running: true, Ready: true}
	answers := [][]siblings.Pod{
		{self, newer}, {self}, {self, newer}, {self}, {self}, {self},
	}
	calls := 0
	h := &handover{
		self: "agent-a",
		list: func(context.Context) ([]siblings.Pod, error) {
			a := answers[calls]
			calls++
			return a, nil
		},
		sleep:       func(time.Duration) {},
		resumeAfter: 3,
	}
	h.waitAside()
	if calls != len(answers) {
		t.Fatalf("waitAside returned after %d checks, want %d", calls, len(answers))
	}
	if Displaced() {
		t.Fatal("still displaced after resuming")
	}
}

// The reconnect loop does not reconnect after it stepped aside; it waits
// aside first, and then connects without a backoff wait.
func TestReconnectLoopStepsAside(t *testing.T) {
	resetHandoverState(t)
	var events []string
	establishCalls := 0
	sm := &streamManager{}
	sm.reconnectLoop(reconnectDeps{
		establish: func() error {
			establishCalls++
			events = append(events, "establish")
			if establishCalls == 1 {
				return replacedErr()
			}
			return nil
		},
		connect: func(context.Context) (l2sec.NodewardClient, *grpc.ClientConn, error) {
			events = append(events, "connect")
			return nil, nil, nil
		},
		sleep:     func(time.Duration) { events = append(events, "backoff") },
		now:       time.Now,
		backoff:   newReconnectBackoff(nil),
		stepAside: func(error) bool { events = append(events, "step aside"); return true },
		waitAside: func() { events = append(events, "wait aside") },
	})
	want := []string{"establish", "step aside", "wait aside", "connect", "establish"}
	if fmt.Sprint(events) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// A connected agent checks for a newer pod that holds the connection and
// ends its own stream when it finds one. This covers the path where the
// server only stops answering the replaced stream.
func TestWatchEndsStreamWhenNewerPodHolds(t *testing.T) {
	resetHandoverState(t)
	self := siblings.Pod{Name: "agent-a", Created: t0, Running: true, Ready: true}
	newer := siblings.Pod{Name: "agent-b", Created: t0.Add(time.Minute), Running: true, Ready: true}
	var checks atomic.Int32
	h := &handover{
		self: "agent-a",
		list: func(context.Context) ([]siblings.Pod, error) {
			if checks.Add(1) < 3 {
				return []siblings.Pod{self}, nil
			}
			return []siblings.Pod{self, newer}, nil
		},
		checkEvery: time.Millisecond,
	}
	ended := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.watch(ctx, func() { close(ended) })
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream stayed up while a newer pod held the connection")
	}
}

// Ready means: this agent has held the control connection (one heartbeat was
// answered) and no newer pod displaced it. The Deployment stops the old pod
// only when the new one is Ready, so the new one must hold the connection
// first.
func TestReadyAfterFirstAnsweredHeartbeat(t *testing.T) {
	resetHandoverState(t)
	if Ready() {
		t.Fatal("Ready before any connection")
	}
	s := newLifecycleStream()
	s.ctx = context.Background()
	done := make(chan error, 1)
	go func() { done <- DriveTestStream(s, 5*time.Millisecond, nil) }()
	eventually(t, "Ready after a heartbeat answer", Ready)
	s.fail <- status.Error(codes.Unavailable, "server restarting")
	<-done
	if !Ready() {
		t.Fatal("a dropped stream made the agent not Ready; only displacement may")
	}
	displaced.Store(true)
	if Ready() {
		t.Fatal("Ready while displaced")
	}
}

// A displaced agent does not renew the shared certificate: the pod that
// holds the connection owns the Secret.
func TestDisplacedAgentDoesNotRenew(t *testing.T) {
	resetHandoverState(t)
	renewals := 0
	sm := &streamManager{clusterAgentTLS: expiringTLS(t), renew: func() error { renewals++; return nil }}
	sm.checkAndRenew()
	if renewals != 1 {
		t.Fatalf("renewals = %d for an expiring certificate, want 1", renewals)
	}
	displaced.Store(true)
	sm.checkAndRenew()
	if renewals != 1 {
		t.Fatalf("a displaced agent renewed: renewals = %d, want 1", renewals)
	}
}

func expiringTLS(t *testing.T) *TLSData {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &TLSData{TLSCert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}
