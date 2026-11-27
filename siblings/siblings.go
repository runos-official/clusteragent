// Package siblings reads the other pods of the cluster agent's own
// Deployment. A rolling update runs a new agent pod before it stops the old
// one, so for a short time two agent processes run. The agent uses this
// package to decide which of the two holds the control connection, and to
// keep work that must run once (the startup build cleanup) away from builds
// that the other pod still runs.
package siblings

import (
	"context"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// Namespace is where the agent's Deployment runs.
	Namespace = "runos"
	// Selector picks the pods of the agent's Deployment.
	Selector = "app=runos-cluster-agent"
	// PodNameEnv names this pod. The Deployment sets it from the downward
	// API. Without it the agent uses the host name, which Kubernetes sets
	// to the pod name.
	PodNameEnv = "POD_NAME"
)

// Pod is what the agent needs to know about one agent pod.
type Pod struct {
	Name        string
	Created     time.Time
	Running     bool // phase Running
	Ready       bool // condition Ready is True
	Terminating bool // has a deletion timestamp
	Finished    bool // phase Succeeded or Failed: its process has ended
}

// SelfName returns the name of this agent's pod.
func SelfName() string {
	if v := os.Getenv(PodNameEnv); v != "" {
		return v
	}
	h, _ := os.Hostname()
	return h
}

// List reads the agent pods.
func List(ctx context.Context, client kubernetes.Interface) ([]Pod, error) {
	list, err := client.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{LabelSelector: Selector})
	if err != nil {
		return nil, err
	}
	return FromPods(list.Items), nil
}

// FromPods converts Kubernetes pods to Pods.
func FromPods(items []corev1.Pod) []Pod {
	out := make([]Pod, 0, len(items))
	for _, item := range items {
		p := Pod{
			Name:        item.Name,
			Created:     item.CreationTimestamp.Time,
			Running:     item.Status.Phase == corev1.PodRunning,
			Terminating: item.DeletionTimestamp != nil,
			Finished:    item.Status.Phase == corev1.PodSucceeded || item.Status.Phase == corev1.PodFailed,
		}
		for _, c := range item.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				p.Ready = true
			}
		}
		out = append(out, p)
	}
	return out
}

// newerThan reports whether p was created after other. The name breaks a
// tie, so two pods never both count as the newer one.
func (p Pod) newerThan(other Pod) bool {
	if !p.Created.Equal(other.Created) {
		return p.Created.After(other.Created)
	}
	return p.Name > other.Name
}

// NewerHolder returns a pod that is newer than this agent's pod and holds,
// or is about to hold, the control connection. The pod must run, must not
// be terminating, and must be Ready. When replaced is true, the server has
// told this agent that a newer connection took its place; then a running
// newer pod is enough, because its readiness probe may not have run yet.
//
// It returns false when self is not in pods. The agent then cannot tell
// which pod is newer, and it keeps its connection.
func NewerHolder(self string, pods []Pod, replaced bool) (Pod, bool) {
	var me *Pod
	for i := range pods {
		if pods[i].Name == self {
			me = &pods[i]
		}
	}
	if me == nil {
		return Pod{}, false
	}
	var best Pod
	found := false
	for _, p := range pods {
		if p.Name == self || !p.Running || p.Terminating || p.Finished || !p.newerThan(*me) {
			continue
		}
		if !p.Ready && !replaced {
			continue
		}
		if !found || p.newerThan(best) {
			best, found = p, true
		}
	}
	return best, found
}

// alone reports whether no other agent pod is left whose process may run.
func alone(self string, pods []Pod) bool {
	for _, p := range pods {
		if p.Name != self && !p.Finished {
			return false
		}
	}
	return true
}

// WaitUntilAlone lists the agent pods every interval until no other agent
// pod is left, or until maxWait has passed. It returns true when this pod
// is alone. A failed list counts as "not alone".
func WaitUntilAlone(list func() ([]Pod, error), self string, every, maxWait time.Duration, sleep func(time.Duration), now func() time.Time) bool {
	start := now()
	for {
		pods, err := list()
		if err == nil && alone(self, pods) {
			return true
		}
		if now().Sub(start) >= maxWait {
			return false
		}
		sleep(every)
	}
}
