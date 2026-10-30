#!/usr/bin/env bash

fail() {
	printf 'COS script-check: %s\n' "$*" >&2
	exit 1
}

script_dir="$(cd "${0%/*}" && pwd)" || exit 1
helper="$script_dir/drive9-csi-prepare-cos-exec.sh"
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/drive9-cos-check.XXXXXX") || exit 1
trap 'rm -rf "$tmp_dir"' EXIT
bin_dir="$tmp_dir/tools"
mkdir -p "$bin_dir" || exit 1

# Capture the real namespace bootstrap without entering the coordinator host.
cat > "$bin_dir/nsenter" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$COS_TEST_ROOT/nsenter-args"
cat > "$COS_TEST_ROOT/host-body"
MOCK
chmod +x "$bin_dir/nsenter" || exit 1
COS_TEST_ROOT="$tmp_dir" PATH="$bin_dir:$PATH" "$helper" ||
	fail 'namespace bootstrap failed'
expected='--mount=/host-proc/1/ns/mnt
--root=/host-proc/1/root
--wd=/host-proc/1/root
--
/bin/bash
--noprofile
--norc
-p
-s'
[[ "$(<"$tmp_dir/nsenter-args")" == "$expected" ]] ||
	fail 'bootstrap did not select host root and stdin shell'

cat > "$bin_dir/stat" <<'MOCK'
#!/usr/bin/env bash
case "$(<"$COS_TEST_CASE/metadata"):${!#}" in
	unsafe:*/bin|unsafe-parent:*) printf '0 777\n' ;;
	owner:*/bin) printf '1000 755\n' ;;
	*) printf '0 755\n' ;;
esac
MOCK
cat > "$bin_dir/mount" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$COS_TEST_CASE/mount-log"
failure=$(<"$COS_TEST_CASE/failure")
if [[ "$2" == --bind ]]; then
	[[ "$failure" != bind ]] || exit 1
	printf '21 10 8:1 /var/lib/drive9-csi/bin %s rw,nosuid,nodev,noexec,noatime - ext4 /dev/test rw\n' \
		"$COS_TEST_CASE/var/lib/drive9-csi/bin" >> "$COS_TEST_CASE/mountinfo"
else
	[[ "$failure" != remount ]] || exit 1
	options="${3#remount,bind,}"
	[[ "$options" == *noatime* ]] || exit 2
	[[ "$failure" != flags ]] || options=rw,noexec
	awk -v target="$COS_TEST_CASE/var/lib/drive9-csi/bin" \
		-v options="$options" -v failure="$failure" '
		$5 == target { $6 = options }
		failure == "parent" && $1 == "10" { $6 = "rw" }
		{ print }
	' "$COS_TEST_CASE/mountinfo" > "$COS_TEST_CASE/mountinfo-new" || exit 1
	mv "$COS_TEST_CASE/mountinfo-new" "$COS_TEST_CASE/mountinfo" || exit 1
fi
MOCK
chmod +x "$bin_dir/stat" "$bin_dir/mount" || exit 1

fixture() {
	case_dir="$tmp_dir/$1"
	mkdir -p "$case_dir/var/lib/drive9-csi" || exit 1
	printf 'safe\n' > "$case_dir/metadata"
	printf 'none\n' > "$case_dir/failure"
	: > "$case_dir/mount-log"
	printf '10 1 8:1 /var %s rw,nosuid,nodev,noexec,noatime - ext4 /dev/test rw\n' \
		"$case_dir/var" > "$case_dir/mountinfo"
	# Redirect only filesystem/tool boundaries; execute the captured host logic.
	sed -e "s|/var|$case_dir/var|g" \
		-e "s|^mountinfo=/proc/self/mountinfo$|mountinfo=$case_dir/mountinfo|" \
		-e "s|^PATH=/usr/bin:/bin$|PATH=$bin_dir:/usr/bin:/bin|" \
		"$tmp_dir/host-body" > "$case_dir/host-script" || exit 1
}

run_host() {
	COS_TEST_CASE="$case_dir" /bin/bash --noprofile --norc -p \
		"$case_dir/host-script" > "$case_dir/stdout" 2> "$case_dir/stderr"
}

expect_failure() {
	if run_host; then
		fail "$1 unexpectedly succeeded"
	fi
	grep -Fq "$1" "$case_dir/stderr" ||
		fail "missing failure detail: $1"
}

fixture fresh
run_host || fail "fresh preparation: $(<"$case_dir/stderr")"
[[ -d "$case_dir/var/lib/drive9-csi/bin" ]] || fail 'binary dir not created'
[[ "$(wc -l < "$case_dir/mount-log")" == *2 ]] || fail 'expected bind and remount'
grep -Fq 'rw,nosuid,nodev,noexec,noatime' "$case_dir/mountinfo" ||
	fail 'parent options changed'
run_host || fail 'repeat preparation failed'
[[ "$(wc -l < "$case_dir/mount-log")" == *2 ]] || fail 'repeat mounted again'

fixture partial
printf 'remount\n' > "$case_dir/failure"
expect_failure 'exec remount failed'
printf 'none\n' > "$case_dir/failure"
run_host || fail 'partial preparation was not recoverable'
[[ "$(grep -c -- --bind "$case_dir/mount-log")" == 1 ]] ||
	fail 'partial retry stacked a bind'

for fault in bind flags parent; do
	fixture "$fault"
	printf '%s\n' "$fault" > "$case_dir/failure"
	case "$fault" in
		bind) expect_failure 'self-bind failed' ;;
		flags) expect_failure 'exec mount verification failed' ;;
		parent) expect_failure 'parent mount changed' ;;
	esac
done

for fault in unsafe owner unsafe-parent; do
	fixture "$fault"
	printf '%s\n' "$fault" > "$case_dir/metadata"
	if [[ "$fault" != owner ]]; then
		expect_failure 'writable by group or others'
	else
		expect_failure 'not root-owned'
	fi
	[[ ! -s "$case_dir/mount-log" ]] || fail 'unsafe path caused a mount'
done

fixture symlink
mkdir "$case_dir/other" || exit 1
ln -s "$case_dir/other" "$case_dir/var/lib/drive9-csi/bin" || exit 1
expect_failure 'not a real directory'
[[ ! -s "$case_dir/mount-log" ]] || fail 'symlink caused a mount'

for fault in foreign stacked nested; do
	fixture "$fault"
	mkdir "$case_dir/var/lib/drive9-csi/bin" || exit 1
	target="$case_dir/var/lib/drive9-csi/bin"
	case "$fault" in
		foreign) source_root=/other ;;
		*) source_root=/var/lib/drive9-csi/bin ;;
	esac
	printf '21 10 8:1 %s %s rw,noexec - ext4 /dev/test rw\n' \
		"$source_root" "$target" >> "$case_dir/mountinfo"
	case "$fault" in
		foreign) expect_failure 'unexpected backing subtree' ;;
		stacked)
			printf '22 21 8:1 %s %s rw,noexec - ext4 /dev/test rw\n' \
				"$source_root" "$target" >> "$case_dir/mountinfo"
			expect_failure 'stacked binary directory mounts'
			;;
		nested)
			printf '22 21 8:1 /other %s/nested rw - ext4 /dev/test rw\n' \
				"$target" >> "$case_dir/mountinfo"
			expect_failure 'unexpected mount below binary directory'
			;;
	esac
	[[ ! -s "$case_dir/mount-log" ]] || fail 'unexpected mount was modified'
done

fixture executable
printf '10 1 8:1 /var %s rw,noatime - ext4 /dev/test rw\n' \
	"$case_dir/var" > "$case_dir/mountinfo"
run_host || fail 'already executable parent failed'
[[ ! -s "$case_dir/mount-log" ]] || fail 'executable parent was mounted'
printf 'COS script-check: ok\n'
