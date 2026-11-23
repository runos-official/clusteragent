package webhook

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/datastore"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktest "k8s.io/client-go/testing"
)

func TestScopedPatchTransitions(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		scoped, buildOnly, conflict bool
		denyAt                      int
		want                        []string
	}{
		{"permit", true, false, false, 0, []string{"permit", "update", "success"}},
		{"deny", true, false, false, 1, []string{"deny", "failed"}},
		{"retry denied", true, false, true, 2, []string{"permit", "update", "deny", "failed"}},
		{"retry permitted", true, false, true, 0, []string{"permit", "update", "permit", "update", "success"}},
		{"legacy", false, false, false, 0, []string{"success", "update"}},
		{"scoped build only", true, true, false, 0, []string{"success"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const osid = "app"
				deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: osid, Namespace: osid}}
				deployment.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app", Image: "old"}}
				client := fake.NewSimpleClientset(deployment)
				var events []string
				requests, updates := 0, 0
				retries := map[string]bool{}
				original := requestStageRecheck
				defer func() { requestStageRecheck = original }()
				requestStageRecheck = func(_ context.Context, auth agentstream.RecheckAuthority, stage agentstream.RecheckStage, retry string) (*agentstream.StageResponse, error) {
					if auth != agentstream.JobAuthority(testAuthorityJobID) || stage != agentstream.StageDeploymentPatch || retry == "" || retries[retry] {
						t.Fatalf("invalid permit correlation")
					}
					retries[retry] = true
					requests++
					if requests == tc.denyAt {
						events = append(events, "deny")
						return &agentstream.StageResponse{Outcome: agentstream.OutcomeDeny}, nil
					}
					events = append(events, "permit")
					return &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, ExpiresAt: time.Now().Add(5 * time.Second), Lifetime: agentstream.NewTestLifetime(true)}, nil
				}
				client.PrependReactor("update", "deployments", func(action ktest.Action) (bool, runtime.Object, error) {
					updates++
					events = append(events, "update")
					if tc.conflict && updates == 1 {
						return true, nil, errors.New("conflict")
					}
					return false, nil, nil
				})
				input := BuildAndDeployInput{JobID: "attempt", OSID: osid, BuildOnly: tc.buildOnly}
				if tc.scoped {
					input.AuthorizationClassification = datastore.AuthorizationScoped
					input.AuthorityJobID = testAuthorityJobID
				}
				// Build publishes at push for ordinary jobs; TestImagePushJobStatus
				// exercises the real Build implementation against disposable storage.
				if !input.awaitDeploymentPatch() {
					events = append(events, datastore.JobStatusSuccess)
				}
				finishPushedImage(context.Background(), client, input, "new", func(job, status string) error {
					if job != input.JobID {
						t.Fatal("wrong job")
					}
					events = append(events, status)
					return nil
				})
				got, err := client.AppsV1().Deployments(osid).Get(context.Background(), osid, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				wantImage := "old"
				if !tc.buildOnly && tc.denyAt == 0 {
					wantImage = "new"
				}
				if got.Spec.Template.Spec.Containers[0].Image != wantImage {
					t.Fatalf("stored image = %s, want %s", got.Spec.Template.Spec.Containers[0].Image, wantImage)
				}
				if !slices.Equal(events, tc.want) {
					t.Fatalf("transitions = %v, want %v", events, tc.want)
				}
			})
		})
	}
}

func TestScopedPatchRejectsUnusablePermit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response *agentstream.StageResponse
		err      error
	}{
		{"expired", &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, ExpiresAt: time.Now().Add(-time.Second), Lifetime: agentstream.NewTestLifetime(true)}, nil},
		{"ended_stream", &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, ExpiresAt: time.Now().Add(time.Minute), Lifetime: agentstream.NewTestLifetime(false)}, nil},
		{"needs", &agentstream.StageResponse{Outcome: agentstream.OutcomeNeeds}, nil},
		{"unavailable", nil, errors.New("transport unavailable")},
		{"empty", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "app"}}
			d.Spec.Template.Spec.Containers = []corev1.Container{{Image: "old"}}
			client := fake.NewSimpleClientset(d)
			previous := requestStageRecheck
			t.Cleanup(func() { requestStageRecheck = previous })
			requestStageRecheck = func(context.Context, agentstream.RecheckAuthority, agentstream.RecheckStage, string) (*agentstream.StageResponse, error) {
				return tc.response, tc.err
			}
			var statuses []string
			finishPushedImage(context.Background(), client, BuildAndDeployInput{JobID: "build", OSID: "app", AuthorityJobID: testAuthorityJobID}, "new", func(_, status string) error { statuses = append(statuses, status); return nil })
			if !slices.Equal(statuses, []string{datastore.JobStatusFailed}) {
				t.Fatalf("statuses=%v", statuses)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() == "update" {
					t.Fatal("refused permit reached Kubernetes Update")
				}
			}
		})
	}
}

func TestScopedPatchRejectedByKubernetesNeverSucceeds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "app"}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Image: "old"}}
	client := fake.NewSimpleClientset(d)
	previous := requestStageRecheck
	t.Cleanup(func() { requestStageRecheck = previous })
	requestStageRecheck = func(context.Context, agentstream.RecheckAuthority, agentstream.RecheckStage, string) (*agentstream.StageResponse, error) {
		return &agentstream.StageResponse{Outcome: agentstream.OutcomePermit, ExpiresAt: time.Now().Add(time.Second), Lifetime: agentstream.NewTestLifetime(true)}, nil
	}
	client.PrependReactor("update", "deployments", func(ktest.Action) (bool, runtime.Object, error) {
		cancel()
		return true, nil, errors.New("update failed")
	})
	var statuses []string
	finishPushedImage(ctx, client, BuildAndDeployInput{JobID: "build", OSID: "app", AuthorityJobID: testAuthorityJobID}, "new", func(_, status string) error { statuses = append(statuses, status); return nil })
	if !slices.Equal(statuses, []string{datastore.JobStatusFailed}) {
		t.Fatalf("statuses=%v", statuses)
	}
}
