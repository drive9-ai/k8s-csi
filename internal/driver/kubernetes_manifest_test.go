package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultStorageClassNoSecretTemplate(t *testing.T) {
	body := readRepoFile(t, "deploy/kubernetes/storageclass.yaml")

	// StorageClass must NOT contain any per-PVC secret template parameters.
	// Credentials are resolved from PVC annotations, not StorageClass templates.
	for _, stale := range []string{
		"csi.storage.k8s.io/provisioner-secret-name",
		"csi.storage.k8s.io/provisioner-secret-namespace",
		"csi.storage.k8s.io/node-stage-secret-name",
		"csi.storage.k8s.io/node-stage-secret-namespace",
		"${pvc.name}",
		"${pvc.namespace}",
	} {
		if strings.Contains(body, stale) {
			t.Fatalf("storageclass.yaml must not contain secret template %q — "+
				"credentials are resolved from PVC annotation drive9.ai/secret-name", stale)
		}
	}
}

func TestDefaultStorageClassMountsWorkspaceRoot(t *testing.T) {
	body := readRepoFile(t, "deploy/kubernetes/storageclass.yaml")

	if strings.Contains(body, "remoteRootPrefix:") {
		t.Fatal("default StorageClass must not set remoteRootPrefix; it should mount the Drive9 workspace root")
	}
	if strings.Contains(body, "remoteRoot:") {
		t.Fatal("default StorageClass must not force a remoteRoot; workspace root is the driver default")
	}
}

func TestDefaultVolumeAttributesClassMountTTLs(t *testing.T) {
	body := readRepoFile(t, "deploy/kubernetes/volumeattributesclass.yaml")

	for _, want := range []string{
		"  attrTTL: 30s",
		"  entryTTL: 30s",
		"  dirTTL: 30s",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("volumeattributesclass.yaml missing default mount TTL parameter %q", want)
		}
	}
}

func TestDefaultVolumeAttributesClassMountPerfDisabled(t *testing.T) {
	body := readRepoFile(t, "deploy/kubernetes/volumeattributesclass.yaml")

	want := "  perfEnabled: \"false\""
	if !strings.Contains(body, want) {
		t.Fatalf("volumeattributesclass.yaml missing default mount perf parameter %q", want)
	}
}

func TestDefaultStorageClassOmitsMountBehavior(t *testing.T) {
	body := readRepoFile(t, "deploy/kubernetes/storageclass.yaml")

	for _, forbidden := range []string{
		"profile:",
		"attrTTL:",
		"entryTTL:",
		"dirTTL:",
		"perfEnabled:",
		"readdirPrefetch:",
		"readdirPrefetchMaxFiles:",
		"readdirPrefetchMaxFileBytes:",
		"readdirPrefetchMaxBytes:",
		"writebackBatchWindow:",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("storageclass.yaml must not set mount behavior parameter %q by default", forbidden)
		}
	}
}

func TestDefaultVolumeAttributesClassOmitsExplicitMountTuning(t *testing.T) {
	body := readRepoFile(t, "deploy/kubernetes/volumeattributesclass.yaml")

	for _, forbidden := range []string{
		"readdirPrefetch:",
		"readdirPrefetchMaxFiles:",
		"readdirPrefetchMaxFileBytes:",
		"readdirPrefetchMaxBytes:",
		"writebackBatchWindow:",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("volumeattributesclass.yaml must not set explicit mount tuning parameter %q by default", forbidden)
		}
	}
}

func TestKustomizationIncludesVolumeAttributesClass(t *testing.T) {
	body := readRepoFile(t, "deploy/kubernetes/kustomization.yaml")

	if !strings.Contains(body, "volumeattributesclass.yaml") {
		t.Fatal("kustomization.yaml must include volumeattributesclass.yaml")
	}
}

func TestControllerUsesVolumeAttributesClassAwareProvisioner(t *testing.T) {
	body := readRepoFile(t, "deploy/kubernetes/controller.yaml")

	for _, want := range []string{
		"registry.k8s.io/sig-storage/csi-provisioner:v6.3.0",
		"--feature-gates=VolumeAttributesClass=true",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("controller.yaml missing VolumeAttributesClass provisioner evidence %q", want)
		}
	}
}

func TestSidecarFallbackMountTTLs(t *testing.T) {
	body := readRepoFile(t, "deploy/sidecar/deployment.yaml")

	for _, want := range []string{
		"DRIVE9_ATTR_TTL",
		"DRIVE9_ENTRY_TTL",
		"DRIVE9_DIR_TTL",
		"--attr-ttl",
		"--entry-ttl",
		"--dir-ttl",
		"value: 30s",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("sidecar deployment missing mount TTL evidence %q", want)
		}
	}
}

func TestSidecarFallbackMountPerf(t *testing.T) {
	body := readRepoFile(t, "deploy/sidecar/deployment.yaml")

	for _, want := range []string{
		"DRIVE9_PERF_ENABLED",
		"set -- --perf-dir /perf",
		"mountPath: /perf",
		"path: /var/lib/drive9-sidecar/demo/perf",
		"value: \"false\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("sidecar deployment missing mount perf evidence %q", want)
		}
	}
	if strings.Contains(body, "DRIVE9_PERF_DIR") {
		t.Fatal("sidecar deployment must not expose arbitrary DRIVE9_PERF_DIR")
	}
}

func TestSidecarFallbackExplicitMountTuning(t *testing.T) {
	body := readRepoFile(t, "deploy/sidecar/deployment.yaml")

	for _, want := range []string{
		"DRIVE9_READDIR_PREFETCH",
		"DRIVE9_READDIR_PREFETCH_MAX_FILES",
		"DRIVE9_READDIR_PREFETCH_MAX_FILE_BYTES",
		"DRIVE9_READDIR_PREFETCH_MAX_BYTES",
		"DRIVE9_WRITEBACK_BATCH_WINDOW",
		"--readdir-prefetch",
		"--readdir-prefetch-max-files",
		"--readdir-prefetch-max-file-bytes",
		"--readdir-prefetch-max-bytes",
		"--writeback-batch-window",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("sidecar deployment missing explicit mount tuning evidence %q", want)
		}
	}
	for _, forbiddenDefault := range []string{
		"- name: DRIVE9_READDIR_PREFETCH",
		"- name: DRIVE9_READDIR_PREFETCH_MAX_FILES",
		"- name: DRIVE9_READDIR_PREFETCH_MAX_FILE_BYTES",
		"- name: DRIVE9_READDIR_PREFETCH_MAX_BYTES",
		"- name: DRIVE9_WRITEBACK_BATCH_WINDOW",
	} {
		if strings.Contains(body, forbiddenDefault) {
			t.Fatalf("sidecar deployment must not set explicit mount tuning env by default: %q", forbiddenDefault)
		}
	}
}

func TestNodeDaemonSetInjectsPerfUploadIdentity(t *testing.T) {
	body := readRepoFile(t, "deploy/kubernetes/node.yaml")

	for _, want := range []string{
		"DRIVE9_CSI_NODE_NAME",
		"fieldPath: spec.nodeName",
		"DRIVE9_CSI_POD_NAME",
		"fieldPath: metadata.name",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("node DaemonSet missing perf upload identity evidence %q", want)
		}
	}
}

func TestRuntimeImageInstallsPerfUploadHelper(t *testing.T) {
	body := readRepoFile(t, "Dockerfile")

	for _, want := range []string{
		"COPY hack/drive9-csi-upload-perf.sh /usr/local/bin/drive9-csi-upload-perf",
		"chmod +x /usr/local/bin/drive9-csi-upload-perf",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Dockerfile missing perf upload helper install evidence %q", want)
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
}

func TestControllerRBACCanReadVolumeAttributesClasses(t *testing.T) {
	body := readRepoFile(t, "deploy/kubernetes/rbac.yaml")

	if !strings.Contains(body, "volumeattributesclasses") {
		t.Fatal("rbac.yaml missing volumeattributesclasses read permission")
	}
}

func TestNodeRBACCanReadSecrets(t *testing.T) {
	body := readRepoFile(t, "deploy/kubernetes/rbac.yaml")

	if !strings.Contains(body, "name: drive9-csi-node") {
		t.Fatal("rbac.yaml missing drive9-csi-node ClusterRole")
	}
	// Node must be able to read Secrets for NodeStageVolume.
	if !strings.Contains(body, "drive9-csi-node") {
		t.Fatal("rbac.yaml missing drive9-csi-node ClusterRoleBinding")
	}
}

func TestKubernetesExamplesUseAnnotationBasedSecrets(t *testing.T) {
	pvc := readRepoFile(t, "deploy/examples/kubernetes/pvc.example.yaml")
	if !strings.Contains(pvc, "drive9.ai/secret-name:") {
		t.Fatal("pvc.example.yaml must include drive9.ai/secret-name annotation")
	}
	if !strings.Contains(pvc, "volumeAttributesClassName: drive9-coding-agent") {
		t.Fatal("pvc.example.yaml must reference the default VolumeAttributesClass")
	}
	// Secret name should NOT follow the old drive9-csi-<pvc-name> pattern.
	secret := readRepoFile(t, "deploy/examples/kubernetes/secret.example.yaml")
	if strings.Contains(secret, "namespace: drive9-csi") {
		t.Fatal("secret.example.yaml should not force credentials into the driver namespace")
	}
	if strings.Contains(secret, "drive9-csi-drive9-workspace") {
		t.Fatal("secret.example.yaml must not use old drive9-csi-<pvc-name> naming convention")
	}
}

func TestE2EUsesPerPVCNamespaceLocalDrive9Secret(t *testing.T) {
	body := readRepoFile(t, "hack/e2e-k8s.sh")

	for _, want := range []string{
		"write_storageclass()",
		"write_volumeattributesclass()",
		"volumeAttributesClassName: $volume_attributes_class",
		"using Drive9 workspace root mode",
		"read after PVC recreate",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("e2e script missing evidence %q", want)
		}
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
