#!/usr/bin/env bash

script_dir="$(cd "${0%/*}" && pwd)" || exit 1
repo_root="$(cd "$script_dir/.." && pwd)" || exit 1
source "$repo_root/e2e/lib/common.sh" || exit 1
source "$repo_root/e2e/lib/manifests.sh" || exit 1

fail() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

read_counter() {
	local value

	value=$(<"$counter_file") || fail "read retry counter"
	[[ "$value" =~ ^[0-9]+$ ]] || fail "retry counter is invalid"
	printf '%s\n' "$value"
}

increment_counter() {
	local value

	value=$(read_counter) || exit 1
	value=$((value + 1))
	printf '%s\n' "$value" > "$counter_file" || exit 1
	printf '%s\n' "$value"
}

cleanup() {
	local exit_code="$?"

	trap - EXIT INT TERM
	rm -rf "$tmp_dir" || exit_code=1
	exit "$exit_code"
}

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/drive9-e2e-check.XXXXXX")" ||
	fail "create temporary directory"
counter_file="$tmp_dir/counter"
stdout_file="$tmp_dir/stdout"
stderr_file="$tmp_dir/stderr"
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Keep retry tests fast without changing the production retry schedule.
sleep() {
	:
}

printf '0\n' > "$counter_file" || fail "initialize retry counter"
kube() {
	local attempt

	attempt=$(increment_counter) || return 1
	if ((attempt < 3)); then
		printf 'partial-output-%s\n' "$attempt"
		printf 'Unable to connect to the server: unexpected EOF\n' >&2
		return 1
	fi
	printf 'ready\n'
}

if ! kube_retry get pods > "$stdout_file" 2> "$stderr_file"; then
	fail "transient Kubernetes errors were not retried"
fi
[[ "$(read_counter)" == "3" ]] || fail "unexpected transient retry count"
[[ "$(<"$stdout_file")" == "ready" ]] || fail "retry lost stdout"
grep -Fq 'retrying kubectl after transient transport error' \
	"$stderr_file" || fail "retry did not explain the transient error"

printf '0\n' > "$counter_file" || fail "reset retry counter"
kube() {
	local attempt

	attempt=$(increment_counter) || return 1
	case "$1" in
	create)
		if ((attempt == 1)); then
			printf 'Unable to connect to the server: unexpected EOF\n' >&2
			return 1
		fi
		printf 'Error from server (AlreadyExists): exists\n' >&2
		return 1
		;;
	get)
		printf 'fixture|drive9-test-run\n'
		;;
	*)
		return 2
		;;
	esac
}

if ! e2e_create_owned_resource fixture storageclass fixture \
	drive9-test-run "$tmp_dir/fixture.yaml" \
	> "$stdout_file" 2> "$stderr_file"; then
	fail "ambiguous owned create was not reconciled"
fi
[[ "$(read_counter)" == "3" ]] || fail "owned create retry count is wrong"
grep -Fq 'reconciled ambiguous create response' "$stderr_file" ||
	fail "owned create reconciliation was not reported"

printf '0\n' > "$counter_file" || fail "reset retry counter"
kube() {
	increment_counter >/dev/null || return 1
	printf 'Error from server (Forbidden): denied\n' >&2
	return 42
}

kube_retry get pods > "$stdout_file" 2> "$stderr_file"
status="$?"
[[ "$status" == "42" ]] || fail "semantic Kubernetes status was changed"
[[ "$(read_counter)" == "1" ]] || fail "semantic error was retried"

printf '0\n' > "$counter_file" || fail "reset retry counter"
kube() {
	local attempt

	attempt=$(increment_counter) || return 1
	printf 'partial-output-%s\n' "$attempt"
	printf 'net/http: TLS handshake timeout\n' >&2
	return 17
}

kube_retry get pods > "$stdout_file" 2> "$stderr_file"
status="$?"
[[ "$status" == "17" ]] || fail "exhausted retry status was changed"
[[ "$(read_counter)" == "4" ]] || fail "retry limit is not four attempts"
[[ ! -s "$stdout_file" ]] || fail "failed retry leaked partial stdout"

printf 'net/http: TLS handshake timeout\n' > "$stderr_file" ||
	fail "write transient error fixture"
e2e_kube_error_is_transient "$stderr_file" ||
	fail "TLS timeout was not classified as transient"
printf 'Error from server (Invalid): bad manifest\n' > "$stderr_file" ||
	fail "write semantic error fixture"
if e2e_kube_error_is_transient "$stderr_file"; then
	fail "semantic error was classified as transient"
fi

digest_image='ghcr.io/drive9-ai/drive9-csi@sha256:'
digest_image+='b4a60d483a236f4dfeca1ab1ed2a51422259551d9ce7ccf5aaf80a135ca35ef3'
trace_tag='drive9-a53e497-csi-d91bfe3'
trace_image="ghcr.io/drive9-ai/drive9-csi:$trace_tag"
e2e_require_validation_image "digest fixture" "$digest_image"
e2e_require_validation_image "trace tag fixture" "$trace_image"
for invalid_image in \
	ghcr.io/drive9-ai/drive9-csi:latest \
	ghcr.io/drive9-ai/drive9-csi:drive9-a53e497-csi-d91bfe \
	example.com/drive9-ai/drive9-csi:drive9-a53e497-csi-d91bfe3; do
	if (e2e_require_validation_image \
		"invalid fixture" "$invalid_image" >/dev/null 2>&1); then
		fail "invalid validation image was accepted: $invalid_image"
	fi
done

unset DRIVE9_CSI_IMAGE
e2e_configure_prepare_image --image-tag "$trace_tag"
[[ "$DRIVE9_CSI_IMAGE" == "$trace_image" ]] ||
	fail "--image-tag did not configure the expected image"

DRIVE9_CSI_IMAGE="$digest_image"
e2e_configure_prepare_image
[[ "$DRIVE9_CSI_IMAGE" == "$digest_image" ]] ||
	fail "prepare image environment fallback changed the image"

if (unset DRIVE9_CSI_IMAGE; e2e_configure_prepare_image \
	--image-tag >/dev/null 2>&1); then
	fail "--image-tag without a value was accepted"
fi
if (unset DRIVE9_CSI_IMAGE; e2e_configure_prepare_image \
	--image-ref "$trace_image" >/dev/null 2>&1); then
	fail "unknown prepare image argument was accepted"
fi
if (unset DRIVE9_CSI_IMAGE; e2e_configure_prepare_image \
	--image-tag "$trace_tag" extra >/dev/null 2>&1); then
	fail "extra prepare image argument was accepted"
fi
if (unset DRIVE9_CSI_IMAGE; e2e_configure_prepare_image \
	--image-tag "$trace_image" >/dev/null 2>&1); then
	fail "a full image reference was accepted as a raw image tag"
fi
if (DRIVE9_CSI_IMAGE="$digest_image"; e2e_configure_prepare_image \
	--image-tag "$trace_tag" >/dev/null 2>&1); then
	fail "conflicting prepare image sources were accepted"
fi

driver_manifest_dir="$tmp_dir/driver-manifests"
mkdir -p "$driver_manifest_dir" || fail "create Driver manifest directory"
manifest_dir="$driver_manifest_dir"
driver_namespace="drive9-csi"
DRIVE9_CSI_IMAGE="$trace_image"
e2e_render_driver_manifests
e2e_validate_driver_manifests

test_prepared_driver() {
	e2e_ensure_present() {
		:
	}
	e2e_ensure_namespaced_present() {
		:
	}
	e2e_require_driver_binding() {
		:
	}
	kube_retry() {
		local output_format

		if [[ "$3" == "rollout" ]]; then
			return 0
		fi
		output_format="${!#}"
		case "$output_format" in
		*.image\})
			printf '%s\n' "$trace_image"
			;;
		*)
			return 2
			;;
		esac
	}

	e2e_require_prepared_driver "$trace_image"
}

if ! (test_prepared_driver \
	> "$stdout_file" 2> "$stderr_file"); then
	fail "prepared Driver rejected a valid trace tag"
fi

driver_namespace="drive9-csi"
DRIVE9_CSI_E2E_SECRET_NAME="drive9-existing-secret"
unset DRIVE9_CSI_E2E_NAMESPACE
e2e_configure_case
[[ "$test_namespace" == "$driver_namespace" ]] ||
	fail "case namespace does not default to Driver namespace"
[[ "$secret_name" == "$DRIVE9_CSI_E2E_SECRET_NAME" ]] ||
	fail "case Secret name was not configured"

DRIVE9_CSI_E2E_NAMESPACE="drive9-csi-cases"
e2e_configure_case
[[ "$test_namespace" == "$DRIVE9_CSI_E2E_NAMESPACE" ]] ||
	fail "explicit case namespace was not honored"

test_namespace="drive9-csi"
secret_name="drive9-existing-secret"
printf '0\n' > "$counter_file" || fail "reset retry counter"
kube() {
	local attempt

	attempt=$(increment_counter) || return 1
	case "$attempt" in
	1)
		[[ "$1" == "get" && "$2" == "namespace" &&
			"$3" == "$test_namespace" ]] || return 2
		printf 'namespace/%s\n' "$test_namespace"
		;;
	2)
		[[ "$1" == "-n" && "$2" == "$test_namespace" &&
			"$3" == "get" && "$4" == "secret" &&
			"$5" == "$secret_name" ]] || return 2
		printf 'secret/%s\n' "$secret_name"
		;;
	*)
		return 2
		;;
	esac
}

if ! e2e_require_case_environment \
	> "$stdout_file" 2> "$stderr_file"; then
	fail "pre-provisioned case environment was rejected"
fi
[[ "$(read_counter)" == "2" ]] ||
	fail "case environment validation made unexpected requests"

printf '0\n' > "$counter_file" || fail "reset retry counter"
kube() {
	local attempt

	attempt=$(increment_counter) || return 1
	if ((attempt == 1)); then
		return 0
	fi
	printf 'fixture|drive9-test-run\n'
}

if ! e2e_create_owned_namespaced_resource fixture drive9-csi \
	pod fixture drive9-test-run "$tmp_dir/fixture.yaml"; then
	fail "owned namespaced resource was not created"
fi
[[ "$(read_counter)" == "2" ]] ||
	fail "owned namespaced create was not verified"

printf '0\n' > "$counter_file" || fail "reset retry counter"
kube() {
	increment_counter >/dev/null || return 1
	printf 'fixture|another-run\n'
}

if e2e_delete_owned_namespaced_resource fixture drive9-csi \
	pod fixture drive9-test-run > "$stdout_file" 2> "$stderr_file"; then
	fail "namespaced cleanup accepted an ownership mismatch"
fi
[[ "$(read_counter)" == "1" ]] ||
	fail "ownership mismatch attempted a delete"

printf '0\n' > "$counter_file" || fail "reset retry counter"
kube() {
	local attempt

	attempt=$(increment_counter) || return 1
	case "$attempt" in
	1)
		[[ "$1" == "-n" && "$2" == "drive9-csi" &&
			"$3" == "get" && "$4" == "pod" ]] || return 2
		printf 'fixture|drive9-test-run\n'
		;;
	2)
		[[ "$1" == "delete" && "$2" == "-n" &&
			"$3" == "drive9-csi" && "$4" == "pod" ]] || return 2
		;;
	*)
		return 2
		;;
	esac
}

if ! e2e_delete_owned_namespaced_resource fixture drive9-csi \
	pod fixture drive9-test-run; then
	fail "owned namespaced resource was not deleted"
fi
[[ "$(read_counter)" == "2" ]] ||
	fail "owned namespaced delete made unexpected requests"

printf '0\n' > "$counter_file" || fail "reset retry counter"
kube() {
	local attempt

	attempt=$(increment_counter) || return 1
	case "$attempt" in
	1)
		[[ "$1" == "-n" && "$2" == "drive9-csi" &&
			"$3" == "get" && "$4" == "pvc" ]] || return 2
		printf 'fixture|drive9-test-run|pv-fixture\n'
		;;
	2)
		[[ "$1" == "delete" && "$2" == "-n" &&
			"$3" == "drive9-csi" && "$4" == "pvc" ]] || return 2
		;;
	3)
		[[ "$1" == "get" && "$2" == "pv" &&
			"$3" == "pv-fixture" ]] || return 2
		;;
	*)
		return 2
		;;
	esac
}

if ! e2e_delete_owned_pvc fixture drive9-csi fixture \
	drive9-test-run pv-fixture; then
	fail "owned PVC and its PV were not deleted"
fi
[[ "$(read_counter)" == "3" ]] ||
	fail "owned PVC delete made unexpected requests"

manifest_dir="$tmp_dir/case-manifests"
mkdir -p "$manifest_dir" || fail "create manifest test directory"
test_namespace="drive9-csi"
secret_name="drive9-existing-secret"
case_run_id="drive9-test-run"
storage_class="drive9-rwo-test"
volume_attributes_class="drive9-coding-agent-test"
DRIVE9_REMOTE_ROOT_PREFIX=""
DRIVE9_PROFILE="coding-agent"
e2e_render_case_manifests
e2e_write_primary_workload
e2e_write_second_pvc
e2e_write_test_pod drive9-test-pod "$tmp_dir/pod.yaml"
e2e_write_test_pod drive9-test-readonly "$tmp_dir/pod-readonly.yaml" true
e2e_write_test_pod_on_node drive9-test-node-pod \
	"$tmp_dir/pod-node.yaml" node-a
e2e_write_multi_pvc_pod drive9-test-multi-pvc \
	"$tmp_dir/pod-multi.yaml"
e2e_validate_case_manifests

if grep -Eq 'kind: Secret|apiKey:|DRIVE9_API_KEY|DRIVE9_SERVER' \
	"$tmp_dir"/*.yaml; then
	fail "case manifests contain inline credentials or a Secret"
fi
for file in workload.yaml workload-b.yaml pod.yaml pod-readonly.yaml \
	pod-node.yaml pod-multi.yaml; do
	grep -Fq "drive9.ai/e2e-run: $case_run_id" "$tmp_dir/$file" ||
		fail "case manifest lacks ownership: $file"
done
grep -Fq 'command: ["/bin/sleep", "3600"]' "$tmp_dir/pod.yaml" ||
	fail "test Pod does not run sleep directly"
if grep -Fq 'command: ["/bin/sh", "-c", "sleep 3600"]' \
	"$tmp_dir/pod.yaml"; then
	fail "test Pod runs sleep through a shell"
fi
[[ "$(grep -Fc 'readOnly: false' "$tmp_dir/pod.yaml")" == "2" ]] ||
	fail "default test Pod is not explicitly writable"
if grep -Fq 'readOnly: true' "$tmp_dir/pod.yaml"; then
	fail "default test Pod unexpectedly renders readonly"
fi
[[ "$(grep -Fc 'readOnly: true' "$tmp_dir/pod-readonly.yaml")" == "2" ]] ||
	fail "readonly test Pod does not set both readonly fields"
if grep -Fq 'readOnly: false' "$tmp_dir/pod-readonly.yaml"; then
	fail "readonly test Pod renders a writable field"
fi
secret_refs=$(awk -v secret="$secret_name" '
	$0 == "    drive9.ai/secret-name: " secret { count += 1 }
	END { print count + 0 }
' "$tmp_dir/workload.yaml" "$tmp_dir/workload-b.yaml") ||
	fail "count case Secret references"
[[ "$secret_refs" == "2" ]] ||
	fail "PVCs do not share the configured Secret"
[[ ! -e "$tmp_dir/namespace.yaml" ]] ||
	fail "case rendered a namespace manifest"

check_rwx_manifests() {
	local case_key="$1"
	local profile="$2"
	local durability="$3"
	local case_dir="$tmp_dir/rwx-$case_key"
	local pvc_name="drive9-rwx-e2e-$case_key"
	local pod_a_name="$pvc_name-a"
	local pod_b_name="$pvc_name-b"

	manifest_dir="$case_dir/manifests"
	case_run_id="drive9-test-$case_key"
	storage_class="drive9-rwx-e2e-$case_key"
	volume_attributes_class="drive9-rwx-e2e-$case_key"
	DRIVE9_PROFILE="$profile"
	mkdir -p "$manifest_dir" || fail "create $case_key manifest directory"

	e2e_render_case_manifests "$durability"
	e2e_write_rwx_workload "$case_dir/workload.yaml" "$pvc_name"
	e2e_write_rwx_pod \
		"$pod_a_name" "$case_dir/pod-a.yaml" rwx-a false "$pvc_name"
	e2e_write_rwx_pod \
		"$pod_b_name" "$case_dir/pod-b.yaml" rwx-b true "$pvc_name"
	e2e_validate_case_manifests

	grep -Fq "    $profile" "$manifest_dir/volumeattributesclass.yaml" ||
		fail "$case_key VAC lost profile"
	if [[ -n "$durability" ]]; then
		grep -Fq '  durability: |-' \
			"$manifest_dir/volumeattributesclass.yaml" ||
			fail "$case_key VAC lost durability"
		grep -Fq "    $durability" \
			"$manifest_dir/volumeattributesclass.yaml" ||
			fail "$case_key VAC has wrong durability"
	elif grep -Fq '  durability:' \
		"$manifest_dir/volumeattributesclass.yaml"; then
		fail "$case_key VAC unexpectedly renders durability"
	fi
	grep -Fq "  name: $pvc_name" "$case_dir/workload.yaml" ||
		fail "$case_key workload lost PVC name"
	grep -Fq "        claimName: $pvc_name" "$case_dir/pod-a.yaml" ||
		fail "$case_key Pod A lost PVC claim"
	grep -Fq "        claimName: $pvc_name" "$case_dir/pod-b.yaml" ||
		fail "$case_key Pod B lost PVC claim"
}

check_rwx_manifests remote-sync none close-sync
check_rwx_manifests coding-agent coding-agent ""

stop_function="$tmp_dir/stop-io-loop.sh"
awk '
	/^stop_io_loop\(\) \{/ { capture = 1 }
	capture { print }
	capture && /^}$/ { exit }
' "$repo_root/e2e/mount-survival.sh" > "$stop_function" ||
	fail "extract stop_io_loop"
source "$stop_function" || fail "load stop_io_loop"

test_split_stop_io_loop() {
	kube_retry() {
		local attempt
		local remote_script="${8:-}"

		attempt=$(increment_counter) || return 1
		[[ "$1" == "-n" && "$2" == "$test_namespace" &&
			"$3" == "exec" && "$4" == "drive9-csi-survival" &&
			"$5" == "--" && "$6" == "sh" && "$7" == "-c" ]] ||
			return 80
		if [[ "$remote_script" == *"sleep 1"* ||
			"$remote_script" == *"while test"* ]]; then
			return 81
		fi
		case "$attempt" in
		1)
			[[ "$remote_script" == *"drive9-survival-stop"* ]] ||
				return 82
			;;
		2 | 3)
			[[ "$remote_script" == *"drive9-survival-stopped"* &&
				"$remote_script" == *"drive9-survival-failure"* &&
				"$remote_script" == *'printf "failed\n"'* &&
				"$remote_script" == *'printf "stopped\n"'* &&
				"$remote_script" == *'printf "running\n"'* ]] ||
				return 83
			if ((attempt == 2)); then
				printf 'running\n'
			else
				printf 'stopped\n'
			fi
			;;
		4)
			[[ "$remote_script" == *'rm -f "/workspace/$1"'* &&
				"$remote_script" != *"sync"* ]] ||
				return 84
			;;
		*)
			return 85
			;;
		esac
		return 0
	}

	stop_io_loop drive9-csi-survival survival.txt
}

printf '0\n' > "$counter_file" || fail "reset retry counter"
if ! (test_split_stop_io_loop > "$stdout_file" 2> "$stderr_file"); then
	command cat "$stderr_file" >&2
	fail "stop_io_loop did not use short idempotent exec calls"
fi
[[ "$(read_counter)" == "4" ]] ||
	fail "stop_io_loop made an unexpected number of exec calls"

test_stop_io_loop_exit_1() {
	kube_retry() {
		local attempt

		attempt=$(increment_counter) || return 1
		((attempt == 1)) && return 0
		return 1
	}

	stop_io_loop drive9-csi-survival survival.txt
}

printf '0\n' > "$counter_file" || fail "reset retry counter"
if (test_stop_io_loop_exit_1 > "$stdout_file" 2> "$stderr_file"); then
	fail "stop_io_loop accepted an exec exit 1"
fi
[[ "$(read_counter)" == "2" ]] ||
	fail "stop_io_loop did not fail immediately after exec exit 1"

test_stop_io_loop_exit_137() {
	kube_retry() {
		local attempt

		attempt=$(increment_counter) || return 1
		((attempt == 1)) && return 0
		return 137
	}

	stop_io_loop drive9-csi-survival survival.txt
}

printf '0\n' > "$counter_file" || fail "reset retry counter"
if (test_stop_io_loop_exit_137 > "$stdout_file" 2> "$stderr_file"); then
	fail "stop_io_loop accepted a remote exit 137"
fi
[[ "$(read_counter)" == "2" ]] ||
	fail "stop_io_loop retried or continued after a remote exit 137"

test_stop_io_loop_rejected_state() {
	local rejected_state="$1"

	kube_retry() {
		local attempt

		attempt=$(increment_counter) || return 1
		((attempt == 1)) && return 0
		printf '%s\n' "$rejected_state"
	}

	stop_io_loop drive9-csi-survival survival.txt
}

for rejected_state in failed unknown; do
	printf '0\n' > "$counter_file" || fail "reset retry counter"
	if (test_stop_io_loop_rejected_state "$rejected_state" \
		> "$stdout_file" 2> "$stderr_file"); then
		fail "stop_io_loop accepted state: $rejected_state"
	fi
	[[ "$(read_counter)" == "2" ]] ||
		fail "stop_io_loop continued after state: $rejected_state"
done

printf 'e2e common checks passed\n'
