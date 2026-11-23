package datastore_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/runos-official/clusteragent/buildkitclient"
	"github.com/runos-official/clusteragent/datastore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktest "k8s.io/client-go/testing"
)

func TestImagePushJobStatus(t *testing.T) {
	datastore.SetupBuildTestDB(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "buildctl"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "pods", func(_ ktest.Action) (bool, runtime.Object, error) {
		return true, &corev1.Pod{Status: corev1.PodStatus{PodIP: "127.0.0.1", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}, nil
	})
	for _, tc := range []struct {
		name       string
		wantStatus string
	}{{"deploy-image", datastore.JobStatusBusy}, {"standalone", datastore.JobStatusSuccess}} {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.name
			if err := datastore.CreateBuildKitJob(id, "app", "", "", "tag"); err != nil {
				t.Fatal(err)
			}
			if err := buildkitclient.Build(context.Background(), client, buildkitclient.BuildConfig{AwaitDeploymentPatch: tc.wantStatus == datastore.JobStatusBusy, JobID: id, OSID: "app", Commit: "tag", LocalContextPath: dir, HarborURL: "registry.example.test"}); err != nil {
				t.Fatal(err)
			}
			row, err := datastore.GetBuildKitJob(id)
			if err != nil {
				t.Fatal(err)
			}
			if row.Status != tc.wantStatus || (row.CompletedAt != nil) != (row.Status == datastore.JobStatusSuccess) {
				t.Fatalf("after buildctl success: status=%s completed_at=%v", row.Status, row.CompletedAt)
			}
		})
	}
}
