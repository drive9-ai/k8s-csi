package driver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
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
		t.Fatal("node.yaml must bound CSI gRPC shutdown")
	}
}

func TestKubernetesManifestNodeProvidesHostRuntimeAndInstaller(t *testing.T) {
	var daemonSet appsv1.DaemonSet
	decodeRepoYAML(t, "deploy/kubernetes/node.yaml", &daemonSet)
	pod := daemonSet.Spec.Template.Spec
	if pod.PriorityClassName != "system-node-critical" {
		t.Fatalf("Node priorityClassName = %q, want system-node-critical", pod.PriorityClassName)
	}
	if len(pod.InitContainers) != 1 {
		t.Fatalf("Node init container count = %d, want 1", len(pod.InitContainers))
	}

	installer := pod.InitContainers[0]
	if installer.Name != "install-host-binaries" {
		t.Fatalf("Node init container = %q, want install-host-binaries", installer.Name)
	}
	node := requiredContainer(t, pod.Containers, "drive9-csi")
	if installer.Image != node.Image {
		t.Fatalf("installer image = %q, node image = %q", installer.Image, node.Image)
	}
	wantInstallerArgs := []string{
		"install-host-binaries",
		"--host-state-dir=/var/lib/drive9-csi",
		"--drive9-source=/usr/local/bin/drive9",
		"--launcher-source=/usr/local/bin/drive9-csi-launcher",
		"--fusermount-source=/usr/bin/fusermount3",
	}
	if !slices.Equal(installer.Args, wantInstallerArgs) {
		t.Fatalf("installer args = %v, want %v", installer.Args, wantInstallerArgs)
	}
	assertVolumeMount(t, installer.VolumeMounts, "state-dir", "/var/lib/drive9-csi", false, nil)

	for _, arg := range []string{"--service-mode=node", "--recover-node-mounts=enabled"} {
		if !slices.Contains(node.Args, arg) {
			t.Fatalf("Node driver args missing %q", arg)
		}
	}
	bidirectional := corev1.MountPropagationBidirectional
	assertVolumeMount(t, node.VolumeMounts, "kubelet-dir", "/var/lib/kubelet", false, &bidirectional)
	assertVolumeMount(t, node.VolumeMounts, "state-dir", "/var/lib/drive9-csi", false, nil)
	assertVolumeMount(t, node.VolumeMounts, "host-proc", "/host-proc", true, nil)
	assertVolumeMount(t, node.VolumeMounts, "host-runtime", "/run/drive9-csi", false, nil)
	assertVolumeMount(t, node.VolumeMounts, "dev-fuse", "/dev/fuse", false, nil)

	volumes := make(map[string]corev1.Volume, len(pod.Volumes))
	for _, volume := range pod.Volumes {
		volumes[volume.Name] = volume
	}
	assertHostPathVolume(t, volumes, "kubelet-dir", "/var/lib/kubelet", corev1.HostPathDirectory)
	assertHostPathVolume(t, volumes, "state-dir", "/var/lib/drive9-csi", corev1.HostPathDirectoryOrCreate)
	assertHostPathVolume(t, volumes, "host-proc", "/proc", corev1.HostPathDirectory)
	assertHostPathVolume(t, volumes, "host-runtime", "/run/drive9-csi", corev1.HostPathDirectoryOrCreate)
	assertHostPathVolume(t, volumes, "dev-fuse", "/dev/fuse", corev1.HostPathCharDev)
}

func TestKubernetesManifestControllerHasNoNodePrerequisites(t *testing.T) {
	var deployment appsv1.Deployment
	decodeRepoYAML(t, "deploy/kubernetes/controller.yaml", &deployment)
	pod := deployment.Spec.Template.Spec
	if len(pod.InitContainers) != 0 {
		t.Fatalf("controller init container count = %d, want 0", len(pod.InitContainers))
	}
	controller := requiredContainer(t, pod.Containers, "drive9-csi")
	for _, arg := range []string{"--service-mode=controller", "--recover-node-mounts=disabled"} {
		if !slices.Contains(controller.Args, arg) {
			t.Fatalf("controller args missing %q", arg)
		}
	}
	for _, mount := range controller.VolumeMounts {
		switch mount.Name {
		case "host-proc", "host-runtime", "state-dir", "dev-fuse", "kubelet-dir":
			t.Fatalf("controller contains Node-only mount %q", mount.Name)
		}
	}
	for _, volume := range pod.Volumes {
		if volume.HostPath != nil {
			t.Fatalf("controller contains hostPath volume %q", volume.Name)
		}
	}
}

func TestKubernetesManifestUsesExplicitBaseAndLocalImageContracts(t *testing.T) {
	var daemonSet appsv1.DaemonSet
	decodeRepoYAML(t, "deploy/kubernetes/node.yaml", &daemonSet)
	var deployment appsv1.Deployment
	decodeRepoYAML(t, "deploy/kubernetes/controller.yaml", &deployment)

	node := requiredContainer(t, daemonSet.Spec.Template.Spec.Containers, "drive9-csi")
	installer := requiredContainer(t, daemonSet.Spec.Template.Spec.InitContainers, "install-host-binaries")
	controller := requiredContainer(t, deployment.Spec.Template.Spec.Containers, "drive9-csi")
	for role, image := range map[string]string{
		"node":       node.Image,
		"installer":  installer.Image,
		"controller": controller.Image,
	} {
		if image != "registry.invalid/drive9-csi:unpublished" {
			t.Fatalf("%s image = %q; manifest base must fail closed until an overlay selects an image", role, image)
		}
	}

	local := readRepoFile(t, "deploy/overlays/local/kustomization.yaml")
	for _, want := range []string{
		"resources:\n  - ../../kubernetes",
		"name: registry.invalid/drive9-csi",
		"newName: ghcr.io/drive9-ai/drive9-csi",
		"newTag: local",
	} {
		if !strings.Contains(local, want) {
			t.Fatalf("local image overlay missing %q", want)
		}
	}
	if strings.Contains(local, "  - ../..\n") {
		t.Fatal("local overlay must not reference a parent kustomization that contains the overlay")
	}
	for _, forbidden := range []string{":latest", "drive9-aff1023-csi-ef5fab2"} {
		if strings.Contains(node.Image+installer.Image+controller.Image+local, forbidden) {
			t.Fatalf("Kubernetes image contract contains stale or mutable reference %q", forbidden)
		}
	}
	if makefile := readRepoFile(t, "Makefile"); !strings.Contains(makefile, "kubectl apply -k deploy/overlays/local") {
		t.Fatal("Makefile manifests target must select the explicit local overlay")
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

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	parts := append([]string{"..", ".."}, strings.Split(name, "/")...)
	body, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

func decodeRepoYAML(t *testing.T, name string, target any) {
	t.Helper()
	body := readRepoFile(t, name)
	jsonBody, err := utilyaml.ToJSON([]byte(body))
	if err != nil {
		t.Fatalf("convert %s to JSON: %v", name, err)
	}
	if err := json.Unmarshal(jsonBody, target); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
}

func requiredContainer(t *testing.T, containers []corev1.Container, name string) corev1.Container {
	t.Helper()
	for _, container := range containers {
		if container.Name == name {
			return container
		}
	}
	t.Fatalf("missing container %q", name)
	return corev1.Container{}
}

func assertVolumeMount(
	t *testing.T,
	mounts []corev1.VolumeMount,
	name string,
	path string,
	readOnly bool,
	propagation *corev1.MountPropagationMode,
) {
	t.Helper()
	for _, mount := range mounts {
		if mount.Name != name {
			continue
		}
		if mount.MountPath != path || mount.ReadOnly != readOnly {
			t.Fatalf("volume mount %q = path %q readOnly=%t", name, mount.MountPath, mount.ReadOnly)
		}
		if propagation == nil && mount.MountPropagation != nil {
			t.Fatalf("volume mount %q has unexpected propagation %q", name, *mount.MountPropagation)
		}
		if propagation != nil && (mount.MountPropagation == nil || *mount.MountPropagation != *propagation) {
			t.Fatalf("volume mount %q propagation = %v, want %q", name, mount.MountPropagation, *propagation)
		}
		return
	}
	t.Fatalf("missing volume mount %q", name)
}

func assertHostPathVolume(
	t *testing.T,
	volumes map[string]corev1.Volume,
	name string,
	path string,
	typeValue corev1.HostPathType,
) {
	t.Helper()
	volume, ok := volumes[name]
	if !ok || volume.HostPath == nil {
		t.Fatalf("missing hostPath volume %q", name)
	}
	if volume.HostPath.Path != path {
		t.Fatalf("hostPath volume %q path = %q, want %q", name, volume.HostPath.Path, path)
	}
	if volume.HostPath.Type == nil || *volume.HostPath.Type != typeValue {
		t.Fatalf("hostPath volume %q type = %v, want %q", name, volume.HostPath.Type, typeValue)
	}
}
