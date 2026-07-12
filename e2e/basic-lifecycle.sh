#!/usr/bin/env bash

script_dir="$(cd "${0%/*}" && pwd)" || exit 1
source "$script_dir/lib/common.sh" || exit 1
source "$script_dir/lib/manifests.sh" || exit 1

cleanup() {
	local exit_code="$?"
	local cleanup_failed=0
	local pod_name
	local pod_names=(
		drive9-csi-e2e-write
		drive9-csi-e2e-read
		drive9-csi-e2e-multi
		drive9-csi-e2e-multi-pvc
		drive9-csi-e2e-recreate-read
	)

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
		e2e_delete_owned_pvc "pvc/drive9-workspace-e2e-b" \
			"$test_namespace" drive9-workspace-e2e-b "$case_run_id" ||
			cleanup_failed=1
		e2e_delete_owned_pvc "pvc/drive9-workspace-e2e" \
			"$test_namespace" drive9-workspace-e2e "$case_run_id" ||
			cleanup_failed=1
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

repo_root="$(cd "$script_dir/.." && pwd)" || exit 1
tmp_dir=""
manifest_dir=""
case_resources_registered=0
storage_class_created=0
volume_attributes_class_created=0
case_run_id="drive9-basic-$(date +%s)-$$"
driver_namespace="${DRIVE9_CSI_E2E_DRIVER_NAMESPACE:-}"
test_namespace=""
secret_name=""
storage_class="${DRIVE9_CSI_E2E_STORAGE_CLASS:-drive9-rwo-e2e}"
volume_attributes_class="${DRIVE9_CSI_E2E_VOLUME_ATTRIBUTES_CLASS:-}"
if [[ -z "$volume_attributes_class" ]]; then
	volume_attributes_class="drive9-coding-agent-e2e"
fi

e2e_init
e2e_configure_case

DRIVE9_REMOTE_ROOT_PREFIX="${DRIVE9_REMOTE_ROOT_PREFIX:-}"
DRIVE9_PROFILE="${DRIVE9_PROFILE:-coding-agent}"
e2e_require_single_line \
	"DRIVE9_REMOTE_ROOT_PREFIX" "$DRIVE9_REMOTE_ROOT_PREFIX"
e2e_require_single_line "DRIVE9_PROFILE" "$DRIVE9_PROFILE"
e2e_require_dns_label "case run ID" "$case_run_id"

e2e_require_dns_subdomain "StorageClass" "$storage_class"
e2e_require_dns_subdomain \
	"VolumeAttributesClass" "$volume_attributes_class"

e2e_require_prepared_driver
e2e_require_case_environment
e2e_ensure_absent storageclass "$storage_class"
e2e_ensure_absent volumeattributesclass "$volume_attributes_class"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/drive9-csi-e2e.XXXXXX")" ||
	e2e_fail "create temporary directory"
manifest_dir="$tmp_dir/manifests"
mkdir -p "$manifest_dir" || e2e_fail "create manifest directory"
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

e2e_info "using VolumeAttributesClass: $volume_attributes_class"
if [[ -z "$DRIVE9_REMOTE_ROOT_PREFIX" ]]; then
	e2e_info "using Drive9 workspace root mode"
else
	e2e_info "using managed Drive9 root: $DRIVE9_REMOTE_ROOT_PREFIX"
fi

e2e_render_case_manifests
e2e_write_primary_workload
e2e_write_test_pod drive9-csi-e2e-write "$tmp_dir/pod-write.yaml"
e2e_write_test_pod drive9-csi-e2e-read "$tmp_dir/pod-read.yaml"
e2e_write_test_pod drive9-csi-e2e-recreate-read \
	"$tmp_dir/pod-recreate-read.yaml"
e2e_validate_case_manifests

storage_class_created=1
e2e_create_owned_resource "storageclass/$storage_class" \
	storageclass "$storage_class" "$case_run_id" \
	"$manifest_dir/storageclass.yaml" ||
	e2e_fail "create case StorageClass"
volume_attributes_class_created=1
e2e_create_owned_resource \
	"volumeattributesclass/$volume_attributes_class" \
	volumeattributesclass "$volume_attributes_class" "$case_run_id" \
	"$manifest_dir/volumeattributesclass.yaml" ||
	e2e_fail "create case VolumeAttributesClass"
case_resources_registered=1
e2e_create_owned_namespaced_resource \
	"pvc/drive9-workspace-e2e" "$test_namespace" \
	pvc drive9-workspace-e2e "$case_run_id" "$tmp_dir/workload.yaml" ||
	e2e_fail "create test PVC"
e2e_create_owned_namespaced_resource \
	"pod/drive9-csi-e2e-write" "$test_namespace" \
	pod drive9-csi-e2e-write "$case_run_id" "$tmp_dir/pod-write.yaml" ||
	e2e_fail "create write Pod"
kube_retry -n "$test_namespace" wait pod/drive9-csi-e2e-write \
	--for=condition=Ready --timeout=300s || e2e_fail "write pod ready"

e2e_token="drive9-csi-e2e-$(date +%s)"
e2e_file=".drive9-csi-e2e-$(date +%s).txt"
kube_retry -n "$test_namespace" exec drive9-csi-e2e-write -- \
	sh -c "printf '%s\n' '$e2e_token' > '/workspace/$e2e_file' && sync" ||
	e2e_fail "write through mounted volume"
kube_retry -n "$test_namespace" exec drive9-csi-e2e-write -- \
	sh -c "test \"\$(cat '/workspace/$e2e_file')\" = '$e2e_token'" ||
	e2e_fail "read through mounted volume"

pv_name="$(kube_retry -n "$test_namespace" get pvc drive9-workspace-e2e \
	-o jsonpath='{.spec.volumeName}')" || e2e_fail "read bound PV name"
[[ -n "$pv_name" ]] || e2e_fail "PVC did not bind a PV"

e2e_delete_owned_namespaced_resource "pod/drive9-csi-e2e-write" \
	"$test_namespace" pod drive9-csi-e2e-write "$case_run_id" ||
	e2e_fail "delete write pod"
e2e_create_owned_namespaced_resource \
	"pod/drive9-csi-e2e-read" "$test_namespace" \
	pod drive9-csi-e2e-read "$case_run_id" "$tmp_dir/pod-read.yaml" ||
	e2e_fail "create read Pod"
kube_retry -n "$test_namespace" wait pod/drive9-csi-e2e-read \
	--for=condition=Ready --timeout=300s || e2e_fail "read pod ready"
kube_retry -n "$test_namespace" exec drive9-csi-e2e-read -- \
	sh -c "test \"\$(cat '/workspace/$e2e_file')\" = '$e2e_token'" ||
	e2e_fail "read after pod remount"

# Keep the first reader running and mount the same PVC into a second Pod on the
# same node. Deleting the first Pod must not unstage the shared mount.
pod1_node="$(kube_retry -n "$test_namespace" get pod drive9-csi-e2e-read \
	-o jsonpath='{.spec.nodeName}')" || e2e_fail "read first Pod node"
[[ -n "$pod1_node" ]] || e2e_fail "first Pod has no node assignment"
e2e_info "multi-Pod test node: $pod1_node"

multi_token="drive9-csi-multi-$(date +%s)"
multi_file=".drive9-csi-multi-$(date +%s).txt"
e2e_write_test_pod_on_node drive9-csi-e2e-multi \
	"$tmp_dir/pod-multi.yaml" "$pod1_node"
e2e_create_owned_namespaced_resource \
	"pod/drive9-csi-e2e-multi" "$test_namespace" \
	pod drive9-csi-e2e-multi "$case_run_id" "$tmp_dir/pod-multi.yaml" ||
	e2e_fail "create second Pod"
kube_retry -n "$test_namespace" wait pod/drive9-csi-e2e-multi \
	--for=condition=Ready --timeout=300s || e2e_fail "second Pod ready"

kube_retry -n "$test_namespace" exec drive9-csi-e2e-read -- \
	sh -c "printf '%s\n' '$multi_token' > '/workspace/$multi_file' && sync" ||
	e2e_fail "first Pod write for multi-Pod test"
kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi -- \
	sh -c "test \"\$(cat '/workspace/$multi_file')\" = '$multi_token'" ||
	e2e_fail "second Pod read file written by first Pod"

e2e_delete_owned_namespaced_resource "pod/drive9-csi-e2e-read" \
	"$test_namespace" pod drive9-csi-e2e-read "$case_run_id" ||
	e2e_fail "delete first Pod while second Pod is running"
second_token="drive9-csi-multi2-$(date +%s)"
kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi -- \
	sh -c "printf '%s\n' '$second_token' > '/workspace/$multi_file' && sync" ||
	e2e_fail "second Pod write after first Pod deletion"
kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi -- \
	sh -c "test \"\$(cat '/workspace/$multi_file')\" = '$second_token'" ||
	e2e_fail "second Pod read after first Pod deletion"
kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi -- \
	sh -c "rm -f '/workspace/$multi_file' && sync" ||
	e2e_fail "remove multi-Pod test file"
if [[ -n "$DRIVE9_REMOTE_ROOT_PREFIX" ]]; then
	kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi -- \
		sh -c "rm -f '/workspace/$e2e_file' && sync" ||
		e2e_fail "remove managed-directory lifecycle test file"
fi
e2e_delete_owned_namespaced_resource "pod/drive9-csi-e2e-multi" \
	"$test_namespace" pod drive9-csi-e2e-multi "$case_run_id" ||
	e2e_fail "delete second Pod"
e2e_info "passed: multi-Pod same-node concurrent mount"

# Mount two PVCs in one Pod. Workspace-root mode shares one root; managed mode
# must keep each generated remote root isolated.
e2e_info "starting one-Pod multi-PVC test"
e2e_write_second_pvc
e2e_create_owned_namespaced_resource \
	"pvc/drive9-workspace-e2e-b" "$test_namespace" \
	pvc drive9-workspace-e2e-b "$case_run_id" \
	"$tmp_dir/workload-b.yaml" || e2e_fail "create second PVC"
e2e_write_multi_pvc_pod drive9-csi-e2e-multi-pvc \
	"$tmp_dir/pod-multi-pvc.yaml"
e2e_create_owned_namespaced_resource \
	"pod/drive9-csi-e2e-multi-pvc" "$test_namespace" \
	pod drive9-csi-e2e-multi-pvc "$case_run_id" \
	"$tmp_dir/pod-multi-pvc.yaml" || e2e_fail "create multi-PVC Pod"
kube_retry -n "$test_namespace" wait pod/drive9-csi-e2e-multi-pvc \
	--for=condition=Ready --timeout=300s || e2e_fail "multi-PVC Pod ready"

multi_pvc_token_a="drive9-csi-mpvc-a-$(date +%s)"
multi_pvc_token_b="drive9-csi-mpvc-b-$(date +%s)"
multi_pvc_file_a=".drive9-csi-mpvc-a-$(date +%s).txt"
multi_pvc_file_b=".drive9-csi-mpvc-b-$(date +%s).txt"
kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
	sh -c \
	"printf '%s\n' '$multi_pvc_token_a' > \
	'/workspace-a/$multi_pvc_file_a' && sync" ||
	e2e_fail "multi-PVC write to PVC-A"
kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
	sh -c \
	"test \"\$(cat '/workspace-a/$multi_pvc_file_a')\" = '$multi_pvc_token_a'" ||
	e2e_fail "multi-PVC read from PVC-A"
kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
	sh -c \
	"printf '%s\n' '$multi_pvc_token_b' > \
	'/workspace-b/$multi_pvc_file_b' && sync" ||
	e2e_fail "multi-PVC write to PVC-B"
kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
	sh -c \
	"test \"\$(cat '/workspace-b/$multi_pvc_file_b')\" = '$multi_pvc_token_b'" ||
	e2e_fail "multi-PVC read from PVC-B"

if [[ -z "$DRIVE9_REMOTE_ROOT_PREFIX" ]]; then
	kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
		sh -c \
		"test \"\$(cat '/workspace-b/$multi_pvc_file_a')\" = '$multi_pvc_token_a'" ||
		e2e_fail "PVC-A file is not visible through PVC-B"
	kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
		sh -c \
		"test \"\$(cat '/workspace-a/$multi_pvc_file_b')\" = '$multi_pvc_token_b'" ||
		e2e_fail "PVC-B file is not visible through PVC-A"
	kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
		sh -c \
		"rm -f '/workspace-a/$multi_pvc_file_a' \
		'/workspace-a/$multi_pvc_file_b' && sync" ||
		e2e_fail "remove multi-PVC files"
else
	kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
		sh -c "! test -f '/workspace-b/$multi_pvc_file_a'" ||
		e2e_fail "PVC-A file leaked into PVC-B"
	kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
		sh -c "! test -f '/workspace-a/$multi_pvc_file_b'" ||
		e2e_fail "PVC-B file leaked into PVC-A"
	kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
		sh -c "rm -f '/workspace-a/$multi_pvc_file_a' && sync" ||
		e2e_fail "remove PVC-A test file"
	kube_retry -n "$test_namespace" exec drive9-csi-e2e-multi-pvc -- \
		sh -c "rm -f '/workspace-b/$multi_pvc_file_b' && sync" ||
		e2e_fail "remove PVC-B test file"
fi

e2e_delete_owned_namespaced_resource "pod/drive9-csi-e2e-multi-pvc" \
	"$test_namespace" pod drive9-csi-e2e-multi-pvc "$case_run_id" ||
	e2e_fail "delete multi-PVC Pod"
pv_name_b="$(kube_retry -n "$test_namespace" get \
	pvc drive9-workspace-e2e-b \
	-o jsonpath='{.spec.volumeName}')" || e2e_fail "read second PV name"
[[ -n "$pv_name_b" ]] || e2e_fail "second PVC did not bind a PV"
e2e_delete_owned_pvc "pvc/drive9-workspace-e2e-b" \
	"$test_namespace" drive9-workspace-e2e-b "$case_run_id" "$pv_name_b" ||
	e2e_fail "delete second PVC"
e2e_info "passed: one-Pod multi-PVC mount"

if [[ -z "$DRIVE9_REMOTE_ROOT_PREFIX" ]]; then
	e2e_delete_owned_pvc "pvc/drive9-workspace-e2e" \
		"$test_namespace" drive9-workspace-e2e "$case_run_id" "$pv_name" ||
		e2e_fail "delete first PVC before recreation"
	e2e_create_owned_namespaced_resource \
		"pvc/drive9-workspace-e2e" "$test_namespace" \
		pvc drive9-workspace-e2e "$case_run_id" "$tmp_dir/workload.yaml" ||
		e2e_fail "recreate test PVC"
	e2e_create_owned_namespaced_resource \
		"pod/drive9-csi-e2e-recreate-read" "$test_namespace" \
		pod drive9-csi-e2e-recreate-read "$case_run_id" \
		"$tmp_dir/pod-recreate-read.yaml" ||
		e2e_fail "create recreated reader Pod"
	kube_retry -n "$test_namespace" wait pod/drive9-csi-e2e-recreate-read \
		--for=condition=Ready --timeout=300s ||
		e2e_fail "recreated reader Pod ready"
	kube_retry -n "$test_namespace" exec drive9-csi-e2e-recreate-read -- \
		sh -c "test \"\$(cat '/workspace/$e2e_file')\" = '$e2e_token'" ||
		e2e_fail "read after PVC recreation"
	kube_retry -n "$test_namespace" exec drive9-csi-e2e-recreate-read -- \
		sh -c "rm -f '/workspace/$e2e_file' && sync" ||
		e2e_fail "remove workspace-root test file"
	pv_name="$(kube_retry -n "$test_namespace" get \
		pvc drive9-workspace-e2e \
		-o jsonpath='{.spec.volumeName}')" ||
		e2e_fail "read recreated PV name"
	[[ -n "$pv_name" ]] || e2e_fail "recreated PVC did not bind a PV"
	e2e_delete_owned_namespaced_resource \
		"pod/drive9-csi-e2e-recreate-read" "$test_namespace" \
		pod drive9-csi-e2e-recreate-read "$case_run_id" ||
		e2e_fail "delete recreated reader Pod"
fi

e2e_delete_owned_pvc "pvc/drive9-workspace-e2e" \
	"$test_namespace" drive9-workspace-e2e "$case_run_id" "$pv_name" ||
	e2e_fail "delete test PVC"

e2e_info \
	"passed: mount/write/read/remount/multi-Pod/multi-PVC/unpublish/unstage/delete"
