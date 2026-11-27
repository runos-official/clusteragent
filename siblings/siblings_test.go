package siblings

import (
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func pod(name string, created time.Time, mods ...func(*Pod)) Pod {
	p := Pod{Name: name, Created: created, Running: true, Ready: true}
	for _, m := range mods {
		m(&p)
	}
	return p
}

func notReady(p *Pod)    { p.Ready = false }
func pending(p *Pod)     { p.Running, p.Ready = false, false }
func terminating(p *Pod) { p.Terminating = true }
func finished(p *Pod)    { p.Finished, p.Running, p.Ready = true, false, false }

// An agent steps aside only for a NEWER pod of its own Deployment that holds
// the connection. It never yields to an older pod, to a pod on its way out,
// or when it cannot find itself in the list.
func TestNewerHolder(t *testing.T) {
	self := pod("agent-a", t0)
	cases := []struct {
		name     string
		pods     []Pod
		replaced bool
		want     string
	}{
		{"alone", []Pod{self}, false, ""},
		{"alone and replaced", []Pod{self}, true, ""},
		{"newer pod is ready", []Pod{self, pod("agent-b", t0.Add(time.Minute))}, false, "agent-b"},
		{"older pod is ready", []Pod{self, pod("agent-0", t0.Add(-time.Minute))}, false, ""},
		{"older pod is ready and we were replaced", []Pod{self, pod("agent-0", t0.Add(-time.Minute))}, true, ""},
		{"newer pod runs but is not ready", []Pod{self, pod("agent-b", t0.Add(time.Minute), notReady)}, false, ""},
		{"newer pod runs, not ready, we were replaced", []Pod{self, pod("agent-b", t0.Add(time.Minute), notReady)}, true, "agent-b"},
		{"newer pod is pending and we were replaced", []Pod{self, pod("agent-b", t0.Add(time.Minute), pending)}, true, ""},
		{"newer pod is terminating", []Pod{self, pod("agent-b", t0.Add(time.Minute), terminating)}, false, ""},
		{"newer pod has finished", []Pod{self, pod("agent-b", t0.Add(time.Minute), finished)}, true, ""},
		{"self not in the list", []Pod{pod("agent-b", t0.Add(time.Minute))}, true, ""},
		{"same creation time, later name wins", []Pod{self, pod("agent-b", t0)}, false, "agent-b"},
		{"same creation time, earlier name loses", []Pod{pod("agent-b", t0), pod("agent-a", t0)}, false, "agent-b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NewerHolder("agent-a", tc.pods, tc.replaced)
			if tc.want == "" {
				if ok {
					t.Fatalf("NewerHolder = %s, want none", got.Name)
				}
				return
			}
			if !ok || got.Name != tc.want {
				t.Fatalf("NewerHolder = %q %v, want %q", got.Name, ok, tc.want)
			}
		})
	}
}

func TestFromPods(t *testing.T) {
	now := metav1.NewTime(t0)
	items := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ready", CreationTimestamp: now},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "leaving", CreationTimestamp: now, DeletionTimestamp: &now},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pending", CreationTimestamp: now},
			Status:     corev1.PodStatus{Phase: corev1.PodPending},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "done", CreationTimestamp: now},
			Status:     corev1.PodStatus{Phase: corev1.PodFailed},
		},
	}
	got := FromPods(items)
	want := []Pod{
		{Name: "ready", Created: t0, Running: true, Ready: true},
		{Name: "leaving", Created: t0, Running: true, Terminating: true},
		{Name: "pending", Created: t0},
		{Name: "done", Created: t0, Finished: true},
	}
	if len(got) != len(want) {
		t.Fatalf("FromPods = %+v, want %+v", got, want)
	}
	for i := range want {
		if !got[i].Created.Equal(want[i].Created) {
			t.Errorf("pod %d created %s, want %s", i, got[i].Created, want[i].Created)
		}
		got[i].Created = want[i].Created
		if got[i] != want[i] {
			t.Errorf("pod %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSelfNamePrefersPodName(t *testing.T) {
	t.Setenv(PodNameEnv, "agent-from-env")
	if got := SelfName(); got != "agent-from-env" {
		t.Fatalf("SelfName = %q, want the %s value", got, PodNameEnv)
	}
}

// The new pod must not sweep build pods or close build rows while an older
// agent pod still runs: those builds belong to the older pod.
func TestWaitUntilAlone(t *testing.T) {
	self := pod("agent-b", t0)
	old := pod("agent-a", t0.Add(-time.Minute), terminating)
	errList := errors.New("api down")

	type answer struct {
		pods []Pod
		err  error
	}
	cases := []struct {
		name      string
		answers   []answer
		wantAlone bool
		wantCalls int
	}{
		{"alone at once", []answer{{pods: []Pod{self}}}, true, 1},
		{"finished pods do not count", []answer{{pods: []Pod{self, pod("agent-x", t0, finished)}}}, true, 1},
		{"waits for the old pod to go", []answer{
			{pods: []Pod{self, old}}, {err: errList}, {pods: []Pod{self, old}}, {pods: []Pod{self}},
		}, true, 4},
		{"gives up at the bound", []answer{
			{pods: []Pod{self, old}}, {pods: []Pod{self, old}}, {pods: []Pod{self, old}}, {pods: []Pod{self, old}},
			{pods: []Pod{self, old}}, {pods: []Pod{self, old}},
		}, false, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := t0
			calls := 0
			list := func() ([]Pod, error) {
				a := tc.answers[calls]
				calls++
				return a.pods, a.err
			}
			alone := WaitUntilAlone(list, "agent-b", 10*time.Second, 30*time.Second,
				func(d time.Duration) { clock = clock.Add(d) }, func() time.Time { return clock })
			if alone != tc.wantAlone || calls != tc.wantCalls {
				t.Fatalf("WaitUntilAlone = %v after %d lists, want %v after %d", alone, calls, tc.wantAlone, tc.wantCalls)
			}
		})
	}
}
