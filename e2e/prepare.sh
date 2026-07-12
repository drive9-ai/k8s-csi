#!/usr/bin/env bash

script_dir="$(cd "${0%/*}" && pwd)" || exit 1
source "$script_dir/lib/common.sh" || exit 1
source "$script_dir/lib/manifests.sh" || exit 1

cleanup() {
	local exit_code="$?"

	trap - EXIT INT TERM
	if [[ -n "$tmp_dir" && -d "$tmp_dir" ]]; then
		if ! rm -rf "$tmp_dir"; then
			e2e_info "cleanup failed: temporary directory $tmp_dir"
			((exit_code == 0)) && exit_code=1
		fi
	fi
	exit "$exit_code"
}

repo_root="$(cd "$script_dir/.." && pwd)" || exit 1
tmp_dir=""
manifest_dir=""
driver_namespace="${DRIVE9_CSI_E2E_DRIVER_NAMESPACE:-}"

e2e_init
e2e_need_env DRIVE9_CSI_IMAGE
e2e_require_immutable_image "DRIVE9_CSI_IMAGE" "$DRIVE9_CSI_IMAGE"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/drive9-csi-prepare.XXXXXX")" ||
	e2e_fail "create temporary directory"
manifest_dir="$tmp_dir/manifests"
mkdir -p "$manifest_dir" || e2e_fail "create manifest directory"
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

e2e_info "preparing Driver image: $DRIVE9_CSI_IMAGE"
e2e_render_driver_manifests
e2e_validate_driver_manifests

kube_retry apply -f "$manifest_dir/namespace.yaml" ||
	e2e_fail "apply Driver namespace"
kube_retry apply -f "$manifest_dir/csidriver.yaml" ||
	e2e_fail "apply CSIDriver"
kube_retry apply -f "$manifest_dir/rbac.yaml" ||
	e2e_fail "apply Driver RBAC"
kube_retry apply -f "$manifest_dir/controller.yaml" ||
	e2e_fail "apply controller"
kube_retry apply -f "$manifest_dir/node.yaml" ||
	e2e_fail "apply node DaemonSet"

e2e_require_prepared_driver "$DRIVE9_CSI_IMAGE"
e2e_info "prepared reusable Driver environment"
