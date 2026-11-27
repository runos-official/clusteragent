package agentstream

import (
	"context"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/runos-official/clusteragent/siblings"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A rolling update runs the new agent pod before it stops the old one. Both
// pods connect with the same identity, and the server keeps only the newest
// connection. The agent that lost the connection to a newer pod of its own
// Deployment steps aside: it stops reconnecting, reports not Ready, and
// waits for Kubernetes to stop it. Its running builds keep running until
// the drain on SIGTERM.
//
// The agent steps aside only for a newer pod of its own Deployment. When
// something else takes its connection (for example a copy of its
// certificate outside the cluster), it reconnects as before, and so ends
// the takeover.

const (
	// replacedMessage is the status text the server sends with ABORTED when
	// a newer connection replaced this one. Nodeward and nodehub use it.
	replacedMessage = "replaced by a newer connection"

	// handoverCheckEvery is how often a connected agent looks for a newer
	// pod that holds the connection, and how often an agent that stepped
	// aside looks again.
	handoverCheckEvery = 10 * time.Second

	// handoverResumeAfter is how many checks in a row must find no newer
	// pod holding the connection before an agent that stepped aside
	// reconnects. With handoverCheckEvery that is one minute.
	handoverResumeAfter = 6

	// handoverListTimeout bounds one read of the agent pods.
	handoverListTimeout = 10 * time.Second
)

// displaced is true while this agent has stepped aside for a newer pod.
var displaced atomic.Bool

// connectionHeld becomes true at the first answered heartbeat: the server
// then routes this agent's replies to it, so it holds the connection. It
// stays true when the stream drops later: a dropped link does not make the
// pod unready, so a server restart does not empty the agent's Service.
var connectionHeld atomic.Bool

// Ready reports whether this agent should take traffic and count as the
// running agent: it has held the control connection, and no newer pod has
// displaced it. The readiness probe reads it. A rolling update stops the
// old pod only after the new pod is Ready, so the new pod holds the
// connection before the old one stops.
func Ready() bool { return connectionHeld.Load() && !displaced.Load() }

// Displaced reports whether this agent has stepped aside for a newer pod.
func Displaced() bool { return displaced.Load() }

// isReplaced reports whether err says that the server replaced this stream
// with a newer connection.
func isReplaced(err error) bool {
	return err != nil && status.Code(err) == codes.Aborted && strings.Contains(err.Error(), replacedMessage)
}

// handover decides when this agent steps aside and when it comes back.
type handover struct {
	self        string
	list        func(context.Context) ([]siblings.Pod, error)
	sleep       func(time.Duration)
	checkEvery  time.Duration
	resumeAfter int
}

func newHandover(k8s *K8sClient) *handover {
	return &handover{
		self: siblings.SelfName(),
		list: func(ctx context.Context) ([]siblings.Pod, error) {
			return siblings.List(ctx, k8s.GetClientset())
		},
		sleep:       time.Sleep,
		checkEvery:  handoverCheckEvery,
		resumeAfter: handoverResumeAfter,
	}
}

func (h *handover) pods() ([]siblings.Pod, error) {
	ctx, cancel := context.WithTimeout(context.Background(), handoverListTimeout)
	defer cancel()
	return h.list(ctx)
}

// stepAsideAfter runs when the stream ended with err. It returns true, and
// marks the agent displaced, when a newer pod of this Deployment holds the
// connection. A replaced stream needs only a running newer pod, because
// that pod's readiness probe may not have run yet. Any other end needs a
// Ready newer pod. When the pods cannot be read, the agent reconnects.
func (h *handover) stepAsideAfter(err error) bool {
	replaced := isReplaced(err)
	pods, lerr := h.pods()
	if lerr != nil {
		log.Printf("Handover: cannot read the agent pods (%v); reconnecting", lerr)
		return false
	}
	newer, ok := siblings.NewerHolder(h.self, pods, replaced)
	if !ok {
		if replaced {
			log.Printf("Handover: the server replaced this connection, but no newer agent pod runs; reconnecting")
		}
		return false
	}
	displaced.Store(true)
	log.Printf("Handover: newer agent pod %s holds the control connection; this pod steps aside and waits to be stopped", newer.Name)
	return true
}

// waitAside blocks while this agent is aside. It returns, and clears
// displaced, when resumeAfter checks in a row find no newer pod that holds
// the connection. That covers a newer pod that went away before it took
// over (a second rollout replaced it): the cluster must not stay without an
// agent. A failed read does not count towards the resume.
func (h *handover) waitAside() {
	misses := 0
	for {
		h.sleep(h.checkEvery)
		pods, err := h.pods()
		if err != nil {
			continue
		}
		if _, ok := siblings.NewerHolder(h.self, pods, false); ok {
			misses = 0
			continue
		}
		misses++
		if misses >= h.resumeAfter {
			displaced.Store(false)
			log.Printf("Handover: no newer agent pod holds the control connection after %d checks; reconnecting", misses)
			return
		}
	}
}

// watch runs while a stream is up. It ends the stream when a newer, Ready
// pod of this Deployment exists. A newer pod is Ready only after it held
// the connection, so this agent has lost it. The reconnect loop then calls
// stepAsideAfter. Without this check the agent learns of the loss only when
// the server ends its stream or its heartbeats fail.
func (h *handover) watch(ctx context.Context, endStream func()) {
	ticker := time.NewTicker(h.checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pods, err := h.pods()
		if err != nil {
			continue
		}
		if newer, ok := siblings.NewerHolder(h.self, pods, false); ok {
			log.Printf("Handover: newer agent pod %s is Ready; ending this pod's control stream", newer.Name)
			endStream()
			return
		}
	}
}
