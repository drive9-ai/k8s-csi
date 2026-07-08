#!/bin/sh

default_server="https://api.drive9.ai"

case_id=""
volume_id=""
server="${default_server}"
token_stdin="false"
state_dir="${DRIVE9_CSI_STATE_DIR:-/var/lib/drive9-csi}"
tty_state=""

usage() {
	cat <<EOF
Usage: drive9-csi-upload-perf --case-id <case-id> [options]

Options:
  --case-id <case-id>     Support case identifier. Required.
  --volume-id <volume>    Perf volume directory to upload.
  --server <url>          Drive9 support API endpoint.
                          Default: ${default_server}
  --token-stdin           Read support upload token from stdin.
  --keep-bundle           Keep the local tarball after upload. Default behavior.
  -h, --help              Show this help text.
EOF
}

die() {
	printf 'drive9-csi-upload-perf: %s\n' "$*" >&2
	exit 1
}

restore_tty() {
	if [ -n "${tty_state}" ]; then
		stty "${tty_state}" 2>/dev/null
		tty_state=""
	fi
}

validate_component() {
	label="$1"
	value="$2"
	max_len="$3"

	if [ -z "${value}" ]; then
		die "${label} is required"
	fi
	if [ "${#value}" -gt "${max_len}" ]; then
		die "${label} must be ${max_len} bytes or shorter"
	fi
	case "${value}" in
		*..*)
			die "${label} must not contain '..'"
			;;
	esac
	case "${value}" in
		*[!A-Za-z0-9._-]*)
			die "${label} contains invalid characters"
			;;
	esac
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--case-id)
			if [ "$#" -lt 2 ]; then
				die "--case-id requires a value"
			fi
			case_id="$2"
			shift 2
			;;
		--case-id=*)
			case_id="${1#--case-id=}"
			shift
			;;
		--volume-id)
			if [ "$#" -lt 2 ]; then
				die "--volume-id requires a value"
			fi
			volume_id="$2"
			shift 2
			;;
		--volume-id=*)
			volume_id="${1#--volume-id=}"
			shift
			;;
		--server)
			if [ "$#" -lt 2 ]; then
				die "--server requires a value"
			fi
			server="$2"
			shift 2
			;;
		--server=*)
			server="${1#--server=}"
			shift
			;;
		--token-stdin)
			token_stdin="true"
			shift
			;;
		--keep-bundle)
			shift
			;;
		-h|--help)
			usage
			exit 0
			;;
		--)
			shift
			if [ "$#" -ne 0 ]; then
				die "unexpected positional argument: $1"
			fi
			;;
		*)
			die "unknown argument: $1"
			;;
	esac
done

validate_component "--case-id" "${case_id}" 128
if [ -z "${server}" ]; then
	die "--server must not be empty"
fi

while [ "${state_dir}" != "/" ] &&
	[ "${state_dir%/}" != "${state_dir}" ]; do
	state_dir="${state_dir%/}"
done
if [ -z "${state_dir}" ]; then
	state_dir="/"
fi
if [ "${state_dir}" = "/" ]; then
	perf_root="/perf"
else
	perf_root="${state_dir}/perf"
fi

print_volume_candidates() {
	for path in "${perf_root}"/*; do
		if [ -d "${path}" ] && [ ! -L "${path}" ]; then
			printf '  %s\n' "${path##*/}" >&2
		fi
	done
}

select_volume_id() {
	if [ -n "${volume_id}" ]; then
		validate_component "--volume-id" "${volume_id}" 128
		volume_dir="${perf_root}/${volume_id}"
		if [ ! -d "${volume_dir}" ] || [ -L "${volume_dir}" ]; then
			die "--volume-id must name an existing directory under ${perf_root}"
		fi
		return
	fi

	if [ ! -d "${perf_root}" ]; then
		die "perf directory does not exist: ${perf_root}"
	fi

	count=0
	selected=""
	for path in "${perf_root}"/*; do
		if [ -d "${path}" ] && [ ! -L "${path}" ]; then
			count=$((count + 1))
			selected="${path##*/}"
		fi
	done

	case "${count}" in
		0)
			die "no perf volume directories found under ${perf_root}"
			;;
		1)
			volume_id="${selected}"
			validate_component "--volume-id" "${volume_id}" 128
			volume_dir="${perf_root}/${volume_id}"
			;;
		*)
			printf 'multiple perf volume directories found under %s:\n' \
				"${perf_root}" >&2
			print_volume_candidates
			die "rerun with --volume-id <volume-id>"
			;;
	esac
}

resolve_node_name() {
	node_name="${DRIVE9_CSI_NODE_NAME:-}"
	if [ -z "${node_name}" ]; then
		node_name="${NODE_NAME:-}"
	fi
	if [ -z "${node_name}" ] && command -v hostname >/dev/null 2>&1; then
		node_name="$(hostname 2>/dev/null)"
	fi
	if [ -z "${node_name}" ]; then
		node_name="unknown-node"
	fi
	case "${node_name}" in
		*..*|*[!A-Za-z0-9._-]*)
			node_name="unknown-node"
			;;
	esac
	if [ "${#node_name}" -gt 128 ]; then
		node_name="unknown-node"
	fi
}

read_token() {
	token=""
	if [ "${token_stdin}" = "true" ]; then
		IFS= read -r token || :
	else
		if [ ! -t 0 ]; then
			die "stdin is not a TTY; pass --token-stdin to read the token"
		fi
		tty_state="$(stty -g 2>/dev/null)"
		if [ -z "${tty_state}" ]; then
			die "cannot read terminal settings; pass --token-stdin"
		fi
		if ! stty -echo 2>/dev/null; then
			die "cannot disable terminal echo; pass --token-stdin"
		fi
		trap 'restore_tty; exit 129' HUP
		trap 'restore_tty; exit 130' INT
		trap 'restore_tty; exit 143' TERM
		trap 'restore_tty' EXIT
		printf 'Drive9 support upload token: ' >&2
		IFS= read -r token || :
		restore_tty
		trap - HUP INT TERM EXIT
		printf '\n' >&2
	fi
	if [ -z "${token}" ]; then
		die "support upload token is required"
	fi
}

select_volume_id
resolve_node_name

bundle="${perf_root}/${case_id}.tgz"
destination=":/support-inbox/${case_id}/${node_name}/${volume_id}.tgz"

if ! tar czf "${bundle}" -C "${perf_root}" -- "${volume_id}"; then
	die "create perf bundle failed: ${bundle}"
fi

read_token

if ! DRIVE9_SERVER="${server}" DRIVE9_API_KEY="${token}" drive9 fs cp \
	--tag "case=${case_id}" \
	--tag "source=k8s-csi" \
	--tag "node=${node_name}" \
	--tag "volume=${volume_id}" \
	--description "Drive9 CSI perf bundle" \
	"${bundle}" \
	"${destination}"; then
	die "upload failed: ${destination}"
fi

if ! DRIVE9_SERVER="${server}" DRIVE9_API_KEY="${token}" drive9 fs stat \
	"${destination}"; then
	die "upload verification failed: ${destination}"
fi

printf 'Uploaded perf bundle: %s\n' "${destination}"
printf 'Local perf bundle: %s\n' "${bundle}"
