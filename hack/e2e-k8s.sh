#!/usr/bin/env bash

fail() {
	echo "error: $*" >&2
	exit 1
}

info() {
	echo "e2e: $*" >&2
}

need_cmd() {
	local name="$1"

	command -v "$name" >/dev/null 2>&1 || fail "missing command: $name"
}

need_env() {
	local name="$1"
	local value="${!name:-}"

	[[ "$value" != "" ]] || fail "$name is required"
}

cleanup() {
	local exit_code="$?"

	if [[ "${DRIVE9_CSI_E2E_KEEP:-}" == "1" ]]; then
		info "keeping e2e resources because DRIVE9_CSI_E2E_KEEP=1"
		info "temporary directory: $tmp_dir"
		exit "$exit_code"
	fi

	if [[ "$test_namespace" != "" ]]; then
		kubectl delete namespace "$test_namespace" \
			--ignore-not-found >/dev/null 2>&1
	fi

	kubectl delete storageclass "$storage_class" \
		--ignore-not-found >/dev/null 2>&1

	if [[ "$manifest_dir" != "" && -d "$manifest_dir" ]]; then
		kubectl delete -f "$manifest_dir" \
			--ignore-not-found >/dev/null 2>&1
	fi

	if [[ "$tmp_dir" != "" && -d "$tmp_dir" ]]; then
		rm -rf "$tmp_dir"
	fi

	exit "$exit_code"
}

ensure_absent() {
	local kind="$1"
	local name="$2"

	if kubectl get "$kind" "$name" >/dev/null 2>&1; then
		fail "$kind/$name already exists; use a clean e2e cluster"
	fi
}

copy_manifest() {
	local source="$1"
	local target="$2"

	awk -v image="$DRIVE9_CSI_IMAGE" -v namespace="$driver_namespace" '
		$0 ~ /^[[:space:]]*image: ghcr.io\/drive9-ai\/drive9-csi:/ {
			sub(/image: .*/, "image: " image)
		}
		$0 == "  namespace: drive9-csi" {
			print "  namespace: " namespace
			next
		}
		{ print }
	' "$repo_root/deploy/kubernetes/$source" > "$manifest_dir/$target" ||
		fail "render $source"
}

write_namespace() {
	cat > "$manifest_dir/namespace.yaml" <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $driver_namespace
EOF
}

write_rbac() {
	awk -v namespace="$driver_namespace" '
		$0 == "  namespace: drive9-csi" {
			print "  namespace: " namespace
			next
		}
		{ print }
	' "$repo_root/deploy/kubernetes/rbac.yaml" > "$manifest_dir/rbac.yaml" ||
		fail "render rbac"
}

write_storageclass() {
	awk \
		-v storage_class="$storage_class" \
		-v root_prefix="$DRIVE9_REMOTE_ROOT_PREFIX" \
		-v profile="$DRIVE9_PROFILE" '
		$0 == "  name: drive9-rwo" {
			print "  name: " storage_class
			next
		}
		$0 == "parameters:" {
			print
			if (root_prefix != "") {
				print "  remoteRootPrefix: " root_prefix
			}
			next
		}
		$0 ~ /^  remoteRootPrefix:/ {
			next
		}
		$0 ~ /^  profile:/ {
			print "  profile: " profile
			next
		}
		$0 ~ /^reclaimPolicy:/ {
			print "reclaimPolicy: Delete"
			next
		}
		{ print }
	' "$repo_root/deploy/kubernetes/storageclass.yaml" \
		> "$manifest_dir/storageclass.yaml" ||
		fail "render storageclass"
}

write_test_workload() {
	cat > "$tmp_dir/workload.yaml" <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $test_namespace
---
apiVersion: v1
kind: Secret
metadata:
  name: drive9-csi-drive9-workspace-e2e
  namespace: $test_namespace
type: Opaque
stringData:
  server: |-
    $DRIVE9_SERVER
  apiKey: |-
    $DRIVE9_API_KEY
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: drive9-workspace-e2e
  namespace: $test_namespace
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: $storage_class
  resources:
    requests:
      storage: 1Gi
EOF
}

write_test_pod() {
	local pod_name="$1"
	local target="$2"

	cat > "$target" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
  namespace: $test_namespace
spec:
  restartPolicy: Never
  containers:
    - name: app
      image: busybox:1.36
      command: ["/bin/sh", "-c", "sleep 3600"]
      volumeMounts:
        - name: workspace
          mountPath: /workspace
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: drive9-workspace-e2e
EOF
}

write_test_pod_on_node() {
	local pod_name="$1"
	local target="$2"
	local node_name="$3"

	cat > "$target" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
  namespace: $test_namespace
spec:
  restartPolicy: Never
  nodeName: $node_name
  containers:
    - name: app
      image: busybox:1.36
      command: ["/bin/sh", "-c", "sleep 3600"]
      volumeMounts:
        - name: workspace
          mountPath: /workspace
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: drive9-workspace-e2e
EOF
}

write_second_pvc() {
	cat > "$tmp_dir/workload-b.yaml" <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: drive9-csi-drive9-workspace-e2e-b
  namespace: $test_namespace
type: Opaque
stringData:
  server: |-
    $DRIVE9_SERVER
  apiKey: |-
    $DRIVE9_API_KEY
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: drive9-workspace-e2e-b
  namespace: $test_namespace
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: $storage_class
  resources:
    requests:
      storage: 1Gi
EOF
}

write_multi_pvc_pod() {
	local pod_name="$1"
	local target="$2"

	cat > "$target" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
  namespace: $test_namespace
spec:
  restartPolicy: Never
  containers:
    - name: app
      image: busybox:1.36
      command: ["/bin/sh", "-c", "sleep 3600"]
      volumeMounts:
        - name: workspace-a
          mountPath: /workspace-a
        - name: workspace-b
          mountPath: /workspace-b
  volumes:
    - name: workspace-a
      persistentVolumeClaim:
        claimName: drive9-workspace-e2e
    - name: workspace-b
      persistentVolumeClaim:
        claimName: drive9-workspace-e2e-b
EOF
}

wait_for_pv_deleted() {
	local pv_name="$1"
	local attempt

	for attempt in {1..60}; do
		if ! kubectl get pv "$pv_name" >/dev/null 2>&1; then
			return 0
		fi
		sleep 5
	done

	fail "PV $pv_name was not deleted after PVC deletion"
}

repo_root="$(cd "${0%/*}/.." && pwd)" || exit 1
tmp_dir=""
manifest_dir=""
driver_namespace="${DRIVE9_CSI_E2E_DRIVER_NAMESPACE:-drive9-csi-e2e-driver}"
test_namespace="${DRIVE9_CSI_E2E_NAMESPACE:-drive9-csi-e2e}"
storage_class="${DRIVE9_CSI_E2E_STORAGE_CLASS:-drive9-rwo-e2e}"

need_cmd kubectl
need_env DRIVE9_SERVER
need_env DRIVE9_API_KEY
need_env DRIVE9_CSI_IMAGE

if [[ "${DRIVE9_CSI_E2E_CONFIRM:-}" != "1" ]]; then
	fail "set DRIVE9_CSI_E2E_CONFIRM=1 to mutate the current cluster"
fi

if [[ "$DRIVE9_CSI_IMAGE" == *":latest" ]]; then
	fail "DRIVE9_CSI_IMAGE must not use :latest"
fi

DRIVE9_REMOTE_ROOT_PREFIX="${DRIVE9_REMOTE_ROOT_PREFIX:-}"
DRIVE9_PROFILE="${DRIVE9_PROFILE:-coding-agent}"

if [[ "$driver_namespace" == "$test_namespace" ]]; then
	fail "driver and test namespaces must be different"
fi

ensure_absent namespace "$driver_namespace"
ensure_absent namespace "$test_namespace"
ensure_absent clusterrole drive9-csi-controller
ensure_absent clusterrolebinding drive9-csi-controller
ensure_absent csidriver csi.drive9.ai
ensure_absent storageclass "$storage_class"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/drive9-csi-e2e.XXXXXX")" ||
	fail "create temporary directory"
manifest_dir="$tmp_dir/manifests"
mkdir -p "$manifest_dir" || fail "create manifest directory"
umask 077
trap cleanup EXIT INT TERM

info "using Kubernetes context: $(kubectl config current-context)"
info "using image: $DRIVE9_CSI_IMAGE"
info "using driver namespace: $driver_namespace"
info "using e2e namespace: $test_namespace"
if [[ "$DRIVE9_REMOTE_ROOT_PREFIX" == "" ]]; then
	info "using Drive9 workspace root mode"
else
	info "using managed Drive9 remote root prefix: $DRIVE9_REMOTE_ROOT_PREFIX"
fi

cp "$repo_root/deploy/kubernetes/csidriver.yaml" \
	"$manifest_dir/csidriver.yaml" || fail "copy csidriver"
write_namespace
write_rbac
copy_manifest controller.yaml controller.yaml
copy_manifest node.yaml node.yaml
write_storageclass
write_test_workload
write_test_pod drive9-csi-e2e-write "$tmp_dir/pod-write.yaml"
write_test_pod drive9-csi-e2e-read "$tmp_dir/pod-read.yaml"
write_test_pod drive9-csi-e2e-recreate-read "$tmp_dir/pod-recreate-read.yaml"

kubectl apply -f "$manifest_dir" || fail "apply CSI manifests"
kubectl -n "$driver_namespace" rollout status deployment/drive9-csi-controller \
	--timeout=300s || fail "controller rollout"
kubectl -n "$driver_namespace" rollout status daemonset/drive9-csi-node \
	--timeout=300s || fail "node rollout"

kubectl apply -f "$tmp_dir/workload.yaml" || fail "apply test workload"
kubectl apply -f "$tmp_dir/pod-write.yaml" || fail "apply write pod"
kubectl -n "$test_namespace" wait pod/drive9-csi-e2e-write \
	--for=condition=Ready --timeout=300s || fail "write pod ready"

e2e_token="drive9-csi-e2e-$(date +%s)"
e2e_file=".drive9-csi-e2e-$(date +%s).txt"
kubectl -n "$test_namespace" exec drive9-csi-e2e-write -- \
	sh -c "printf '%s\n' '$e2e_token' > '/workspace/$e2e_file' && sync" ||
	fail "write through mounted volume"
kubectl -n "$test_namespace" exec drive9-csi-e2e-write -- \
	sh -c "test \"\$(cat '/workspace/$e2e_file')\" = '$e2e_token'" ||
	fail "read through mounted volume"

pv_name="$(kubectl -n "$test_namespace" get pvc drive9-workspace-e2e \
	-o jsonpath='{.spec.volumeName}')" || fail "read bound PV name"
[[ "$pv_name" != "" ]] || fail "PVC did not bind a PV"

kubectl -n "$test_namespace" delete pod drive9-csi-e2e-write \
	--wait=true || fail "delete write pod"
kubectl apply -f "$tmp_dir/pod-read.yaml" || fail "apply read pod"
kubectl -n "$test_namespace" wait pod/drive9-csi-e2e-read \
	--for=condition=Ready --timeout=300s || fail "read pod ready"
kubectl -n "$test_namespace" exec drive9-csi-e2e-read -- \
	sh -c "test \"\$(cat '/workspace/$e2e_file')\" = '$e2e_token'" ||
	fail "read after pod remount"

# --- Multi-pod same-node concurrent test ---
# Pod1 (read pod) is still running. Get the node it landed on, then launch
# Pod2 pinned to the same node so both pods mount the same PVC concurrently.
pod1_node="$(kubectl -n "$test_namespace" get pod drive9-csi-e2e-read \
	-o jsonpath='{.spec.nodeName}')" || fail "read pod1 node name"
[[ "$pod1_node" != "" ]] || fail "pod1 has no node assignment"
info "multi-pod test: pod1 on node $pod1_node"

multi_token="drive9-csi-multi-$(date +%s)"
multi_file=".drive9-csi-multi-$(date +%s).txt"

write_test_pod_on_node drive9-csi-e2e-multi "$tmp_dir/pod-multi.yaml" "$pod1_node"
kubectl apply -f "$tmp_dir/pod-multi.yaml" || fail "apply multi pod"
kubectl -n "$test_namespace" wait pod/drive9-csi-e2e-multi \
	--for=condition=Ready --timeout=300s || fail "multi pod ready"

# Pod1 writes a file, Pod2 reads it.
kubectl -n "$test_namespace" exec drive9-csi-e2e-read -- \
	sh -c "printf '%s\n' '$multi_token' > '/workspace/$multi_file' && sync" ||
	fail "pod1 write for multi-pod test"
kubectl -n "$test_namespace" exec drive9-csi-e2e-multi -- \
	sh -c "test \"\$(cat '/workspace/$multi_file')\" = '$multi_token'" ||
	fail "pod2 read file written by pod1"

# Delete Pod1, verify Pod2 still works (unstage must not fire while Pod2
# still has a publish reference).
kubectl -n "$test_namespace" delete pod drive9-csi-e2e-read \
	--wait=true || fail "delete pod1 while pod2 still running"

second_token="drive9-csi-multi2-$(date +%s)"
kubectl -n "$test_namespace" exec drive9-csi-e2e-multi -- \
	sh -c "printf '%s\n' '$second_token' > '/workspace/$multi_file' && sync" ||
	fail "pod2 write after pod1 deletion"
kubectl -n "$test_namespace" exec drive9-csi-e2e-multi -- \
	sh -c "test \"\$(cat '/workspace/$multi_file')\" = '$second_token'" ||
	fail "pod2 read-back after pod1 deletion"

# Clean up multi-pod test files and pod.
kubectl -n "$test_namespace" exec drive9-csi-e2e-multi -- \
	sh -c "rm -f '/workspace/$multi_file' && sync" ||
	fail "remove multi-pod e2e file"
kubectl -n "$test_namespace" delete pod drive9-csi-e2e-multi \
	--wait=true || fail "delete multi pod"
info "passed: multi-pod same-node concurrent mount"
# --- End multi-pod test ---

# --- One-pod multi-PVC test ---
# One pod mounts two different PVCs backed by two different Secrets
# (same API key, same workspace root). This validates that the CSI
# driver handles multiple independent volumes in a single pod.
info "starting one-pod multi-PVC test"

write_second_pvc
kubectl apply -f "$tmp_dir/workload-b.yaml" || fail "apply second PVC"
write_multi_pvc_pod drive9-csi-e2e-multi-pvc "$tmp_dir/pod-multi-pvc.yaml"
kubectl apply -f "$tmp_dir/pod-multi-pvc.yaml" || fail "apply multi-PVC pod"
kubectl -n "$test_namespace" wait pod/drive9-csi-e2e-multi-pvc \
	--for=condition=Ready --timeout=300s || fail "multi-PVC pod ready"

multi_pvc_token_a="drive9-csi-mpvc-a-$(date +%s)"
multi_pvc_token_b="drive9-csi-mpvc-b-$(date +%s)"
multi_pvc_file_a=".drive9-csi-mpvc-a-$(date +%s).txt"
multi_pvc_file_b=".drive9-csi-mpvc-b-$(date +%s).txt"

# Write through PVC-A, read through PVC-B (same workspace root).
kubectl -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
	sh -c "printf '%s\n' '$multi_pvc_token_a' > '/workspace-a/$multi_pvc_file_a' && sync" ||
	fail "multi-PVC: write to PVC-A"
kubectl -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
	sh -c "test \"\$(cat '/workspace-b/$multi_pvc_file_a')\" = '$multi_pvc_token_a'" ||
	fail "multi-PVC: read PVC-A file through PVC-B"

# Write through PVC-B, read through PVC-A.
kubectl -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
	sh -c "printf '%s\n' '$multi_pvc_token_b' > '/workspace-b/$multi_pvc_file_b' && sync" ||
	fail "multi-PVC: write to PVC-B"
kubectl -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
	sh -c "test \"\$(cat '/workspace-a/$multi_pvc_file_b')\" = '$multi_pvc_token_b'" ||
	fail "multi-PVC: read PVC-B file through PVC-A"

# Clean up multi-PVC test files and pod.
kubectl -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
	sh -c "rm -f '/workspace-a/$multi_pvc_file_a' '/workspace-a/$multi_pvc_file_b' && sync" ||
	fail "remove multi-PVC e2e files"
kubectl -n "$test_namespace" delete pod drive9-csi-e2e-multi-pvc \
	--wait=true || fail "delete multi-PVC pod"

pv_name_b="$(kubectl -n "$test_namespace" get pvc drive9-workspace-e2e-b \
	-o jsonpath='{.spec.volumeName}')" || fail "read second PV name"
[[ "$pv_name_b" != "" ]] || fail "second PVC did not bind a PV"
kubectl -n "$test_namespace" delete pvc drive9-workspace-e2e-b \
	--wait=true || fail "delete second PVC"
wait_for_pv_deleted "$pv_name_b"
info "passed: one-pod multi-PVC mount"
# --- End multi-PVC test ---

if [[ "$DRIVE9_REMOTE_ROOT_PREFIX" == "" ]]; then
	kubectl -n "$test_namespace" delete pvc drive9-workspace-e2e \
		--wait=true || fail "delete test PVC before recreate"
	wait_for_pv_deleted "$pv_name"

	kubectl apply -f "$tmp_dir/workload.yaml" || fail "recreate test PVC"
	kubectl apply -f "$tmp_dir/pod-recreate-read.yaml" ||
		fail "apply recreate read pod"
	kubectl -n "$test_namespace" wait pod/drive9-csi-e2e-recreate-read \
		--for=condition=Ready --timeout=300s || fail "recreate read pod ready"
	kubectl -n "$test_namespace" exec drive9-csi-e2e-recreate-read -- \
		sh -c "test \"\$(cat '/workspace/$e2e_file')\" = '$e2e_token'" ||
		fail "read after PVC recreate"
	kubectl -n "$test_namespace" exec drive9-csi-e2e-recreate-read -- \
		sh -c "rm -f '/workspace/$e2e_file' && sync" ||
		fail "remove e2e file from workspace root"

	pv_name="$(kubectl -n "$test_namespace" get pvc drive9-workspace-e2e \
		-o jsonpath='{.spec.volumeName}')" || fail "read recreated bound PV name"
	[[ "$pv_name" != "" ]] || fail "recreated PVC did not bind a PV"

	kubectl -n "$test_namespace" delete pod drive9-csi-e2e-recreate-read \
		--wait=true || fail "delete recreate read pod"
fi

kubectl -n "$test_namespace" delete pvc drive9-workspace-e2e \
	--wait=true || fail "delete test PVC"
wait_for_pv_deleted "$pv_name"

info "passed: mount/write/read/remount/multi-pod/multi-pvc/unpublish/unstage/delete"
