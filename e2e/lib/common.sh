#!/usr/bin/env bash

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	printf 'error: e2e/lib/common.sh must be sourced\n' >&2
	exit 2
fi

e2e_fail() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

e2e_info() {
	printf 'e2e: %s\n' "$*" >&2
}

e2e_need_cmd() {
	local name="$1"

	command -v "$name" >/dev/null 2>&1 ||
		e2e_fail "missing command: $name"
}

e2e_need_env() {
	local name="$1"
	local value="${!name:-}"

	[[ -n "$value" ]] || e2e_fail "$name is required"
}

kube() {
	local arg

	for arg in "$@"; do
		[[ "$arg" == "--" ]] && break
		case "$arg" in
		--context | --context=* | --server | --server=* | --cluster | --cluster=*)
			e2e_fail "kube does not allow Kubernetes target overrides"
			;;
		esac
	done
	kubectl --context "$DRIVE9_CSI_E2E_CONTEXT" "$@"
}

e2e_kube_error_is_transient() {
	local error_file="$1"
	local pattern

	pattern='Unable to connect to the server'
	pattern+='|TLS handshake timeout'
	pattern+='|unexpected EOF'
	pattern+='|(^|[^[:alpha:]])EOF([^[:alpha:]]|$)'
	pattern+='|connection reset by peer'
	pattern+='|connection refused'
	pattern+='|error dialing backend'
	pattern+='|i/o timeout'
	pattern+='|context deadline exceeded'
	pattern+='|Client.Timeout exceeded while awaiting headers'
	pattern+='|request canceled while waiting for connection'
	pattern+='|http2: client connection lost'
	pattern+='|stream error'
	pattern+='|no such host'
	pattern+='|the server was unable to return a response in the time allotted'
	LC_ALL=C grep -Eiq "$pattern" "$error_file"
}

kube_retry() {
	local attempt
	local delay=1
	local error_file
	local max_attempts=4
	local output_file
	local retry_dir
	local status=1

	retry_dir=$(mktemp -d \
		"${TMPDIR:-/tmp}/drive9-csi-kube-retry.XXXXXX") ||
		e2e_fail "create kubectl retry directory"
	error_file="$retry_dir/stderr"
	output_file="$retry_dir/stdout"
	for ((attempt = 1; attempt <= max_attempts; attempt++)); do
		if ! : > "$error_file" || ! : > "$output_file"; then
			rm -rf "$retry_dir"
			e2e_fail "reset kubectl retry output files"
		fi
		kube "$@" > "$output_file" 2> "$error_file"
		status="$?"
		if [[ -s "$error_file" ]]; then
			command cat "$error_file" >&2
		fi
		if ((status == 0)); then
			command cat "$output_file"
			rm -rf "$retry_dir"
			return 0
		fi
		if ((attempt == max_attempts)) ||
			! e2e_kube_error_is_transient "$error_file"; then
			rm -rf "$retry_dir"
			return "$status"
		fi
		e2e_info \
			"retrying kubectl after transient transport error ($attempt/$max_attempts)"
		sleep "$delay"
		delay=$((delay * 2))
	done

	rm -rf "$retry_dir"
	return "$status"
}

e2e_require_validation_image() {
	local label="$1"
	local image="$2"
	local digest_pattern
	local trace_tag_pattern

	digest_pattern='^[A-Za-z0-9._:/-]+@sha256:[0-9a-f]{64}$'
	trace_tag_pattern='^ghcr\.io/drive9-ai/drive9-csi:'
	trace_tag_pattern+='drive9-[0-9a-f]{7}-csi-[0-9a-f]{7}$'
	if [[ ! "$image" =~ $digest_pattern &&
		! "$image" =~ $trace_tag_pattern ]]; then
		e2e_fail \
			"$label must be an @sha256 reference or a Drive9 CSI trace tag"
	fi
}

e2e_configure_prepare_image() {
	local image_tag
	local trace_tag_pattern

	if (($# == 0)); then
		e2e_need_env DRIVE9_CSI_IMAGE
		e2e_require_validation_image \
			"DRIVE9_CSI_IMAGE" "$DRIVE9_CSI_IMAGE"
		return
	fi
	if (($# != 2)) || [[ "$1" != "--image-tag" ]]; then
		e2e_fail \
			"usage: e2e/prepare.sh [--image-tag <drive9-sha7-csi-sha7>]"
	fi
	if [[ -n "${DRIVE9_CSI_IMAGE:-}" ]]; then
		e2e_fail \
			"DRIVE9_CSI_IMAGE cannot be combined with --image-tag"
	fi

	image_tag="$2"
	trace_tag_pattern='^drive9-[0-9a-f]{7}-csi-[0-9a-f]{7}$'
	if [[ ! "$image_tag" =~ $trace_tag_pattern ]]; then
		e2e_fail "--image-tag must match drive9-<sha7>-csi-<sha7>"
	fi

	DRIVE9_CSI_IMAGE="ghcr.io/drive9-ai/drive9-csi:$image_tag"
	e2e_require_validation_image "--image-tag" "$DRIVE9_CSI_IMAGE"
}

e2e_require_single_line() {
	local label="$1"
	local value="$2"

	if [[ "$value" == *$'\n'* || "$value" == *$'\r'* ]]; then
		e2e_fail "$label must be a single line"
	fi
}

e2e_require_dns_label() {
	local label="$1"
	local value="$2"

	if ((${#value} > 63)) ||
		[[ ! "$value" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; then
		e2e_fail "$label must be a valid DNS label"
	fi
}

e2e_require_dns_subdomain() {
	local label="$1"
	local value="$2"
	local pattern

	pattern='^[a-z0-9]([-a-z0-9]*[a-z0-9])?'
	pattern+='(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$'
	if ((${#value} > 253)) || [[ ! "$value" =~ $pattern ]]; then
		e2e_fail "$label must be a valid DNS subdomain"
	fi
}

e2e_require_namespace() {
	local label="$1"
	local value="$2"

	e2e_require_dns_label "$label" "$value"
	case "$value" in
	default | kube-system | kube-public | kube-node-lease)
		e2e_fail "$label cannot be a protected Kubernetes namespace"
		;;
	esac
}

e2e_configure_case() {
	test_namespace="${DRIVE9_CSI_E2E_NAMESPACE:-$driver_namespace}"
	secret_name="${DRIVE9_CSI_E2E_SECRET_NAME:-}"

	e2e_need_env DRIVE9_CSI_E2E_SECRET_NAME
	e2e_require_single_line \
		"DRIVE9_CSI_E2E_NAMESPACE" "$test_namespace"
	e2e_require_single_line \
		"DRIVE9_CSI_E2E_SECRET_NAME" "$secret_name"
	e2e_require_namespace "case namespace" "$test_namespace"
	e2e_require_dns_subdomain "case Secret name" "$secret_name"
}

e2e_init() {
	local context_lower
	local server

	e2e_need_cmd kubectl
	e2e_need_env DRIVE9_CSI_E2E_CONTEXT
	e2e_need_env DRIVE9_CSI_E2E_DRIVER_NAMESPACE
	e2e_require_single_line \
		"DRIVE9_CSI_E2E_CONTEXT" "$DRIVE9_CSI_E2E_CONTEXT"
	e2e_require_single_line \
		"DRIVE9_CSI_E2E_DRIVER_NAMESPACE" \
		"$DRIVE9_CSI_E2E_DRIVER_NAMESPACE"
	e2e_require_namespace \
		"Driver namespace" "$DRIVE9_CSI_E2E_DRIVER_NAMESPACE"

	if [[ "${DRIVE9_CSI_E2E_CONFIRM:-}" != "1" ]]; then
		e2e_fail "set DRIVE9_CSI_E2E_CONFIRM=1 to mutate the selected cluster"
	fi

	context_lower=$(printf '%s' "$DRIVE9_CSI_E2E_CONTEXT" |
		tr '[:upper:]' '[:lower:]') ||
		e2e_fail "normalize Kubernetes context"
	if [[ "$context_lower" =~ prod|production ]]; then
		e2e_fail "production-like Kubernetes contexts are forbidden"
	fi

	if ! server=$(kube config view --minify \
		-o jsonpath='{.clusters[0].cluster.server}'); then
		e2e_fail "resolve Kubernetes context $DRIVE9_CSI_E2E_CONTEXT"
	fi
	[[ -n "$server" ]] || e2e_fail "selected Kubernetes context has no server"

	umask 077
	e2e_info "using Kubernetes context: $DRIVE9_CSI_E2E_CONTEXT"
	e2e_info "using Kubernetes API server: $server"
	e2e_info "using Driver namespace: $DRIVE9_CSI_E2E_DRIVER_NAMESPACE"
}

e2e_ensure_absent() {
	local kind="$1"
	local name="$2"
	local existing

	if ! existing=$(kube_retry get "$kind" "$name" \
		--ignore-not-found -o name); then
		e2e_fail "verify that $kind/$name does not exist"
	fi
	if [[ -n "$existing" ]]; then
		e2e_fail "$kind/$name already exists; clean the prior case first"
	fi
}

e2e_ensure_present() {
	local kind="$1"
	local name="$2"
	local existing

	if ! existing=$(kube_retry get "$kind" "$name" \
		--ignore-not-found -o name); then
		e2e_fail "verify that $kind/$name exists"
	fi
	if [[ -z "$existing" ]]; then
		e2e_fail "$kind/$name is missing; run e2e/prepare.sh first"
	fi
}

e2e_ensure_namespaced_present() {
	local kind="$1"
	local name="$2"
	local existing

	if ! existing=$(kube_retry -n "$driver_namespace" get "$kind" "$name" \
		--ignore-not-found -o name); then
		e2e_fail "verify that $kind/$name exists in $driver_namespace"
	fi
	if [[ -z "$existing" ]]; then
		e2e_fail "$kind/$name is missing; run e2e/prepare.sh first"
	fi
}

e2e_require_case_environment() {
	local existing

	if ! existing=$(kube_retry get namespace "$test_namespace" \
		--ignore-not-found -o name); then
		e2e_fail "verify case namespace $test_namespace"
	fi
	if [[ -z "$existing" ]]; then
		e2e_fail "case namespace $test_namespace does not exist"
	fi
	if ! existing=$(kube_retry -n "$test_namespace" get \
		secret "$secret_name" --ignore-not-found -o name); then
		e2e_fail "verify pre-provisioned Secret $test_namespace/$secret_name"
	fi
	if [[ -z "$existing" ]]; then
		e2e_fail \
			"pre-provisioned Secret $test_namespace/$secret_name does not exist"
	fi

	e2e_info "using E2E namespace: $test_namespace"
	e2e_info "using pre-provisioned Secret: $test_namespace/$secret_name"
}

e2e_require_driver_binding() {
	local account="$1"
	local binding_data
	local jsonpath
	local want

	jsonpath='jsonpath={.roleRef.kind}{"|"}{.roleRef.name}{"|"}'
	jsonpath+='{.subjects[0].kind}{"|"}{.subjects[0].name}{"|"}'
	jsonpath+='{.subjects[0].namespace}'
	if ! binding_data=$(kube_retry get clusterrolebinding "$account" \
		-o "$jsonpath"); then
		e2e_fail "read prepared ClusterRoleBinding $account"
	fi
	want="ClusterRole|$account|ServiceAccount|$account|$driver_namespace"
	if [[ "$binding_data" != "$want" ]]; then
		e2e_fail "ClusterRoleBinding $account does not target Driver namespace"
	fi
}

e2e_require_prepared_driver() {
	local expected_image="${1:-}"
	local controller_jsonpath
	local controller_image
	local installer_jsonpath
	local installer_image
	local node_jsonpath
	local node_image

	controller_jsonpath='jsonpath={.spec.template.spec.containers'
	controller_jsonpath+='[?(@.name=="drive9-csi")].image}'
	node_jsonpath="$controller_jsonpath"
	installer_jsonpath='jsonpath={.spec.template.spec.initContainers'
	installer_jsonpath+='[?(@.name=="install-host-binaries")].image}'

	e2e_ensure_present namespace "$driver_namespace"
	e2e_ensure_present csidriver csi.drive9.ai
	e2e_ensure_present clusterrole drive9-csi-controller
	e2e_ensure_present clusterrole drive9-csi-node
	e2e_ensure_present clusterrolebinding drive9-csi-controller
	e2e_ensure_present clusterrolebinding drive9-csi-node
	e2e_ensure_namespaced_present serviceaccount drive9-csi-controller
	e2e_ensure_namespaced_present serviceaccount drive9-csi-node
	e2e_require_driver_binding drive9-csi-controller
	e2e_require_driver_binding drive9-csi-node
	kube_retry -n "$driver_namespace" rollout status \
		deployment/drive9-csi-controller --timeout=300s ||
		e2e_fail "prepared controller is not ready"
	kube_retry -n "$driver_namespace" rollout status \
		daemonset/drive9-csi-node --timeout=300s ||
		e2e_fail "prepared node DaemonSet is not ready"

	controller_image=$(kube_retry -n "$driver_namespace" get \
		deployment drive9-csi-controller -o \
		"$controller_jsonpath") ||
		e2e_fail "read prepared controller image"
	node_image=$(kube_retry -n "$driver_namespace" get \
		daemonset drive9-csi-node -o "$node_jsonpath") ||
		e2e_fail "read prepared node image"
	installer_image=$(kube_retry -n "$driver_namespace" get \
		daemonset drive9-csi-node -o \
		"$installer_jsonpath") ||
		e2e_fail "read prepared installer image"

	e2e_require_validation_image "prepared controller image" "$controller_image"
	e2e_require_validation_image "prepared node image" "$node_image"
	e2e_require_validation_image "prepared installer image" "$installer_image"
	if [[ "$controller_image" != "$node_image" ||
		"$controller_image" != "$installer_image" ]]; then
		e2e_fail "prepared Drive9 CSI containers use different images"
	fi
	if [[ -n "$expected_image" && "$controller_image" != "$expected_image" ]]; then
		e2e_fail "prepared Driver image does not match DRIVE9_CSI_IMAGE"
	fi

	e2e_info "prepared Driver is ready with image: $controller_image"
}

e2e_create_owned_resource() {
	local description="$1"
	local kind="$2"
	local name="$3"
	local run_id="$4"
	local manifest="$5"
	local identity
	local jsonpath

	if kube_retry create -f "$manifest"; then
		return 0
	fi

	jsonpath='jsonpath={.metadata.name}{"|"}'
	jsonpath+='{.metadata.labels.drive9\.ai/e2e-run}'
	if ! identity=$(kube_retry get "$kind" "$name" \
		--ignore-not-found -o "$jsonpath"); then
		e2e_info "create reconciliation failed: $description"
		return 1
	fi
	if [[ "$identity" != "$name|$run_id" ]]; then
		e2e_info "create failed without an owned resource: $description"
		return 1
	fi

	e2e_info "reconciled ambiguous create response: $description"
}

e2e_create_owned_namespaced_resource() {
	local description="$1"
	local namespace="$2"
	local kind="$3"
	local name="$4"
	local run_id="$5"
	local manifest="$6"
	local create_succeeded=0
	local identity
	local jsonpath

	if kube_retry -n "$namespace" create -f "$manifest"; then
		create_succeeded=1
	fi

	jsonpath='jsonpath={.metadata.name}{"|"}'
	jsonpath+='{.metadata.labels.drive9\.ai/e2e-run}'
	if ! identity=$(kube_retry -n "$namespace" get "$kind" "$name" \
		--ignore-not-found -o "$jsonpath"); then
		e2e_info "create reconciliation failed: $description"
		return 1
	fi
	if [[ "$identity" != "$name|$run_id" ]]; then
		e2e_info "create failed without an owned resource: $description"
		return 1
	fi
	if ((create_succeeded == 0)); then
		e2e_info "reconciled ambiguous create response: $description"
	fi
	return 0
}

e2e_cleanup_delete() {
	local description="$1"
	shift

	if ! kube_retry delete "$@" --ignore-not-found --wait=true \
		--timeout=300s >/dev/null; then
		e2e_info "cleanup failed: $description"
		return 1
	fi
}

e2e_delete_owned_namespaced_resource() {
	local description="$1"
	local namespace="$2"
	local kind="$3"
	local name="$4"
	local run_id="$5"
	local identity
	local jsonpath

	jsonpath='jsonpath={.metadata.name}{"|"}'
	jsonpath+='{.metadata.labels.drive9\.ai/e2e-run}'
	if ! identity=$(kube_retry -n "$namespace" get "$kind" "$name" \
		--ignore-not-found -o "$jsonpath"); then
		e2e_info "ownership check failed: $description"
		return 1
	fi
	if [[ -z "$identity" ]]; then
		return 0
	fi
	if [[ "$identity" != "$name|$run_id" ]]; then
		e2e_info "ownership mismatch: $description"
		return 1
	fi
	e2e_cleanup_delete "$description" -n "$namespace" "$kind" "$name"
}

e2e_delete_owned_pvc() {
	local description="$1"
	local namespace="$2"
	local name="$3"
	local run_id="$4"
	local expected_pv="${5:-}"
	local actual_name
	local actual_run_id
	local identity
	local jsonpath
	local pv_name

	jsonpath='jsonpath={.metadata.name}{"|"}'
	jsonpath+='{.metadata.labels.drive9\.ai/e2e-run}{"|"}'
	jsonpath+='{.spec.volumeName}'
	if ! identity=$(kube_retry -n "$namespace" get pvc "$name" \
		--ignore-not-found -o "$jsonpath"); then
		e2e_info "ownership check failed: $description"
		return 1
	fi
	if [[ -z "$identity" ]]; then
		return 0
	fi
	IFS='|' read -r actual_name actual_run_id pv_name <<< "$identity"
	if [[ "$actual_name" != "$name" || "$actual_run_id" != "$run_id" ]]; then
		e2e_info "ownership mismatch: $description"
		return 1
	fi
	if [[ -n "$expected_pv" && "$pv_name" != "$expected_pv" ]]; then
		e2e_info "PV identity mismatch: $description"
		return 1
	fi
	if ! e2e_cleanup_delete "$description" \
		-n "$namespace" pvc "$name"; then
		return 1
	fi
	if [[ -n "$pv_name" ]]; then
		e2e_wait_for_pv_deleted "$pv_name" || return 1
	fi
	return 0
}

e2e_cleanup_owned_resource() {
	local description="$1"
	local kind="$2"
	local name="$3"
	local run_id="$4"
	local identity
	local jsonpath

	jsonpath='jsonpath={.metadata.name}{"|"}'
	jsonpath+='{.metadata.labels.drive9\.ai/e2e-run}'
	if ! identity=$(kube_retry get "$kind" "$name" --ignore-not-found \
		-o "$jsonpath"); then
		e2e_info "cleanup ownership check failed: $description"
		return 1
	fi
	if [[ -z "$identity" ]]; then
		return 0
	fi
	if [[ "$identity" != "$name|$run_id" ]]; then
		e2e_info "cleanup ownership mismatch: $description"
		return 1
	fi
	e2e_cleanup_delete "$description" "$kind" "$name"
}

e2e_wait_for_pv_deleted() {
	local pv_name="$1"
	local attempt
	local existing
	local last_observation="PV still exists"

	for attempt in {1..60}; do
		if ! existing=$(kube get pv "$pv_name" \
			--ignore-not-found -o name 2>/dev/null); then
			last_observation="Kubernetes API query failed"
			sleep 5
			continue
		fi
		if [[ -z "$existing" ]]; then
			return 0
		fi
		last_observation="PV still exists"
		sleep 5
	done

	e2e_info "PV $pv_name deletion was not confirmed: $last_observation"
	return 1
}
