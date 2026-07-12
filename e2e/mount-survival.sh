#!/usr/bin/env bash

script_dir="$(cd "${0%/*}" && pwd)" || exit 1
source "$script_dir/lib/common.sh" || exit 1
source "$script_dir/lib/manifests.sh" || exit 1

cleanup() {
	local exit_code="$?"
	local cleanup_failed=0

	trap - EXIT INT TERM
	if [[ "${DRIVE9_CSI_E2E_KEEP:-}" == "1" ]]; then
		e2e_info "keeping E2E resources because DRIVE9_CSI_E2E_KEEP=1"
		e2e_info "temporary directory: $tmp_dir"
		exit "$exit_code"
	fi

	if ((case_resources_registered != 0)); then
		e2e_delete_owned_namespaced_resource \
			"pod/drive9-csi-survival" "$test_namespace" \
			pod drive9-csi-survival "$case_run_id" || cleanup_failed=1
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

find_node_pod() {
	local node_name="$1"
	local pod_name

	if ! pod_name=$(kube_retry -n "$driver_namespace" get pods \
		-l app=drive9-csi-node \
		--field-selector "spec.nodeName=$node_name" \
		-o jsonpath='{.items[0].metadata.name}'); then
		e2e_fail "find CSI node Pod on $node_name"
	fi
	[[ -n "$pod_name" ]] || e2e_fail "CSI node Pod not found on $node_name"
	printf '%s\n' "$pod_name"
}

wait_for_replacement_node_pod() {
	local node_name="$1"
	local old_uid="$2"
	local attempt
	local candidate
	local pods

	for attempt in {1..60}; do
		if pods=$(kube -n "$driver_namespace" get pods \
			-l app=drive9-csi-node \
			--field-selector "spec.nodeName=$node_name" -o json); then
			candidate=$(printf '%s' "$pods" | jq -r --arg old "$old_uid" '
				[
					.items[]
					| select(.metadata.uid != $old)
					| select(any(.status.conditions[]?;
						.type == "Ready" and .status == "True"))
					| .metadata.name
				]
				| first // empty
			') || e2e_fail "parse replacement CSI node Pods"
			if [[ -n "$candidate" ]]; then
				printf '%s\n' "$candidate"
				return 0
			fi
		fi
		sleep 5
	done

	e2e_fail "replacement CSI node Pod did not become ready"
}

capture_mount_identity() {
	local node_pod="$1"
	local volume_id="$2"
	local output="$3"
	local actual_pid_start_time
	local binary_path
	local cgroup
	local executable
	local mount_id
	local pid
	local pid_start_time
	local process_stat
	local process_stat_tail
	local staging_target
	local state
	local state_path="/var/lib/drive9-csi/${volume_id}.json"
	local systemd_unit

	if ! state=$(kube_retry -n "$driver_namespace" exec "$node_pod" \
		-c drive9-csi -- cat "$state_path"); then
		e2e_fail "read mount state $state_path from $node_pod"
	fi
	if ! printf '%s' "$state" | jq -e --arg volumeID "$volume_id" '
		.schemaVersion == 2 and
		.phase == "active" and
		.volumeID == $volumeID and
		(.pid | type == "number" and . > 1) and
		(.pidStartTime | type == "string" and test("^[0-9]+$")) and
		(.systemdUnit | test("^drive9-mount-[0-9a-f]{16}\\.service$")) and
		(.binaryPath | test("^/var/lib/drive9-csi/bin/drive9-[0-9a-f]{64}$")) and
		(.stagingTarget | type == "string") and
		(.stagingTarget | startswith("/var/lib/kubelet/"))
	' >/dev/null; then
		e2e_fail "mount state is not a complete active identity"
	fi

	pid=$(printf '%s' "$state" | jq -r '.pid') ||
		e2e_fail "read mount PID"
	pid_start_time=$(printf '%s' "$state" | jq -r '.pidStartTime') ||
		e2e_fail "read mount PID start time"
	systemd_unit=$(printf '%s' "$state" | jq -r '.systemdUnit') ||
		e2e_fail "read mount systemd unit"
	binary_path=$(printf '%s' "$state" | jq -r '.binaryPath') ||
		e2e_fail "read mount binary path"
	staging_target=$(printf '%s' "$state" | jq -r '.stagingTarget') ||
		e2e_fail "read staging target"

	if ! executable=$(kube_retry -n "$driver_namespace" exec "$node_pod" \
		-c drive9-csi -- readlink "/host-proc/$pid/exe"); then
		e2e_fail "read host executable for mount PID $pid"
	fi
	if [[ "$executable" != "$binary_path" ]]; then
		e2e_fail "mount executable does not match recorded binary"
	fi
	if ! process_stat=$(kube_retry -n "$driver_namespace" exec "$node_pod" \
		-c drive9-csi -- cat "/host-proc/$pid/stat"); then
		e2e_fail "read host stat for mount PID $pid"
	fi
	process_stat_tail="${process_stat##*) }"
	actual_pid_start_time=$(printf '%s\n' "$process_stat_tail" |
		awk '{ print $20 }') || e2e_fail "parse host PID start time"
	if [[ ! "$actual_pid_start_time" =~ ^[0-9]+$ ]] ||
		[[ "$actual_pid_start_time" != "$pid_start_time" ]]; then
		e2e_fail "mount PID start time does not match recorded state"
	fi

	if ! cgroup=$(kube_retry -n "$driver_namespace" exec "$node_pod" \
		-c drive9-csi -- cat "/host-proc/$pid/cgroup"); then
		e2e_fail "read host cgroup for mount PID $pid"
	fi
	if [[ "$cgroup" != *"/system.slice/$systemd_unit"* ]]; then
		e2e_fail "mount PID is not owned by $systemd_unit"
	fi

	if ! mount_id=$(kube_retry -n "$driver_namespace" exec "$node_pod" \
		-c drive9-csi -- awk -v target="$staging_target" \
		'$5 == target { print $1 }' /host-proc/1/mountinfo); then
		e2e_fail "read host mount ID for $staging_target"
	fi
	if [[ ! "$mount_id" =~ ^[0-9]+$ ]]; then
		e2e_fail "staging target must resolve to exactly one host mount ID"
	fi

	jq -nS \
		--arg pid "$pid" \
		--arg pidStartTime "$pid_start_time" \
		--arg mountID "$mount_id" \
		--arg systemdUnit "$systemd_unit" \
		--arg binaryPath "$binary_path" \
		--arg stagingTarget "$staging_target" \
		'{
			pid: $pid,
			pidStartTime: $pidStartTime,
			mountID: $mountID,
			systemdUnit: $systemdUnit,
			binaryPath: $binaryPath,
			stagingTarget: $stagingTarget
		}' > "$output" || e2e_fail "write mount identity"
}

start_io_loop() {
	local pod_name="$1"
	local file_name="$2"

	kube -n "$test_namespace" exec "$pod_name" -- sh -c '
		file_name=$1
		rm -f /tmp/drive9-survival-count
		rm -f /tmp/drive9-survival-failure
		rm -f /tmp/drive9-survival-stop /tmp/drive9-survival-stopped
		(
			count=0
			while ! test -e /tmp/drive9-survival-stop; do
				count=$((count + 1))
				value="survival-${count}"
				if ! printf "%s\n" "$value" > "/workspace/${file_name}.tmp" ||
					! sync ||
					! mv "/workspace/${file_name}.tmp" "/workspace/$file_name" ||
					! test "$(cat "/workspace/$file_name")" = "$value"; then
					printf "I/O loop failed\n" > /tmp/drive9-survival-failure
					exit 1
				fi
				printf "%s\n" "$count" > /tmp/drive9-survival-count
				sleep 1
			done
			printf "stopped\n" > /tmp/drive9-survival-stopped
		) </dev/null >/tmp/drive9-survival.log 2>&1 &
	' sh "$file_name" || e2e_fail "start workload I/O loop"
}

read_io_count() {
	local pod_name="$1"
	local count

	count=$(kube_retry -n "$test_namespace" exec "$pod_name" -- \
		cat /tmp/drive9-survival-count 2>/dev/null) || return 1
	[[ "$count" =~ ^[0-9]+$ ]] || return 1
	printf '%s\n' "$count"
}

wait_for_io_progress() {
	local pod_name="$1"
	local minimum="$2"
	local attempt
	local count

	for attempt in {1..60}; do
		if kube_retry -n "$test_namespace" exec "$pod_name" -- \
			test -s /tmp/drive9-survival-failure >/dev/null 2>&1; then
			e2e_fail "workload I/O loop recorded a failure"
		fi
		if count=$(read_io_count "$pod_name") && ((count > minimum)); then
			printf '%s\n' "$count"
			return 0
		fi
		sleep 2
	done

	e2e_fail "workload I/O did not progress beyond $minimum"
}

stop_io_loop() {
	local attempt
	local pod_name="$1"
	local file_name="$2"
	local status
	local stopped=0

	if ! kube_retry -n "$test_namespace" exec "$pod_name" -- sh -c '
		if test -s /tmp/drive9-survival-failure; then
			cat /tmp/drive9-survival.log >&2
			exit 2
		fi
		: > /tmp/drive9-survival-stop
	'; then
		e2e_fail "request workload I/O loop stop"
	fi

	for attempt in {1..15}; do
		kube_retry -n "$test_namespace" exec "$pod_name" -- sh -c '
			if test -s /tmp/drive9-survival-failure; then
				cat /tmp/drive9-survival.log >&2
				exit 2
			fi
			test -s /tmp/drive9-survival-stopped
		'
		status="$?"
		case "$status" in
		0)
			stopped=1
			break
			;;
		1)
			sleep 1
			;;
		2)
			e2e_fail "workload I/O loop recorded a failure"
			;;
		*)
			e2e_fail "observe workload I/O loop stop"
			;;
		esac
	done
	if ((stopped == 0)); then
		if ! kube_retry -n "$test_namespace" exec "$pod_name" -- \
			cat /tmp/drive9-survival.log >&2; then
			e2e_info "could not read workload I/O loop log after stop timeout"
		fi
		e2e_fail "workload I/O loop did not stop cooperatively"
	fi
	e2e_info "workload I/O loop stopped cooperatively"

	if ! kube_retry -n "$test_namespace" exec "$pod_name" -- sh -c '
		rm -f "/workspace/$1" "/workspace/$1.tmp"
	' sh "$file_name"; then
		e2e_fail "clean workload I/O files"
	fi
}

wait_for_mount_cleanup() {
	local node_pod="$1"
	local volume_id="$2"
	local identity_file="$3"
	local attempt
	local binary_path
	local executable
	local mount_id
	local observation
	local pid
	local state_present
	local staging_target
	local state_path="/var/lib/drive9-csi/${volume_id}.json"
	local systemd_unit
	local unit_present

	pid=$(jq -r '.pid' "$identity_file") || e2e_fail "read cleanup PID"
	binary_path=$(jq -r '.binaryPath' "$identity_file") ||
		e2e_fail "read cleanup binary path"
	staging_target=$(jq -r '.stagingTarget' "$identity_file") ||
		e2e_fail "read cleanup staging target"
	systemd_unit=$(jq -r '.systemdUnit' "$identity_file") ||
		e2e_fail "read cleanup systemd unit"

	for attempt in {1..60}; do
		if ! observation=$(kube -n "$driver_namespace" exec "$node_pod" \
			-c drive9-csi -- sh -c '
				state_present=false
				unit_present=false
				test -e "$1" && state_present=true
				executable=$(readlink "$2" 2>/dev/null || true)
				mount_id=$(awk -v target="$3" \
					"\$5 == target { print \$1 }" /host-proc/1/mountinfo)
				test -e "$4" && unit_present=true
				printf "%s|%s|%s|%s\n" \
					"$state_present" "$executable" \
					"$mount_id" "$unit_present"
			' sh "$state_path" "/host-proc/$pid/exe" \
			"$staging_target" \
			"/host-proc/1/root/run/systemd/transient/$systemd_unit"); then
			sleep 5
			continue
		fi
		IFS='|' read -r state_present executable mount_id unit_present \
			<<< "$observation"
		if [[ "$state_present" == "true" ]]; then
			sleep 5
			continue
		fi
		if [[ "$executable" == "$binary_path" ]]; then
			sleep 5
			continue
		fi
		if [[ -n "$mount_id" ]]; then
			sleep 5
			continue
		fi
		if [[ "$unit_present" == "true" ]]; then
			sleep 5
			continue
		fi
		return 0
	done

	e2e_fail "mount resources were not fully cleaned"
}

repo_root="$(cd "$script_dir/.." && pwd)" || exit 1
tmp_dir=""
manifest_dir=""
case_resources_registered=0
storage_class_created=0
volume_attributes_class_created=0
case_run_id="drive9-survival-$(date +%s)-$$"
driver_namespace="${DRIVE9_CSI_E2E_DRIVER_NAMESPACE:-}"
test_namespace=""
secret_name=""
storage_class="${DRIVE9_CSI_E2E_STORAGE_CLASS:-drive9-rwo-survival}"
volume_attributes_class="${DRIVE9_CSI_E2E_VOLUME_ATTRIBUTES_CLASS:-}"
if [[ -z "$volume_attributes_class" ]]; then
	volume_attributes_class="drive9-coding-agent-survival"
fi

e2e_init
e2e_configure_case
e2e_need_cmd jq
e2e_need_cmd diff

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

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/drive9-csi-survival.XXXXXX")" ||
	e2e_fail "create temporary directory"
manifest_dir="$tmp_dir/manifests"
mkdir -p "$manifest_dir" || e2e_fail "create manifest directory"
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

e2e_render_case_manifests
e2e_write_primary_workload
e2e_write_test_pod drive9-csi-survival "$tmp_dir/pod.yaml"
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
	e2e_fail "create survival PVC"
e2e_create_owned_namespaced_resource \
	"pod/drive9-csi-survival" "$test_namespace" \
	pod drive9-csi-survival "$case_run_id" "$tmp_dir/pod.yaml" ||
	e2e_fail "create survival Pod"
kube_retry -n "$test_namespace" wait pod/drive9-csi-survival \
	--for=condition=Ready --timeout=300s || e2e_fail "survival Pod ready"

workload_uid=$(kube_retry -n "$test_namespace" get \
	pod drive9-csi-survival \
	-o jsonpath='{.metadata.uid}') || e2e_fail "read workload Pod UID"
node_name=$(kube_retry -n "$test_namespace" get pod drive9-csi-survival \
	-o jsonpath='{.spec.nodeName}') || e2e_fail "read workload node"
[[ -n "$workload_uid" && -n "$node_name" ]] ||
	e2e_fail "workload Pod identity is incomplete"

pv_name=$(kube_retry -n "$test_namespace" get pvc drive9-workspace-e2e \
	-o jsonpath='{.spec.volumeName}') || e2e_fail "read bound PV name"
[[ -n "$pv_name" ]] || e2e_fail "survival PVC did not bind a PV"
volume_id=$(kube_retry get pv "$pv_name" \
	-o jsonpath='{.spec.csi.volumeHandle}') || e2e_fail "read volume ID"
if [[ ! "$volume_id" =~ ^(drive9|drive9-root)-[0-9a-f]{32}$ ]]; then
	e2e_fail "PV contains an unexpected Drive9 volume ID"
fi

if ! node_pod=$(find_node_pod "$node_name"); then
	e2e_fail "resolve CSI node Pod"
fi
node_pod_uid=$(kube_retry -n "$driver_namespace" get pod "$node_pod" \
	-o jsonpath='{.metadata.uid}') || e2e_fail "read CSI node Pod UID"
[[ -n "$node_pod_uid" ]] || e2e_fail "CSI node Pod UID is empty"

io_file=".drive9-csi-survival-$(date +%s).txt"
start_io_loop drive9-csi-survival "$io_file"
if ! count_before=$(wait_for_io_progress drive9-csi-survival 2); then
	e2e_fail "wait for initial workload I/O"
fi
capture_mount_identity "$node_pod" "$volume_id" "$tmp_dir/identity-before.json"

e2e_info "deleting CSI node Pod $node_pod on $node_name"
kube_retry -n "$driver_namespace" delete pod "$node_pod" \
	--ignore-not-found --wait=false ||
	e2e_fail "delete CSI node Pod"
if ! replacement_pod=$(
	wait_for_replacement_node_pod "$node_name" "$node_pod_uid"
); then
	e2e_fail "wait for replacement CSI node Pod"
fi
e2e_info "replacement CSI node Pod is $replacement_pod"

if ! count_after=$(
	wait_for_io_progress drive9-csi-survival "$count_before"
); then
	e2e_fail "wait for workload I/O after CSI node Pod replacement"
fi
if ((count_after <= count_before)); then
	e2e_fail "workload I/O did not continue across CSI node Pod replacement"
fi
current_workload_uid=$(kube_retry -n "$test_namespace" get \
	pod drive9-csi-survival \
	-o jsonpath='{.metadata.uid}') || e2e_fail "read current workload Pod UID"
if [[ "$current_workload_uid" != "$workload_uid" ]]; then
	e2e_fail "workload Pod was replaced during CSI node Pod replacement"
fi

capture_mount_identity "$replacement_pod" "$volume_id" \
	"$tmp_dir/identity-after.json"
if ! diff -u "$tmp_dir/identity-before.json" \
	"$tmp_dir/identity-after.json"; then
	e2e_fail "host mount identity changed across CSI node Pod replacement"
fi
if ! count_final=$(
	wait_for_io_progress drive9-csi-survival "$count_after"
); then
	e2e_fail "verify workload I/O after mount identity capture"
fi
e2e_info "workload I/O progressed from $count_before to $count_final"
e2e_info "passed: workload I/O and host mount identity survived"

stop_io_loop drive9-csi-survival "$io_file"
e2e_delete_owned_namespaced_resource "pod/drive9-csi-survival" \
	"$test_namespace" pod drive9-csi-survival "$case_run_id" ||
	e2e_fail "delete survival Pod"
e2e_delete_owned_pvc "pvc/drive9-workspace-e2e" \
	"$test_namespace" drive9-workspace-e2e "$case_run_id" "$pv_name" ||
	e2e_fail "delete survival PVC"
wait_for_mount_cleanup "$replacement_pod" "$volume_id" \
	"$tmp_dir/identity-before.json"

e2e_info "passed: CSI node Pod mount survival and cleanup"
