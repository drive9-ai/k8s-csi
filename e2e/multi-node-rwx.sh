#!/usr/bin/env bash

script_dir="$(cd "${0%/*}" && pwd)" || exit 1
source "$script_dir/lib/common.sh" || exit 1
source "$script_dir/lib/manifests.sh" || exit 1

cleanup_case_resources() {
	local cleanup_failed=0
	local pod_name
	local pod_names=("$pod_a_name" "$pod_b_name")

	if ((case_resources_registered != 0)); then
		for pod_name in "${pod_names[@]}"; do
			e2e_delete_owned_namespaced_resource "pod/$pod_name" \
				"$test_namespace" pod "$pod_name" "$case_run_id" ||
					cleanup_failed=1
		done
		e2e_delete_owned_pvc "pvc/$pvc_name" \
			"$test_namespace" "$pvc_name" "$case_run_id" \
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
	if ((cleanup_failed == 0)); then
		case_resources_registered=0
		storage_class_created=0
		volume_attributes_class_created=0
		pv_name=""
	fi

	return "$cleanup_failed"
}

cleanup() {
	local exit_code="$?"
	local cleanup_failed=0

	trap - EXIT INT TERM
	if [[ "${DRIVE9_CSI_E2E_KEEP:-}" == "1" ]]; then
		e2e_info "keeping E2E resources because DRIVE9_CSI_E2E_KEEP=1"
		e2e_info "temporary directory: $tmp_dir"
		exit "$exit_code"
	fi

	cleanup_case_resources || cleanup_failed=1
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
	local direction="$5"
	local attempt
	local elapsed_seconds
	local started_at="$SECONDS"

	for attempt in {1..60}; do
		if kube -n "$test_namespace" exec "$pod_name" -- sh -c '
			test "$(cat "$1")" = "$2"
		' sh "/workspace/$file_name" "$value" >/dev/null 2>&1; then
			elapsed_seconds=$((SECONDS - started_at))
			e2e_info "$direction cross-node visibility latency: ${elapsed_seconds}s"
			return 0
		fi
		sleep 5
	done

	e2e_fail "$description"
}

configure_rwx_case() {
	local case_key="$1"
	local profile="$2"

	case_run_id="$root_run_id-$case_key"
	storage_class="$storage_class_prefix-$case_key"
	volume_attributes_class="$volume_attributes_class_prefix-$case_key"
	pvc_name="drive9-rwx-e2e-$case_key"
	pod_a_name="$pvc_name-a"
	pod_b_name="$pvc_name-b"
	DRIVE9_PROFILE="$profile"

	e2e_require_dns_label "case run ID" "$case_run_id"
	e2e_require_dns_subdomain "StorageClass" "$storage_class"
	e2e_require_dns_subdomain \
		"VolumeAttributesClass" "$volume_attributes_class"
	e2e_require_dns_subdomain "PVC" "$pvc_name"
	e2e_require_dns_subdomain "Pod A" "$pod_a_name"
	e2e_require_dns_subdomain "Pod B" "$pod_b_name"
}

run_rwx_case() {
	local case_key="$1"
	local profile="$2"
	local durability="$3"
	local durability_label="${durability:-unset}"
	local case_dir
	local file_a
	local file_b
	local file_survival
	local pod_a_node
	local pod_b_node
	local value_a
	local value_b
	local value_survival

	configure_rwx_case "$case_key" "$profile"
	case_dir="$tmp_dir/$case_key"
	manifest_dir="$case_dir/manifests"
	pv_name=""
	case_resources_registered=0
	storage_class_created=0
	volume_attributes_class_created=0
	mkdir -p "$manifest_dir" || e2e_fail "create $case_key manifest directory"

	e2e_info "running RWX subcase: $case_key"
	e2e_info "mount parameters: profile=$profile durability=$durability_label"
	e2e_render_case_manifests "$durability"
	e2e_write_rwx_workload "$case_dir/workload.yaml" "$pvc_name"
	e2e_write_rwx_pod \
		"$pod_a_name" "$case_dir/pod-a.yaml" rwx-a false "$pvc_name"
	e2e_write_rwx_pod \
		"$pod_b_name" "$case_dir/pod-b.yaml" rwx-b true "$pvc_name"
	e2e_validate_case_manifests

	storage_class_created=1
	e2e_create_owned_resource "storageclass/$storage_class" \
		storageclass "$storage_class" "$case_run_id" \
		"$manifest_dir/storageclass.yaml" ||
		e2e_fail "create $case_key StorageClass"
	volume_attributes_class_created=1
	e2e_create_owned_resource \
		"volumeattributesclass/$volume_attributes_class" \
		volumeattributesclass "$volume_attributes_class" "$case_run_id" \
		"$manifest_dir/volumeattributesclass.yaml" ||
		e2e_fail "create $case_key VolumeAttributesClass"
	case_resources_registered=1
	e2e_create_owned_namespaced_resource \
		"pvc/$pvc_name" "$test_namespace" pvc "$pvc_name" \
		"$case_run_id" "$case_dir/workload.yaml" ||
		e2e_fail "create $case_key RWX PVC"

	e2e_create_owned_namespaced_resource \
		"pod/$pod_a_name" "$test_namespace" pod "$pod_a_name" \
		"$case_run_id" "$case_dir/pod-a.yaml" ||
		e2e_fail "create $case_key Pod A"
	kube_retry -n "$test_namespace" wait "pod/$pod_a_name" \
		--for=condition=Ready --timeout=300s ||
		e2e_fail "$case_key Pod A ready"

	e2e_create_owned_namespaced_resource \
		"pod/$pod_b_name" "$test_namespace" pod "$pod_b_name" \
		"$case_run_id" "$case_dir/pod-b.yaml" ||
		e2e_fail "create $case_key Pod B"
	kube_retry -n "$test_namespace" wait "pod/$pod_b_name" \
		--for=condition=Ready --timeout=300s ||
		e2e_fail "$case_key Pod B ready on a distinct eligible node"

	pod_a_node="$(kube_retry -n "$test_namespace" get pod "$pod_a_name" \
		-o jsonpath='{.spec.nodeName}')" || e2e_fail "read $case_key Pod A node"
	pod_b_node="$(kube_retry -n "$test_namespace" get pod "$pod_b_name" \
		-o jsonpath='{.spec.nodeName}')" || e2e_fail "read $case_key Pod B node"
	[[ -n "$pod_a_node" && -n "$pod_b_node" ]] ||
		e2e_fail "$case_key RWX Pods have no node assignment"
	if [[ "$pod_a_node" == "$pod_b_node" ]]; then
		e2e_fail "$case_key Pods were scheduled on the same node"
	fi
	e2e_info "$case_key Pod A node: $pod_a_node"
	e2e_info "$case_key Pod B node: $pod_b_node"

	pv_name="$(kube_retry -n "$test_namespace" get pvc "$pvc_name" \
		-o jsonpath='{.spec.volumeName}')" ||
		e2e_fail "read $case_key RWX PV name"
	[[ -n "$pv_name" ]] || e2e_fail "$case_key RWX PVC did not bind a PV"

	file_a=".drive9-rwx-a-$case_run_id.txt"
	file_b=".drive9-rwx-b-$case_run_id.txt"
	file_survival=".drive9-rwx-survival-$case_run_id.txt"
	value_a="rwxa-$case_run_id"
	value_b="rwxb-$case_run_id"
	value_survival="survival-$case_run_id"

	write_file_value "$pod_a_name" "$file_a" "$value_a" \
		"write $case_key unique file in Pod A"
	wait_for_file_value "$pod_b_name" "$file_a" "$value_a" \
		"$case_key Pod B did not observe Pod A's file within 300 seconds" \
		"$case_key A-to-B"
	write_file_value "$pod_b_name" "$file_b" "$value_b" \
		"write $case_key unique file in Pod B"
	wait_for_file_value "$pod_a_name" "$file_b" "$value_b" \
		"$case_key Pod A did not observe Pod B's file within 300 seconds" \
		"$case_key B-to-A"

	e2e_delete_owned_namespaced_resource "pod/$pod_a_name" \
		"$test_namespace" pod "$pod_a_name" "$case_run_id" ||
		e2e_fail "delete $case_key Pod A"
	kube_retry -n "$test_namespace" exec "$pod_b_name" -- sh -c '
		test "$(cat "$1")" = "$2"
	' sh "/workspace/$file_a" "$value_a" ||
		e2e_fail "$case_key read after Pod A deletion"
	write_file_value "$pod_b_name" "$file_survival" "$value_survival" \
		"$case_key write after Pod A deletion"
	kube_retry -n "$test_namespace" exec "$pod_b_name" -- sh -c '
		test "$(cat "$1")" = "$2"
	' sh "/workspace/$file_survival" "$value_survival" ||
		e2e_fail "$case_key read surviving Pod B write"
	kube_retry -n "$test_namespace" exec "$pod_b_name" -- sh -c '
		rm -f "$1" "$2" "$3"
	' sh "/workspace/$file_a" "/workspace/$file_b" \
		"/workspace/$file_survival" ||
		e2e_fail "remove $case_key RWX test files"

	e2e_delete_owned_namespaced_resource "pod/$pod_b_name" \
		"$test_namespace" pod "$pod_b_name" "$case_run_id" ||
		e2e_fail "delete $case_key Pod B"
	e2e_delete_owned_pvc "pvc/$pvc_name" \
		"$test_namespace" "$pvc_name" "$case_run_id" "$pv_name" ||
		e2e_fail "delete $case_key RWX PVC"

	e2e_info "passed RWX subcase: $case_key"
	if [[ "${DRIVE9_CSI_E2E_KEEP:-}" != "1" ]]; then
		cleanup_case_resources || e2e_fail "clean up RWX subcase: $case_key"
	fi
}

repo_root="$(cd "$script_dir/.." && pwd)" || exit 1
tmp_dir=""
manifest_dir=""
case_resources_registered=0
storage_class_created=0
volume_attributes_class_created=0
root_run_id="drive9-rwx-$(date +%s)-$$"
case_run_id=""
driver_namespace="${DRIVE9_CSI_E2E_DRIVER_NAMESPACE:-}"
test_namespace=""
secret_name=""
pv_name=""
storage_class_prefix="${DRIVE9_CSI_E2E_STORAGE_CLASS:-drive9-rwx-e2e}"
volume_attributes_class_prefix="${DRIVE9_CSI_E2E_VOLUME_ATTRIBUTES_CLASS:-}"
if [[ -z "$volume_attributes_class_prefix" ]]; then
	volume_attributes_class_prefix="drive9-rwx-e2e"
fi
storage_class=""
volume_attributes_class=""
pvc_name=""
pod_a_name=""
pod_b_name=""

e2e_init
e2e_configure_case

DRIVE9_REMOTE_ROOT_PREFIX="${DRIVE9_REMOTE_ROOT_PREFIX:-}"
e2e_require_single_line \
	"DRIVE9_REMOTE_ROOT_PREFIX" "$DRIVE9_REMOTE_ROOT_PREFIX"
e2e_require_prepared_driver
e2e_require_case_environment

configure_rwx_case remote-sync none
e2e_ensure_absent storageclass "$storage_class"
e2e_ensure_absent volumeattributesclass "$volume_attributes_class"
configure_rwx_case coding-agent coding-agent
e2e_ensure_absent storageclass "$storage_class"
e2e_ensure_absent volumeattributesclass "$volume_attributes_class"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/drive9-csi-rwx.XXXXXX")" ||
	e2e_fail "create temporary directory"
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

run_rwx_case remote-sync none close-sync
run_rwx_case coding-agent coding-agent ""

e2e_info "passed: both two-node RWX parameter subcases"
