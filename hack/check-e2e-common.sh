#!/usr/bin/env bash

script_dir="$(cd "${0%/*}" && pwd)" || exit 1
repo_root="$(cd "$script_dir/.." && pwd)" || exit 1
source "$repo_root/e2e/lib/common.sh" || exit 1

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

printf 'e2e common checks passed\n'
