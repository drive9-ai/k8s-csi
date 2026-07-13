#!/usr/bin/env bash

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	printf 'error: e2e/lib/manifests.sh must be sourced\n' >&2
	exit 2
fi

e2e_copy_manifest() {
	local source="$1"
	local target="$2"
	local expected_replacements="$3"
	local expected_namespaces="$4"

	awk -v image="$DRIVE9_CSI_IMAGE" \
		-v namespace="$driver_namespace" \
		-v expected="$expected_replacements" \
		-v expected_namespaces="$expected_namespaces" '
		$0 ~ /^[[:space:]]*image:/ &&
		$0 ~ /registry\.invalid\/drive9-csi:unpublished/ {
			sub(/image: .*/, "image: " image)
			replacements += 1
		}
		$0 == "  namespace: drive9-csi" {
			print "  namespace: " namespace
			namespace_replacements += 1
			next
		}
		{ print }
		END {
			if (replacements != expected ||
				namespace_replacements != expected_namespaces) exit 42
		}
	' "$repo_root/deploy/kubernetes/$source" > "$manifest_dir/$target" ||
		e2e_fail "render $source"
}

e2e_write_driver_namespace() {
	awk -v namespace="$driver_namespace" '
		$0 == "  name: drive9-csi" {
			print "  name: " namespace
			replacements += 1
			next
		}
		{ print }
		END {
			if (replacements != 1) exit 42
		}
	' "$repo_root/deploy/kubernetes/namespace.yaml" \
		> "$manifest_dir/namespace.yaml" ||
		e2e_fail "render namespace"
}

e2e_write_rbac() {
	awk -v namespace="$driver_namespace" '
		$0 ~ /^[[:space:]]+namespace: drive9-csi$/ {
			sub(/drive9-csi$/, namespace)
			print
			replacements += 1
			next
		}
		{ print }
		END {
			if (replacements != 4) exit 42
		}
	' "$repo_root/deploy/kubernetes/rbac.yaml" > "$manifest_dir/rbac.yaml" ||
		e2e_fail "render rbac"
}

e2e_write_storageclass() {
	awk \
		-v storage_class="$storage_class" \
		-v root_prefix="$DRIVE9_REMOTE_ROOT_PREFIX" \
		-v run_id="$case_run_id" '
		$0 == "metadata:" {
			print
			print "  labels:"
			print "    drive9.ai/e2e-run: " run_id
			next
		}
		$0 == "  name: drive9-rwo" {
			print "  name: " storage_class
			next
		}
		$0 ~ /^parameters:/ {
			if (root_prefix != "") {
				print "parameters:"
				print "  remoteRootPrefix: |-"
				print "    " root_prefix
			} else {
				print "parameters: {}"
			}
			next
		}
		$0 ~ /^  remoteRootPrefix:/ { next }
		$0 ~ /^reclaimPolicy:/ {
			print "reclaimPolicy: Delete"
			next
		}
		{ print }
	' "$repo_root/deploy/kubernetes/storageclass.yaml" \
		> "$manifest_dir/storageclass.yaml" ||
		e2e_fail "render storageclass"
}

e2e_write_volume_attributes_class() {
	awk \
		-v volume_attributes_class="$volume_attributes_class" \
		-v profile="$DRIVE9_PROFILE" \
		-v run_id="$case_run_id" '
		$0 == "metadata:" {
			print
			print "  labels:"
			print "    drive9.ai/e2e-run: " run_id
			next
		}
		$0 == "  name: drive9-coding-agent" {
			print "  name: " volume_attributes_class
			next
		}
		$0 ~ /^  profile:/ {
			print "  profile: |-"
			print "    " profile
			next
		}
		{ print }
	' "$repo_root/deploy/kubernetes/volumeattributesclass.yaml" \
		> "$manifest_dir/volumeattributesclass.yaml" ||
		e2e_fail "render volumeattributesclass"
}

e2e_render_driver_manifests() {
	cp "$repo_root/deploy/kubernetes/csidriver.yaml" \
		"$manifest_dir/csidriver.yaml" || e2e_fail "copy csidriver"
	e2e_write_driver_namespace
	e2e_write_rbac
	e2e_copy_manifest controller.yaml controller.yaml 1 1
	e2e_copy_manifest node.yaml node.yaml 2 1
}

e2e_validate_driver_manifests() {
	local count=0
	local file
	local files=()
	local image_count
	local mode

	files=(namespace.yaml csidriver.yaml rbac.yaml controller.yaml node.yaml)
	for file in "${files[@]}"; do
		[[ -s "$manifest_dir/$file" ]] ||
			e2e_fail "prepared Driver manifest is missing: $file"
	done
	for file in "$manifest_dir"/*; do
		[[ -f "$file" ]] || e2e_fail "invalid Driver manifest entry"
		case "${file##*/}" in
		namespace.yaml | csidriver.yaml | rbac.yaml | controller.yaml | node.yaml)
			;;
		*)
			e2e_fail "unexpected prepared Driver manifest: ${file##*/}"
			;;
		esac
		count=$((count + 1))
	done
	((count == 5)) || e2e_fail "prepared Driver manifest set is incomplete"

	if grep -Fq 'registry.invalid/drive9-csi:unpublished' \
		"$manifest_dir/controller.yaml" "$manifest_dir/node.yaml"; then
		e2e_fail "prepared Driver manifests contain the placeholder image"
	fi
	image_count=$(awk -v image="$DRIVE9_CSI_IMAGE" '
		index($0, "image: " image) { count += 1 }
		END { print count + 0 }
	' "$manifest_dir/controller.yaml" "$manifest_dir/node.yaml") ||
		e2e_fail "count prepared Driver images"
	[[ "$image_count" == "3" ]] ||
		e2e_fail "prepared Driver image must appear exactly three times"
	for mode in enforce audit warn; do
		grep -Fq "pod-security.kubernetes.io/$mode: privileged" \
			"$manifest_dir/namespace.yaml" ||
			e2e_fail "prepared namespace lost privileged Pod Security labels"
	done
}

e2e_render_case_manifests() {
	e2e_write_storageclass
	e2e_write_volume_attributes_class
}

e2e_validate_case_manifests() {
	local count=0
	local file

	for file in storageclass.yaml volumeattributesclass.yaml; do
		[[ -s "$manifest_dir/$file" ]] ||
			e2e_fail "case manifest is missing: $file"
		grep -Fq "drive9.ai/e2e-run: $case_run_id" \
			"$manifest_dir/$file" ||
			e2e_fail "case manifest has no ownership label: $file"
	done
	for file in "$manifest_dir"/*; do
		[[ -f "$file" ]] || e2e_fail "invalid case manifest entry"
		case "${file##*/}" in
		storageclass.yaml | volumeattributesclass.yaml)
			;;
		*)
			e2e_fail "unexpected case manifest: ${file##*/}"
			;;
		esac
		count=$((count + 1))
	done
	((count == 2)) || e2e_fail "case manifest set is incomplete"
}

e2e_write_primary_workload() {
	cat > "$tmp_dir/workload.yaml" <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: drive9-workspace-e2e
  namespace: $test_namespace
  labels:
    drive9.ai/e2e-run: $case_run_id
  annotations:
    drive9.ai/secret-name: $secret_name
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: $storage_class
  volumeAttributesClassName: $volume_attributes_class
  resources:
    requests:
      storage: 1Gi
EOF
	[[ "$?" == "0" ]] || e2e_fail "write primary workload"
}

e2e_write_test_pod() {
	local pod_name="$1"
	local target="$2"
	local readonly="${3:-false}"

	[[ "$readonly" == "true" || "$readonly" == "false" ]] ||
		e2e_fail "test Pod readonly value must be true or false"

	cat > "$target" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
  namespace: $test_namespace
  labels:
    drive9.ai/e2e-run: $case_run_id
spec:
  restartPolicy: Never
  containers:
    - name: app
      image: busybox:1.36
      command: ["/bin/sleep", "3600"]
      volumeMounts:
        - name: workspace
          mountPath: /workspace
          readOnly: $readonly
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: drive9-workspace-e2e
        readOnly: $readonly
EOF
	[[ "$?" == "0" ]] || e2e_fail "write test Pod"
}

e2e_write_test_pod_on_node() {
	local pod_name="$1"
	local target="$2"
	local node_name="$3"

	cat > "$target" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
  namespace: $test_namespace
  labels:
    drive9.ai/e2e-run: $case_run_id
spec:
  restartPolicy: Never
  nodeName: $node_name
  containers:
    - name: app
      image: busybox:1.36
      command: ["/bin/sh", "-c", "sleep 3600"]
      volumeMounts:
        - name: workspace
          mountPath: /workspace
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: drive9-workspace-e2e
EOF
	[[ "$?" == "0" ]] || e2e_fail "write node-pinned test Pod"
}

e2e_write_second_pvc() {
	cat > "$tmp_dir/workload-b.yaml" <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: drive9-workspace-e2e-b
  namespace: $test_namespace
  labels:
    drive9.ai/e2e-run: $case_run_id
  annotations:
    drive9.ai/secret-name: $secret_name
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: $storage_class
  volumeAttributesClassName: $volume_attributes_class
  resources:
    requests:
      storage: 1Gi
EOF
	[[ "$?" == "0" ]] || e2e_fail "write second workload"
}

e2e_write_multi_pvc_pod() {
	local pod_name="$1"
	local target="$2"

	cat > "$target" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
  namespace: $test_namespace
  labels:
    drive9.ai/e2e-run: $case_run_id
spec:
  restartPolicy: Never
  containers:
    - name: app
      image: busybox:1.36
      command: ["/bin/sh", "-c", "sleep 3600"]
      volumeMounts:
        - name: workspace-a
          mountPath: /workspace-a
        - name: workspace-b
          mountPath: /workspace-b
  volumes:
    - name: workspace-a
      persistentVolumeClaim:
        claimName: drive9-workspace-e2e
    - name: workspace-b
      persistentVolumeClaim:
        claimName: drive9-workspace-e2e-b
EOF
	[[ "$?" == "0" ]] || e2e_fail "write multi-PVC test Pod"
}
