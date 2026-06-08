package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultStorageClassUsesPerPVCNamespaceSecrets(t *testing.T) {
	body := readRepoFile(t, "deploy/kubernetes/storageclass.yaml")

	for _, want := range []string{
		"csi.storage.k8s.io/provisioner-secret-name: drive9-csi-${pvc.name}",
		"csi.storage.k8s.io/provisioner-secret-namespace: ${pvc.namespace}",
		"csi.storage.k8s.io/node-stage-secret-name: drive9-csi-${pvc.name}",
		"csi.storage.k8s.io/node-stage-secret-namespace: ${pvc.namespace}",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("storageclass.yaml missing %q", want)
		}
	}

	for _, stale := range []string{
		"csi.storage.k8s.io/provisioner-secret-name: drive9-csi-secret",
		"csi.storage.k8s.io/provisioner-secret-namespace: drive9-csi",
		"csi.storage.k8s.io/node-stage-secret-name: drive9-csi-secret",
		"csi.storage.k8s.io/node-stage-secret-namespace: drive9-csi",
	} {
		if strings.Contains(body, stale) {
			t.Fatalf("storageclass.yaml still contains stale global secret namespace %q", stale)
		}
	}
}

func TestControllerRBACCanReadPerPVCNamespaceLocalDrive9Secrets(t *testing.T) {
	body := readRepoFile(t, "deploy/kubernetes/rbac.yaml")

	want := strings.Join([]string{
		"  - apiGroups: [\"\"]",
		"    resources: [\"secrets\"]",
		"    verbs: [\"get\"]",
	}, "\n")
	if !strings.Contains(body, want) {
		t.Fatalf("rbac.yaml missing ClusterRole secret get rule:\n%s", want)
	}
	for _, stale := range []string{
		"kind: Role\nmetadata:\n  name: drive9-csi-controller-secrets",
		"kind: RoleBinding\nmetadata:\n  name: drive9-csi-controller-secrets",
	} {
		if strings.Contains(body, stale) {
			t.Fatalf("rbac.yaml still contains namespaced secret RBAC: %q", stale)
		}
	}
	if strings.Contains(body, "resourceNames: [\"drive9-csi-secret\"]") {
		t.Fatal("rbac.yaml still restricts secret reads to the old fixed secret name")
	}
}

func TestKubernetesExamplesUsePerPVCWorkloadNamespaceSecrets(t *testing.T) {
	secret := readRepoFile(t, "deploy/examples/kubernetes/secret.example.yaml")
	if strings.Contains(secret, "namespace: drive9-csi") {
		t.Fatal("secret.example.yaml should not force credentials into the driver namespace")
	}
	if !strings.Contains(secret, "name: drive9-csi-drive9-workspace") {
		t.Fatal("secret.example.yaml must use the default drive9-csi-<pvc-name> secret name")
	}
}

func TestE2EUsesPerPVCNamespaceLocalDrive9Secret(t *testing.T) {
	body := readRepoFile(t, "hack/e2e-k8s.sh")

	for _, want := range []string{
		"kind: Secret\nmetadata:\n  name: drive9-csi-drive9-workspace-e2e\n  namespace: $test_namespace",
		"write_storageclass()",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("e2e script missing namespace-local secret evidence %q", want)
		}
	}
	if strings.Contains(body, "csi.storage.k8s.io/.*-secret-namespace") {
		t.Fatal("e2e script should not rewrite CSI secret namespaces to the driver namespace")
	}
}

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	parts := append([]string{"..", ".."}, strings.Split(name, "/")...)
	body, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}
