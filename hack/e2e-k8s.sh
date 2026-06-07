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

write_secret() {
	cat > "$manifest_dir/secret.yaml" <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: drive9-csi-secret
  namespace: $driver_namespace
type: Opaque
stringData:
  server: |-
    $DRIVE9_SERVER
  apiKey: |-
    $DRIVE9_API_KEY
EOF
}

write_storageclass() {
	awk \
		-v storage_class="$storage_class" \
		-v namespace="$driver_namespace" \
		-v root_prefix="$DRIVE9_REMOTE_ROOT_PREFIX" \
		-v profile="$DRIVE9_PROFILE" '
		$0 == "  name: drive9-rwo" {
			print "  name: " storage_class
			next
		}
		$0 ~ /^  csi.storage.k8s.io\/.*-secret-namespace:/ {
			sub(/: .*/, ": " namespace)
			print
			next
		}
		$0 ~ /^  remoteRootPrefix:/ {
			print "  remoteRootPrefix: " root_prefix
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

DRIVE9_REMOTE_ROOT_PREFIX="${DRIVE9_REMOTE_ROOT_PREFIX:-/k8s/pvc-e2e}"
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

cp "$repo_root/deploy/kubernetes/csidriver.yaml" \
	"$manifest_dir/csidriver.yaml" || fail "copy csidriver"
write_namespace
write_rbac
copy_manifest controller.yaml controller.yaml
copy_manifest node.yaml node.yaml
write_secret
write_storageclass
write_test_workload
write_test_pod drive9-csi-e2e-write "$tmp_dir/pod-write.yaml"
write_test_pod drive9-csi-e2e-read "$tmp_dir/pod-read.yaml"

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
kubectl -n "$test_namespace" exec drive9-csi-e2e-write -- \
	sh -c "printf '%s\n' '$e2e_token' > /workspace/e2e.txt && sync" ||
	fail "write through mounted volume"
kubectl -n "$test_namespace" exec drive9-csi-e2e-write -- \
	sh -c "test \"\$(cat /workspace/e2e.txt)\" = '$e2e_token'" ||
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
	sh -c "test \"\$(cat /workspace/e2e.txt)\" = '$e2e_token'" ||
	fail "read after pod remount"
kubectl -n "$test_namespace" delete pod drive9-csi-e2e-read \
	--wait=true || fail "delete read pod"
kubectl -n "$test_namespace" delete pvc drive9-workspace-e2e \
	--wait=true || fail "delete test PVC"
wait_for_pv_deleted "$pv_name"

info "passed: mount/write/read/remount/unpublish/unstage/delete"
