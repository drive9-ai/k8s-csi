#!/usr/bin/env bash

# Send the script over stdin because its image path does not exist on the host.
exec nsenter --mount=/host-proc/1/ns/mnt \
	--root=/host-proc/1/root --wd=/host-proc/1/root -- \
	/bin/bash --noprofile --norc -p -s <<'HOST_SCRIPT'
PATH=/usr/bin:/bin
export PATH
bin_dir=/var/lib/drive9-csi/bin
mountinfo=/proc/self/mountinfo

fail() {
	printf 'drive9-csi COS exec preparation: %s\n' "$*" >&2
	exit 1
}

trusted_directory() {
	local directory="$1" metadata owner mode

	[[ -d "$directory" && ! -L "$directory" ]] ||
		fail "not a real directory: $directory"
	metadata=$(stat -c '%u %a' -- "$directory") ||
		fail "stat failed: $directory"
	read -r owner mode <<< "$metadata"
	[[ "$owner" == 0 && "$mode" =~ ^[0-7]{3,4}$ ]] ||
		fail "directory is not root-owned: $directory"
	(( (8#$mode & 0022) == 0 )) ||
		fail "directory is writable by group or others: $directory"
}

read_mounts() {
	local id _parent device root target options _rest
	local parent_device='' parent_root='' parent_target=''
	local bind_device='' bind_root='' expected_root

	bind_id=''
	bin_options=''
	parent_id=''
	parent_options=''
	while read -r id _parent device root target options _rest; do
		[[ "$id" =~ ^[0-9]+$ && -n "$options" ]] ||
			fail 'invalid host mountinfo record'
		if [[ "$target" == "$bin_dir"/* ]]; then
			fail "unexpected mount below binary directory: $target"
		elif [[ "$target" == "$bin_dir" ]]; then
			[[ -z "$bind_id" ]] || fail 'stacked binary directory mounts'
			bind_id="$id"
			bind_device="$device"
			bind_root="$root"
			bin_options="$options"
		elif [[ "$target" == / || "$bin_dir" == "$target"/* ]]; then
			if (( ${#target} > ${#parent_target} )); then
				parent_id="$id"
				parent_device="$device"
				parent_root="$root"
				parent_target="$target"
				parent_options="$options"
			elif [[ "$target" == "$parent_target" ]]; then
				fail "stacked parent mounts: $target"
			fi
		fi
	done < "$mountinfo"
	[[ -n "$parent_id" ]] || fail 'binary directory parent mount not found'
	expected_root="${parent_root%/}/${bin_dir#"${parent_target%/}/"}"
	if [[ -n "$bind_id" ]]; then
		[[ "$bind_device" == "$parent_device" &&
			"$bind_root" == "$expected_root" ]] ||
			fail 'binary directory mount has an unexpected backing subtree'
	else
		bin_options="$parent_options"
	fi
}

for tool in mount stat; do
	command -v "$tool" >/dev/null || fail "missing host command: $tool"
done
for directory in /var /var/lib /var/lib/drive9-csi; do
	trusted_directory "$directory"
done
if [[ ! -e "$bin_dir" && ! -L "$bin_dir" ]]; then
	mkdir -m 0755 -- "$bin_dir" || fail 'cannot create binary directory'
fi
trusted_directory "$bin_dir"
read_mounts
before_parent_id="$parent_id"
before_parent_options="$parent_options"
if [[ -z "$bind_id" && ",$bin_options," != *,noexec,* ]]; then
	printf 'drive9-csi COS binary directory is already executable\n'
	exit 0
fi
if [[ -z "$bind_id" ]]; then
	mount -n --bind "$bin_dir" "$bin_dir" || fail 'self-bind failed'
	read_mounts
	[[ -n "$bind_id" ]] || fail 'self-bind did not create a mountpoint'
fi
if [[ ",$bin_options," == *,noexec,* ||
	",$bin_options," != *,nosuid,* || ",$bin_options," != *,nodev,* ]]; then
	options="${bin_options//noexec/exec},exec,nosuid,nodev"
	mount -n -o "remount,bind,$options" "$bin_dir" "$bin_dir" ||
		fail 'exec remount failed; existing bind retained for retry'
fi
read_mounts
[[ "$parent_id" == "$before_parent_id" &&
	"$parent_options" == "$before_parent_options" ]] ||
	fail 'parent mount changed during preparation'
[[ -n "$bind_id" && ",$bin_options," == *,rw,* &&
	",$bin_options," != *,noexec,* && ",$bin_options," == *,nosuid,* &&
	",$bin_options," == *,nodev,* ]] || fail 'exec mount verification failed'
printf 'drive9-csi COS binary directory prepared: %s\n' "$bin_dir"
HOST_SCRIPT
