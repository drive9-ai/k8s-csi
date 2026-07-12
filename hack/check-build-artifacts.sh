#!/usr/bin/env bash

fail() {
	printf 'build-check: %s\n' "$*" >&2
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

script_dir="$(cd "${0%/*}" && pwd)" || exit 1
repo_root="$(cd "$script_dir/.." && pwd)" || exit 1
tmp_dir=""

cd "$repo_root" || exit 1

command -v go >/dev/null 2>&1 || fail "missing command: go"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/drive9-build-check.XXXXXX")" ||
	fail "create temporary directory"
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

verifier="$tmp_dir/drive9-csi-build"
go_cache="$tmp_dir/go-cache"
if ! CGO_ENABLED=0 GOCACHE="$go_cache" go build -buildvcs=false \
	-trimpath -o "$verifier" ./cmd/drive9-csi; then
	fail "build artifact verifier"
fi

names=(drive9-csi drive9-csi-launcher)
packages=(./cmd/drive9-csi ./cmd/drive9-csi-launcher)
go_paths=(
	github.com/drive9-ai/csi/cmd/drive9-csi
	github.com/drive9-ai/csi/cmd/drive9-csi-launcher
)

for arch in amd64 arm64; do
	for index in 0 1; do
		output="$tmp_dir/${names[$index]}-$arch"
		if ! CGO_ENABLED=0 GOCACHE="$go_cache" GOOS=linux GOARCH="$arch" \
			go build -buildvcs=false -trimpath -o "$output" \
			"${packages[$index]}"; then
			fail "build ${names[$index]} for linux/$arch"
		fi
		if ! "$verifier" verify-host-binary \
			--path="$output" --target-arch="$arch"; then
			fail "verify ${names[$index]} for linux/$arch"
		fi
		if ! metadata=$(go version -m "$output"); then
			fail "read ${names[$index]} build metadata"
		fi
		path_marker=$'\tpath\t'"${go_paths[$index]}"
		if [[ "$metadata" != *"$path_marker"* ]]; then
			fail "unexpected ${names[$index]} Go build path"
		fi
	done
done

printf 'build-check: ok\n'
