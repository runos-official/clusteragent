package agentstream

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func buildkitConfigMapFixture(coreURL string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "buildkit", Namespace: Namespace},
		Data: map[string]string{
			"harbor-url":      coreURL,
			"harbor-core-url": coreURL,
			"cluster-domain":  "cluster.example.test",
		},
	}
}

func buildkitSecretFixture(osid string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "buildkit", Namespace: Namespace},
		Data: map[string][]byte{
			"harbor-username": []byte("buildkit-user"),
			"harbor-password": []byte("buildkit-pass"),
			"harbor-osid":     []byte(osid),
		},
	}
}

// TestGetBuildKitConfig_RefusesTornConfigMapSecretPairing pins review
// finding 1074: Conductor writes the buildkit ConfigMap and Secret as two
// separate, non-atomic API calls during a Harbor retarget
// (ensureBuildConfig.ts: ensureConfigMap, then ensureSecret). A read that
// observes the ConfigMap already naming the new Harbor's URL while the
// Secret still names the old Harbor's identity (or vice versa) must be
// refused as an inconsistent pairing, not silently trusted - the criterion
// 6(b) identity comparison is only as good as the single coherent
// configuration it is compared against.
func TestGetBuildKitConfig_RefusesTornConfigMapSecretPairing(t *testing.T) {
	clientset := kubefake.NewSimpleClientset(
		buildkitConfigMapFixture("harbor-old.example.test"),
		buildkitSecretFixture("harbor-old-osid"),
	)

	var cmGets int
	clientset.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		cmGets++
		if cmGets == 1 {
			return true, buildkitConfigMapFixture("harbor-old.example.test"), nil
		}
		// The second read observes a retarget that landed in the ConfigMap
		// between our two reads; the Secret fixture never changes, so this
		// models the exact torn pairing the finding describes.
		return true, buildkitConfigMapFixture("harbor-new.example.test"), nil
	})

	client := &K8sClient{clientset: clientset}
	if _, err := client.GetBuildKitConfig(context.Background()); err == nil {
		t.Fatal("expected a refusal for a configuration that changed between reads")
	}
}

// TestGetBuildKitConfig_RefusesTornSecretIdentity is the Secret-side mirror:
// the osid changes between the two reads while the ConfigMap stays put.
func TestGetBuildKitConfig_RefusesTornSecretIdentity(t *testing.T) {
	clientset := kubefake.NewSimpleClientset(
		buildkitConfigMapFixture("harbor.example.test"),
		buildkitSecretFixture("harbor-old-osid"),
	)

	var secretGets int
	clientset.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		secretGets++
		if secretGets == 1 {
			return true, buildkitSecretFixture("harbor-old-osid"), nil
		}
		return true, buildkitSecretFixture("harbor-new-osid"), nil
	})

	client := &K8sClient{clientset: clientset}
	if _, err := client.GetBuildKitConfig(context.Background()); err == nil {
		t.Fatal("expected a refusal for an identity that changed between reads")
	}
}

// TestGetBuildKitConfig_ConsistentReadSucceeds proves the common,
// legitimate case - no retarget in flight, both reads agree - still works.
func TestGetBuildKitConfig_ConsistentReadSucceeds(t *testing.T) {
	clientset := kubefake.NewSimpleClientset(
		buildkitConfigMapFixture("harbor.example.test"),
		buildkitSecretFixture("harbor-osid-1"),
	)
	client := &K8sClient{clientset: clientset}

	cfg, err := client.GetBuildKitConfig(context.Background())
	if err != nil {
		t.Fatalf("a stable configuration must succeed: %v", err)
	}
	if cfg.HarborOsid != "harbor-osid-1" || cfg.HarborCoreURL != "harbor.example.test" {
		t.Fatalf("config = %+v, want the stable fixture values", cfg)
	}
}
