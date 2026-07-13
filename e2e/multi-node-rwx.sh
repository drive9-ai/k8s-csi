#!/usr/bin/env bash

script_dir="$(cd "${0%/*}" && pwd)" || exit 1
source "$script_dir/lib/common.sh" || exit 1
source "$script_dir/lib/manifests.sh" || exit 1

cleanup() {
	local exit_code="$?"
	local cleanup_failed=0
	local pod_name
	local pod_names=(drive9-rwx-e2e-a drive9-rwx-e2e-b)

	trap - EXIT INT TERM
	if [[ "${DRIVE9_CSI_E2E_KEEP:-}" == "1" ]]; then
		e2e_info "keeping E2E resources because DRIVE9_CSI_E2E_KEEP=1"
		e2e_info "temporary directory: $tmp_dir"
		exit "$exit_code"
	fi

	if ((case_resources_registered != 0)); then
		for pod_name in "${pod_names[@]}"; do
			e2e_delete_owned_namespaced_resource "pod/$pod_name" \
				"$test_namespace" pod "$pod_name" "$case_run_id" ||
				cleanup_failed=1
		done
		e2e_delete_owned_pvc "pvc/drive9-rwx-e2e" \
			"$test_namespace" drive9-rwx-e2e "$case_run_id" \
			"$pv_name" || cleanup_failed=1
	fi
	if ((storage_class_created != 0)); then
		e2e_cleanup_owned_resource "storageclass/$storage_class" \
			storageclass "$storage_class" "$case_run_id" || cleanup_failed=1
	fi
	if ((volume_attributes_class_created != 0)); then
		e2e_cleanup_owned_resource \
			"volumeattributesclass/$volume_attributes_class" \
			volumeattributesclass "$volume_attributes_class" \
			"$case_run_id" || cleanup_failed=1
	fi
	if [[ -n "$tmp_dir" && -d "$tmp_dir" ]]; then
		if ! rm -rf "$tmp_dir"; then
			e2e_info "cleanup failed: temporary directory $tmp_dir"
			cleanup_failed=1
		fi
	fi
	if ((exit_code == 0 && cleanup_failed != 0)); then
		exit_code=1
	fi

	exit "$exit_code"
}

write_file_value() {
	local pod_name="$1"
	local file_name="$2"
	local value="$3"
	local description="$4"

	kube_retry -n "$test_namespace" exec "$pod_name" -- sh -c '
		printf "%s\n" "$2" > "$1"
	' sh "/workspace/$file_name" "$value" || e2e_fail "$description"
}

wait_for_file_value() {
	local pod_name="$1"
	local file_name="$2"
	local value="$3"
	local description="$4"
	local attempt

	for attempt in {1..60}; do
		if kube -n "$test_namespace" exec "$pod_name" -- sh -c '
			test "$(cat "$1")" = "$2"
		' sh "/workspace/$file_name" "$value" >/dev/null 2>&1; then
			return 0
		fi
		sleep 5
	done

	e2e_fail "$description"
}

repo_root="$(cd "$script_dir/.." && pwd)" || exit 1
tmp_dir=""
manifest_dir=""
case_resources_registered=0
storage_class_created=0
volume_attributes_class_created=0
case_run_id="drive9-rwx-$(date +%s)-$$"
driver_namespace="${DRIVE9_CSI_E2E_DRIVER_NAMESPACE:-}"
test_namespace=""
secret_name=""
pv_name=""
storage_class="${DRIVE9_CSI_E2E_STORAGE_CLASS:-drive9-rwx-e2e}"
volume_attributes_class="${DRIVE9_CSI_E2E_VOLUME_ATTRIBUTES_CLASS:-}"
if [[ -z "$volume_attributes_class" ]]; then
	volume_attributes_class="drive9-rwx-e2e"
fi

e2e_init
e2e_configure_case

DRIVE9_REMOTE_ROOT_PREFIX="${DRIVE9_REMOTE_ROOT_PREFIX:-}"
DRIVE9_PROFILE="none"
case_durability="close-sync"
e2e_require_single_line \
	"DRIVE9_REMOTE_ROOT_PREFIX" "$DRIVE9_REMOTE_ROOT_PREFIX"
e2e_require_dns_label "case run ID" "$case_run_id"
e2e_require_dns_subdomain "StorageClass" "$storage_class"
e2e_require_dns_subdomain \
	"VolumeAttributesClass" "$volume_attributes_class"

e2e_require_prepared_driver
e2e_require_case_environment
e2e_ensure_absent storageclass "$storage_class"
e2e_ensure_absent volumeattributesclass "$volume_attributes_class"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/drive9-csi-rwx.XXXXXX")" ||
	e2e_fail "create temporary directory"
manifest_dir="$tmp_dir/manifests"
mkdir -p "$manifest_dir" || e2e_fail "create manifest directory"
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

e2e_render_case_manifests "$case_durability"
e2e_write_rwx_workload "$tmp_dir/workload.yaml"
e2e_write_rwx_pod drive9-rwx-e2e-a "$tmp_dir/pod-a.yaml" rwx-a false
e2e_write_rwx_pod drive9-rwx-e2e-b "$tmp_dir/pod-b.yaml" rwx-b true
e2e_validate_case_manifests

storage_class_created=1
e2e_create_owned_resource "storageclass/$storage_class" \
	storageclass "$storage_class" "$case_run_id" \
	"$manifest_dir/storageclass.yaml" || e2e_fail "create case StorageClass"
volume_attributes_class_created=1
e2e_create_owned_resource \
	"volumeattributesclass/$volume_attributes_class" \
	volumeattributesclass "$volume_attributes_class" "$case_run_id" \
	"$manifest_dir/volumeattributesclass.yaml" ||
	e2e_fail "create case VolumeAttributesClass"
case_resources_registered=1
e2e_create_owned_namespaced_resource \
	"pvc/drive9-rwx-e2e" "$test_namespace" pvc drive9-rwx-e2e \
	"$case_run_id" "$tmp_dir/workload.yaml" || e2e_fail "create RWX PVC"

e2e_create_owned_namespaced_resource \
	"pod/drive9-rwx-e2e-a" "$test_namespace" pod drive9-rwx-e2e-a \
	"$case_run_id" "$tmp_dir/pod-a.yaml" || e2e_fail "create Pod A"
kube_retry -n "$test_namespace" wait pod/drive9-rwx-e2e-a \
	--for=condition=Ready --timeout=300s || e2e_fail "Pod A ready"

e2e_create_owned_namespaced_resource \
	"pod/drive9-rwx-e2e-b" "$test_namespace" pod drive9-rwx-e2e-b \
	"$case_run_id" "$tmp_dir/pod-b.yaml" || e2e_fail "create Pod B"
kube_retry -n "$test_namespace" wait pod/drive9-rwx-e2e-b \
	--for=condition=Ready --timeout=300s ||
	e2e_fail "Pod B ready on a distinct eligible node"

pod_a_node="$(kube_retry -n "$test_namespace" get pod drive9-rwx-e2e-a \
	-o jsonpath='{.spec.nodeName}')" || e2e_fail "read Pod A node"
pod_b_node="$(kube_retry -n "$test_namespace" get pod drive9-rwx-e2e-b \
	-o jsonpath='{.spec.nodeName}')" || e2e_fail "read Pod B node"
[[ -n "$pod_a_node" && -n "$pod_b_node" ]] ||
	e2e_fail "RWX Pods have no node assignment"
if [[ "$pod_a_node" == "$pod_b_node" ]]; then
	e2e_fail "Pods were scheduled on the same node"
fi
e2e_info "Pod A node: $pod_a_node"
e2e_info "Pod B node: $pod_b_node"

pv_name="$(kube_retry -n "$test_namespace" get pvc drive9-rwx-e2e \
	-o jsonpath='{.spec.volumeName}')" || e2e_fail "read bound RWX PV name"
[[ -n "$pv_name" ]] || e2e_fail "RWX PVC did not bind a PV"

file_a=".drive9-rwx-a-$case_run_id.txt"
file_b=".drive9-rwx-b-$case_run_id.txt"
file_survival=".drive9-rwx-survival-$case_run_id.txt"
value_a="rwxa-$case_run_id"
value_b="rwxb-$case_run_id"
value_survival="survival-$case_run_id"

write_file_value drive9-rwx-e2e-a "$file_a" "$value_a" \
	"write unique file in Pod A"
wait_for_file_value drive9-rwx-e2e-b "$file_a" "$value_a" \
	"Pod B did not observe Pod A's closed file within 300 seconds"
write_file_value drive9-rwx-e2e-b "$file_b" "$value_b" \
	"write unique file in Pod B"
wait_for_file_value drive9-rwx-e2e-a "$file_b" "$value_b" \
	"Pod A did not observe Pod B's closed file within 300 seconds"

e2e_delete_owned_namespaced_resource "pod/drive9-rwx-e2e-a" \
	"$test_namespace" pod drive9-rwx-e2e-a "$case_run_id" ||
	e2e_fail "delete Pod A"
kube_retry -n "$test_namespace" exec drive9-rwx-e2e-b -- sh -c '
	test "$(cat "$1")" = "$2"
' sh "/workspace/$file_a" "$value_a" ||
	e2e_fail "read after Pod A deletion"
write_file_value drive9-rwx-e2e-b "$file_survival" "$value_survival" \
	"write after Pod A deletion"
kube_retry -n "$test_namespace" exec drive9-rwx-e2e-b -- sh -c '
	test "$(cat "$1")" = "$2"
' sh "/workspace/$file_survival" "$value_survival" ||
	e2e_fail "read surviving Pod B write"
kube_retry -n "$test_namespace" exec drive9-rwx-e2e-b -- sh -c '
	rm -f "$1" "$2" "$3"
' sh "/workspace/$file_a" "/workspace/$file_b" \
	"/workspace/$file_survival" || e2e_fail "remove RWX test files"

e2e_delete_owned_namespaced_resource "pod/drive9-rwx-e2e-b" \
	"$test_namespace" pod drive9-rwx-e2e-b "$case_run_id" ||
	e2e_fail "delete Pod B"
e2e_delete_owned_pvc "pvc/drive9-rwx-e2e" \
	"$test_namespace" drive9-rwx-e2e "$case_run_id" "$pv_name" ||
	e2e_fail "delete RWX PVC"

e2e_info "passed: two-node RWX visibility and surviving-node I/O"
