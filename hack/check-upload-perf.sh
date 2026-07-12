#!/usr/bin/env bash

fail() {
	printf 'script-check: %s\n' "$*" >&2
	exit 1
}

cleanup() {
	local exit_code="$?"

	trap - EXIT INT TERM
	if [[ -n "$tmp_dir" && -d "$tmp_dir" ]]; then
		rm -rf "$tmp_dir" || exit_code=1
	fi
	exit "$exit_code"
}

assert_contains() {
	local path="$1"
	local value="$2"

	grep -Fq -- "$value" "$path" ||
		fail "$path does not contain: $value"
}

assert_not_contains() {
	local path="$1"
	local value="$2"

	if grep -Fq -- "$value" "$path"; then
		fail "$path contains forbidden value: $value"
	fi
}

run_upload() {
	local state_dir="$1"
	local stdin_body="$2"
	local stdout_path="$3"
	local stderr_path="$4"
	local exit_code
	shift 4

	printf '%s' "$stdin_body" |
		DRIVE9_CSI_STATE_DIR="$state_dir" \
		DRIVE9_CSI_NODE_NAME="node-a" \
		DRIVE9_SERVER="https://api.drive9.ai" \
		DRIVE9_FAKE_LOG="$log_path" \
		PATH="$bin_dir:$PATH" \
		/bin/sh "$helper" "$@" >"$stdout_path" 2>"$stderr_path"
	exit_code="${PIPESTATUS[1]}"
	return "$exit_code"
}

script_dir="$(cd "${0%/*}" && pwd)" || exit 1
helper="$script_dir/drive9-csi-upload-perf.sh"
tmp_dir=""

for command in grep mktemp tar; do
	command -v "$command" >/dev/null 2>&1 ||
		fail "missing command: $command"
done
/bin/sh -n "$helper" || fail "upload helper syntax"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/drive9-script-check.XXXXXX")" ||
	fail "create temporary directory"
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

bin_dir="$tmp_dir/bin"
log_path="$tmp_dir/drive9.log"
mkdir -p "$bin_dir" || fail "create fake binary directory"
cat >"$bin_dir/drive9" <<'EOF'
#!/bin/sh
{
	printf 'cmd=%s %s\n' "$1" "$2"
	shift 2
	i=1
	for arg do
		printf 'arg%d=%s\n' "$i" "$arg"
		i=$((i + 1))
	done
	printf 'server=%s\n' "${DRIVE9_SERVER:-}"
	if [ -n "${DRIVE9_API_KEY:-}" ]; then
		printf 'token=present\n'
	else
		printf 'token=missing\n'
		exit 17
	fi
} >>"$DRIVE9_FAKE_LOG"
EOF
[[ "$?" == "0" ]] || fail "write fake drive9"
chmod +x "$bin_dir/drive9" || fail "make fake drive9 executable"

stdout_path="$tmp_dir/stdout"
stderr_path="$tmp_dir/stderr"

state_dir="$tmp_dir/missing-case"
mkdir -p "$state_dir" || fail "create missing-case state"
if run_upload "$state_dir" token "$stdout_path" "$stderr_path" \
	--token-stdin; then
	fail "upload helper accepted a missing case ID"
fi
[[ ! -s "$stdout_path" ]] || fail "missing-case wrote stdout"
assert_contains "$stderr_path" "--case-id is required"

state_dir="$tmp_dir/happy"
perf_root="$state_dir/perf"
volume_id="drive9-volume-1"
mkdir -p "$perf_root/$volume_id" || fail "create happy perf directory"
printf 'sample' >"$perf_root/$volume_id/sample.txt" ||
	fail "write perf sample"
: >"$log_path" || fail "clear fake drive9 log"
token="support-token-secret"
if ! run_upload "$state_dir" "$token" "$stdout_path" "$stderr_path" \
	--case-id CASE-123 --token-stdin; then
	fail "happy upload failed"
fi
bundle="$perf_root/CASE-123.tgz"
[[ -f "$bundle" ]] || fail "upload bundle was not created"
destination=":/support-inbox/CASE-123/node-a/$volume_id.tgz"
assert_contains "$stdout_path" "Uploaded perf bundle: $destination"
assert_contains "$stdout_path" "Local perf bundle: $bundle"
for value in \
	"cmd=fs cp" \
	"cmd=fs stat" \
	"server=https://api.drive9.ai" \
	"token=present" \
	"case=CASE-123" \
	"source=k8s-csi" \
	"node=node-a" \
	"volume=$volume_id" \
	"Drive9 CSI perf bundle" \
	"$bundle" \
	"$destination"; do
	assert_contains "$log_path" "$value"
done
for path in "$stdout_path" "$stderr_path" "$log_path" "$bundle"; do
	assert_not_contains "$path" "$token"
done

state_dir="$tmp_dir/multiple"
mkdir -p "$state_dir/perf/drive9-a" "$state_dir/perf/drive9-b" ||
	fail "create multiple-volume state"
printf 'old' >"$state_dir/perf/old.tgz" || fail "write old bundle"
if run_upload "$state_dir" token "$stdout_path" "$stderr_path" \
	--case-id CASE-123 --token-stdin; then
	fail "upload helper accepted ambiguous volume directories"
fi
for value in drive9-a drive9-b "rerun with --volume-id"; do
	assert_contains "$stderr_path" "$value"
done
assert_not_contains "$stderr_path" "old.tgz"

state_dir="$tmp_dir/missing-token"
mkdir -p "$state_dir/perf/drive9-vol" ||
	fail "create missing-token state"
: >"$log_path" || fail "clear fake drive9 log"
if run_upload "$state_dir" "" "$stdout_path" "$stderr_path" \
	--case-id CASE-123; then
	fail "upload helper accepted a missing non-interactive token"
fi
assert_contains "$stderr_path" "pass --token-stdin"
[[ ! -s "$log_path" ]] || fail "drive9 ran without a token"

state_dir="$tmp_dir/unsafe"
mkdir -p "$state_dir/perf/drive9-vol" || fail "create unsafe state"
if run_upload "$state_dir" token "$stdout_path" "$stderr_path" \
	--case-id CASE..123 --token-stdin; then
	fail "upload helper accepted case traversal"
fi
assert_contains "$stderr_path" "--case-id must not contain '..'"
if run_upload "$state_dir" token "$stdout_path" "$stderr_path" \
	--case-id CASE-123 --volume-id bad/vol --token-stdin; then
	fail "upload helper accepted an unsafe volume ID"
fi
assert_contains "$stderr_path" "--volume-id contains invalid characters"

printf 'script-check: ok\n'
