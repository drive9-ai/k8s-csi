//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

type fileContract struct {
	path      string
	required  []string
	forbidden []string
}

var failures int

func main() {
	checkTextContracts()
	checkNodeManifest()
	checkControllerManifest()
	if failures != 0 {
		fmt.Fprintf(os.Stderr, "manifest-check: %d failure(s)\n", failures)
		os.Exit(1)
	}
	fmt.Println("manifest-check: ok")
}

func checkTextContracts() {
	contracts := []fileContract{
		{
			path: "deploy/kubernetes/storageclass.yaml",
			forbidden: []string{
				"csi.storage.k8s.io/provisioner-secret-name",
				"csi.storage.k8s.io/provisioner-secret-namespace",
				"csi.storage.k8s.io/node-stage-secret-name",
				"csi.storage.k8s.io/node-stage-secret-namespace",
				"${pvc.name}", "${pvc.namespace}",
				"remoteRootPrefix:", "remoteRoot:", "profile:",
				"attrTTL:", "entryTTL:", "dirTTL:",
				"perfEnabled:", "readdirPrefetch:",
				"readdirPrefetchMaxFiles:",
				"readdirPrefetchMaxFileBytes:",
				"readdirPrefetchMaxBytes:", "writebackBatchWindow:",
			},
		},
		{
			path: "deploy/kubernetes/volumeattributesclass.yaml",
			required: []string{
				"  attrTTL: 30s", "  entryTTL: 30s", "  dirTTL: 30s",
				"  perfEnabled: \"false\"",
			},
			forbidden: []string{
				"readdirPrefetch:", "readdirPrefetchMaxFiles:",
				"readdirPrefetchMaxFileBytes:",
				"readdirPrefetchMaxBytes:", "writebackBatchWindow:",
			},
		},
		{
			path:     "deploy/kubernetes/kustomization.yaml",
			required: []string{"volumeattributesclass.yaml"},
		},
		{
			path: "deploy/kubernetes/controller.yaml",
			required: []string{
				"registry.k8s.io/sig-storage/csi-provisioner:v6.3.0",
				"--feature-gates=VolumeAttributesClass=true",
				"--recover-node-mounts=disabled",
			},
		},
		{
			path: "deploy/sidecar/deployment.yaml",
			required: []string{
				"terminationGracePeriodSeconds: 60",
				"registry.invalid/drive9-csi:unpublished",
				"exec /usr/local/bin/drive9-csi supervise-sidecar-mount --",
				"/usr/local/bin/drive9 mount", "--direct-mount-strict",
				"DRIVE9_ATTR_TTL", "DRIVE9_ENTRY_TTL", "DRIVE9_DIR_TTL",
				"--attr-ttl", "--entry-ttl", "--dir-ttl", "value: 30s",
				"--foreground", "DRIVE9_PERF_ENABLED",
				"set -- --perf-dir /perf", "mountPath: /perf",
				"path: /var/lib/drive9-sidecar/demo/perf",
				"value: \"false\"", "DRIVE9_READDIR_PREFETCH",
				"DRIVE9_READDIR_PREFETCH_MAX_FILES",
				"DRIVE9_READDIR_PREFETCH_MAX_FILE_BYTES",
				"DRIVE9_READDIR_PREFETCH_MAX_BYTES",
				"DRIVE9_WRITEBACK_BATCH_WINDOW", "--readdir-prefetch",
				"--readdir-prefetch-max-files",
				"--readdir-prefetch-max-file-bytes",
				"--readdir-prefetch-max-bytes", "--writeback-batch-window",
			},
			forbidden: []string{
				"ghcr.io/drive9-ai/drive9-csi:", "exec drive9 mount",
				"DRIVE9_PERF_DIR", "- name: DRIVE9_READDIR_PREFETCH",
				"- name: DRIVE9_READDIR_PREFETCH_MAX_FILES",
				"- name: DRIVE9_READDIR_PREFETCH_MAX_FILE_BYTES",
				"- name: DRIVE9_READDIR_PREFETCH_MAX_BYTES",
				"- name: DRIVE9_WRITEBACK_BATCH_WINDOW",
			},
		},
		{
			path: "deploy/kubernetes/node.yaml",
			required: []string{
				"DRIVE9_CSI_NODE_NAME", "fieldPath: spec.nodeName",
				"DRIVE9_CSI_POD_NAME", "fieldPath: metadata.name",
				"--recover-node-mounts=enabled",
				"terminationGracePeriodSeconds: 120",
			},
			forbidden: []string{"--fusermount-source"},
		},
		{
			path: "Dockerfile",
			required: []string{
				"FROM --platform=$TARGETPLATFORM debian:bookworm-slim AS runtime",
				"/usr/local/bin/drive9 mount --direct-mount-strict --help",
				"COPY hack/drive9-csi-upload-perf.sh ",
				"/usr/local/bin/drive9-csi-upload-perf",
				"chmod +x /usr/local/bin/drive9-csi-upload-perf",
			},
			forbidden: []string{"fuse3", "/etc/fuse.conf", "user_allow_other"},
		},
		{
			path: "deploy/kubernetes/rbac.yaml",
			required: []string{
				"resources: [\"secrets\"]", "verbs: [\"get\"]",
				"volumeattributesclasses", "name: drive9-csi-node",
				"resources: [\"persistentvolumes\"]",
				"verbs: [\"get\", \"list\"]",
			},
		},
		{
			path: "deploy/examples/kubernetes/pvc.example.yaml",
			required: []string{
				"drive9.ai/secret-name:",
				"volumeAttributesClassName: drive9-coding-agent",
			},
		},
		{
			path:      "deploy/examples/kubernetes/secret.example.yaml",
			forbidden: []string{"namespace: drive9-csi", "drive9-csi-drive9-workspace"},
		},
		{
			path: "deploy/overlays/local/kustomization.yaml",
			required: []string{
				"resources:\n  - ../../kubernetes",
				"name: registry.invalid/drive9-csi",
				"newName: ghcr.io/drive9-ai/drive9-csi", "newTag: local",
			},
			forbidden: []string{"  - ../..\n", ":latest", "drive9-aff1023"},
		},
		{
			path:     "Makefile",
			required: []string{"kubectl apply -k deploy/overlays/local"},
		},
	}
	for _, contract := range contracts {
		body := readFile(contract.path)
		for _, required := range contract.required {
			if !strings.Contains(body, required) {
				failf("%s missing %q", contract.path, required)
			}
		}
		for _, forbidden := range contract.forbidden {
			if strings.Contains(body, forbidden) {
				failf("%s contains forbidden %q", contract.path, forbidden)
			}
		}
	}
}

func checkNodeManifest() {
	var daemonSet appsv1.DaemonSet
	decodeYAML("deploy/kubernetes/node.yaml", &daemonSet)
	pod := daemonSet.Spec.Template.Spec
	if pod.PriorityClassName != "system-node-critical" {
		failf("node priorityClassName = %q", pod.PriorityClassName)
	}
	if len(pod.InitContainers) != 1 {
		failf("node init container count = %d", len(pod.InitContainers))
		return
	}
	installer := pod.InitContainers[0]
	node, ok := findContainer(pod.Containers, "drive9-csi")
	if !ok {
		return
	}
	if installer.Name != "install-host-binaries" {
		failf("node init container = %q", installer.Name)
	}
	if installer.Image != node.Image {
		failf("installer image differs from node image")
	}
	wantInstallerArgs := []string{
		"install-host-binaries",
		"--host-state-dir=/var/lib/drive9-csi",
		"--drive9-source=/usr/local/bin/drive9",
		"--launcher-source=/usr/local/bin/drive9-csi-launcher",
	}
	if !slices.Equal(installer.Args, wantInstallerArgs) {
		failf("installer args = %v", installer.Args)
	}
	for _, arg := range []string{
		"--service-mode=node", "--recover-node-mounts=enabled",
	} {
		if !slices.Contains(node.Args, arg) {
			failf("node args missing %q", arg)
		}
	}
	bidirectional := corev1.MountPropagationBidirectional
	checkMount(installer, "state-dir", "/var/lib/drive9-csi", false, nil)
	checkMount(node, "kubelet-dir", "/var/lib/kubelet", false, &bidirectional)
	checkMount(node, "state-dir", "/var/lib/drive9-csi", false, nil)
	checkMount(node, "host-proc", "/host-proc", true, nil)
	checkMount(node, "host-runtime", "/run/drive9-csi", false, nil)
	checkMount(node, "dev-fuse", "/dev/fuse", false, nil)

	volumes := make(map[string]corev1.Volume, len(pod.Volumes))
	for _, volume := range pod.Volumes {
		volumes[volume.Name] = volume
	}
	checkHostPath(volumes, "kubelet-dir", "/var/lib/kubelet",
		corev1.HostPathDirectory)
	checkHostPath(volumes, "state-dir", "/var/lib/drive9-csi",
		corev1.HostPathDirectoryOrCreate)
	checkHostPath(volumes, "host-proc", "/proc", corev1.HostPathDirectory)
	checkHostPath(volumes, "host-runtime", "/run/drive9-csi",
		corev1.HostPathDirectoryOrCreate)
	checkHostPath(volumes, "dev-fuse", "/dev/fuse", corev1.HostPathCharDev)
	for role, image := range map[string]string{
		"node": node.Image, "installer": installer.Image,
	} {
		checkBaseImage(role, image)
	}
}

func checkControllerManifest() {
	var deployment appsv1.Deployment
	decodeYAML("deploy/kubernetes/controller.yaml", &deployment)
	pod := deployment.Spec.Template.Spec
	if len(pod.InitContainers) != 0 {
		failf("controller has %d init containers", len(pod.InitContainers))
	}
	controller, ok := findContainer(pod.Containers, "drive9-csi")
	if !ok {
		return
	}
	for _, arg := range []string{
		"--service-mode=controller", "--recover-node-mounts=disabled",
	} {
		if !slices.Contains(controller.Args, arg) {
			failf("controller args missing %q", arg)
		}
	}
	for _, mount := range controller.VolumeMounts {
		switch mount.Name {
		case "host-proc", "host-runtime", "state-dir", "dev-fuse", "kubelet-dir":
			failf("controller contains node-only mount %q", mount.Name)
		}
	}
	for _, volume := range pod.Volumes {
		if volume.HostPath != nil {
			failf("controller contains hostPath volume %q", volume.Name)
		}
	}
	checkBaseImage("controller", controller.Image)
}

func checkBaseImage(role string, image string) {
	if image != "registry.invalid/drive9-csi:unpublished" {
		failf("%s image = %q", role, image)
	}
}

func findContainer(containers []corev1.Container, name string) (corev1.Container, bool) {
	for _, container := range containers {
		if container.Name == name {
			return container, true
		}
	}
	failf("missing container %q", name)
	return corev1.Container{}, false
}

func checkMount(
	container corev1.Container,
	name string,
	path string,
	readOnly bool,
	propagation *corev1.MountPropagationMode,
) {
	for _, mount := range container.VolumeMounts {
		if mount.Name != name {
			continue
		}
		if mount.MountPath != path || mount.ReadOnly != readOnly {
			failf("mount %s = %s readOnly=%t", name, mount.MountPath,
				mount.ReadOnly)
		}
		if !equalPropagation(mount.MountPropagation, propagation) {
			failf("mount %s propagation = %v", name, mount.MountPropagation)
		}
		return
	}
	failf("missing mount %q", name)
}

func equalPropagation(
	got *corev1.MountPropagationMode,
	want *corev1.MountPropagationMode,
) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}

func checkHostPath(
	volumes map[string]corev1.Volume,
	name string,
	path string,
	kind corev1.HostPathType,
) {
	volume, ok := volumes[name]
	if !ok || volume.HostPath == nil {
		failf("missing hostPath volume %q", name)
		return
	}
	if volume.HostPath.Path != path || volume.HostPath.Type == nil ||
		*volume.HostPath.Type != kind {
		failf("hostPath %s = %#v", name, volume.HostPath)
	}
}

func decodeYAML(path string, target any) {
	body := readFile(path)
	jsonBody, err := utilyaml.ToJSON([]byte(body))
	if err != nil {
		failf("convert %s to JSON: %v", path, err)
		return
	}
	if err := json.Unmarshal(jsonBody, target); err != nil {
		failf("parse %s: %v", path, err)
	}
}

func readFile(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		failf("read %s: %v", path, err)
		return ""
	}
	return string(body)
}

func failf(format string, args ...any) {
	failures++
	fmt.Fprintf(os.Stderr, "manifest-check: "+format+"\n", args...)
}
