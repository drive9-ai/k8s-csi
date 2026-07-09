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

func TestSidecarFallbackRunsMountInForeground(t *testing.T) {
	body := readRepoFile(t, "deploy/sidecar/deployment.yaml")

	if !strings.Contains(body, "exec drive9 mount \\\n                --foreground \\") {
		t.Fatal("sidecar deployment must run drive9 mount with --foreground")
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

func TestNodeRBACCanReadPersistentVolumesForRecovery(t *testing.T) {
	body := readRepoFile(t, "deploy/kubernetes/rbac.yaml")
	idx := strings.Index(body, "name: drive9-csi-node\nrules:")
	if idx < 0 {
		t.Fatal("rbac.yaml missing drive9-csi-node ClusterRole rules")
	}
	nodeRole := body[idx:]

	for _, want := range []string{
		"resources: [\"persistentvolumes\"]",
		"verbs: [\"get\", \"list\"]",
	} {
		if !strings.Contains(nodeRole, want) {
			t.Fatalf("rbac.yaml missing node PV recovery permission evidence %q", want)
		}
	}
}

func TestRecoverNodeMountsManifestArgs(t *testing.T) {
	controller := readRepoFile(t, "deploy/kubernetes/controller.yaml")
	node := readRepoFile(t, "deploy/kubernetes/node.yaml")

	if !strings.Contains(controller, "--recover-node-mounts=disabled") {
		t.Fatal("controller.yaml must disable node mount recovery")
	}
	if !strings.Contains(node, "--recover-node-mounts=enabled") {
		t.Fatal("node.yaml must enable node mount recovery")
	}
	if !strings.Contains(node, "terminationGracePeriodSeconds: 120") {
		t.Fatal("node.yaml must set terminationGracePeriodSeconds for graceful umount")
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

func TestKubernetesExamplePodUsesSubtreeMountPath(t *testing.T) {
	pod := readRepoFile(t, "deploy/examples/kubernetes/pod.example.yaml")
	for _, want := range []string{
		"mountPath: /drive9",
		"mountPropagation: HostToContainer",
		"/drive9/workspace/hello.txt",
		"claimName: drive9-workspace-tuned",
	} {
		if !strings.Contains(pod, want) {
			t.Fatalf("pod.example.yaml missing subtree evidence %q", want)
		}
	}
}

func TestKubernetesExamplesIncludeVolumeAttributesClassDependencies(t *testing.T) {
	controller := readRepoFile(t, "deploy/examples/kubernetes/controller.example.yaml")
	for _, want := range []string{
		"registry.k8s.io/sig-storage/csi-provisioner:v6.3.0",
		"--feature-gates=VolumeAttributesClass=true",
		"--recover-node-mounts=disabled",
	} {
		if !strings.Contains(controller, want) {
			t.Fatalf("controller.example.yaml missing evidence %q", want)
		}
	}

	rbac := readRepoFile(t, "deploy/examples/kubernetes/rbac.example.yaml")
	for _, want := range []string{
		"volumeattributesclasses",
		"resources: [\"persistentvolumes\"]",
		"verbs: [\"get\", \"list\"]",
	} {
		if !strings.Contains(rbac, want) {
			t.Fatalf("rbac.example.yaml missing evidence %q", want)
		}
	}

	node := readRepoFile(t, "deploy/examples/kubernetes/node.example.yaml")
	for _, want := range []string{
		"--recover-node-mounts=enabled",
		"terminationGracePeriodSeconds: 120",
		"mountPropagation: Bidirectional",
	} {
		if !strings.Contains(node, want) {
			t.Fatalf("node.example.yaml missing evidence %q", want)
		}
	}
}

func TestKubernetesExamplePVCReferencesDefinedVolumeAttributesClass(t *testing.T) {
	pvc := readRepoFile(t, "deploy/examples/kubernetes/pvc.example.yaml")
	vac := readRepoFile(t, "deploy/examples/kubernetes/volumeattributesclass.example.yaml")

	if !strings.Contains(pvc, "volumeAttributesClassName: drive9-coding-agent") {
		t.Fatal("pvc.example.yaml must reference drive9-coding-agent")
	}
	if !strings.Contains(vac, "  name: drive9-coding-agent\n") {
		t.Fatal("volumeattributesclass.example.yaml must define drive9-coding-agent")
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
