---
title: Drive9 CSI Lite
---

This repository provides a minimal Kubernetes integration for
`github.com/mem9-ai/drive9`.

It intentionally ships a small stable surface first:

- PVCs mount the Drive9 workspace root selected by the per-PVC API key by
  default.
- Optional managed directory volumes backed by Drive9 remote paths.
- `ReadWriteOnce` by default. `SINGLE_NODE_MULTI_WRITER` supported for same-node
  multi-pod access.
- `ReadWriteMany` adds no `profile` or `durability` requirements or defaults;
  configured mount parameters are forwarded to `drive9 mount`.
- Credentials are resolved from PVC annotation `drive9.ai/secret-name` →
  Kubernetes Secret.
- Default workspace-root volumes do not create or delete Drive9 workspace data.
- Managed directory volumes write a marker file.
- `DeleteVolume` detaches CSI ownership only: it removes CSI metadata (marker,
  index, name index) but never deletes Drive9 workspace data.
- `NodeStageVolume` runs
  `drive9 mount --supervise-foreground --mode=fuse --direct-mount-strict`
  through a host systemd service.
- `NodePublishVolume` bind-mounts the staged path into the pod.
- No snapshots, expansion, or automatic tenant provisioning.

## Why CSI Lite

Customers usually already run business workloads in Kubernetes pods and want
normal PVCs. CSI Lite gives them that path without requiring application-level
changes.

The driver does not reimplement Drive9 FUSE. It installs the official `drive9`
CLI from the node-plugin image to a content-addressed host path and keeps CSI
focused on Kubernetes lifecycle, idempotency, mount orchestration, and secret
handling. New mounts require Drive9's direct `mount(2)` path and never fall back
to `fusermount3` or `fusermount`. The Drive9 process owned by systemd is the
in-binary supervisor; its replaceable child owns the FUSE connection.

## Security Model

Put Drive9 credentials in a Kubernetes Secret:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: drive9-workspace-secret
type: Opaque
stringData:
  server: https://api.drive9.ai
  apiKey: drive9_api_key_redacted
```

Create this Secret in the workload namespace before creating the matching PVC.
Each PVC specifies which Secret to use via the `drive9.ai/secret-name`
annotation. The PVC and Secret must be in the same namespace. This lets one
namespace create many PVCs backed by different Drive9 API keys or workspaces
without requiring one cluster-scoped `StorageClass` per workspace.

The optional PVC annotation `drive9.ai/remote-root` can mount an existing
subpath of that workspace instead of `/`. Omit it for the normal workspace-root
behavior.

Credentials are never stored in StorageClass parameters, PV attributes, pod env,
or volume parameters. The driver resolves them at runtime:

- `CreateVolume`: reads PVC annotation → fetches Secret via K8s client
- `NodeStageVolume`: reads Secret reference from PV volumeAttributes (fixated
  during CreateVolume, contains only Secret name/namespace — not the API key)
- `DeleteVolume`: looks up PV by volumeHandle → reads Secret reference from
  volumeAttributes → fetches Secret

If the required `drive9.ai/secret-name` annotation is missing, `CreateVolume`
fails closed with `InvalidArgument` — there is no implicit fallback.

The sidecar fallback necessarily injects the secret into the mounter sidecar
environment. Use CSI for production when the customer can install a node plugin.

The node plugin needs privileged FUSE access, `SYS_ADMIN`, `/dev/fuse`, the host
mount namespace, and host systemd. Treat it like other node storage plugins:
restrict who can modify its DaemonSet and workload namespace Secrets. The image
does not install a FUSE mount helper on the node.

Both the controller and node service accounts need `get` access to Secrets so
the driver can resolve per-PVC credentials at provision and mount time. The
default RBAC does not grant `list` or `watch` beyond what is needed.

The default workspace-root mode does not write CSI metadata into the Drive9
workspace and `DeleteVolume` is a no-op for Drive9 data. If you opt into managed
directory mode with `remoteRootPrefix`, the driver stores CSI metadata under
`/k8s/.drive9-csi/volumes`. A scoped Drive9 token for that mode must cover both
the volume prefix, for example `/k8s/pvc`, and the metadata index path
`/k8s/.drive9-csi/volumes`.

## Install

The checked-in Kubernetes manifests are a fail-closed base. Their CSI image is
`registry.invalid/drive9-csi:unpublished`, so applying the base cannot silently
run an older, incompatible driver. The manually triggered validation-image
workflow resolves the latest complete Drive9 CLI release, builds and pushes the
image, then reports its trace tag, manifest-list digest, and immutable reference
in the workflow summary. It does not generate deployment manifests. Each target
architecture must execute
`drive9 mount --supervise-foreground --direct-mount-strict --help`
successfully during the image build, and its help must expose
`--gvisor-compat`, `--local-only`, `--remote-only`, `--append-log`, and the
corresponding `DRIVE9_MOUNT_*` environment names used by the fallback sidecar. The minimum
compatible version is the first published Drive9 release containing those
contracts; do not use the older `dac2d62` minimum for this driver version. The
runtime image installs neither `fuse3` nor an `/etc/fuse.conf` dependency.
Non-production validation must inject
that immutable reference through a local, environment-specific overlay.

Validation images have a traceable tag:

```text
ghcr.io/drive9-ai/drive9-csi:drive9-<drive9-short-sha>-csi-<csi-short-sha>
```

The publish workflow does not publish `:latest` or `0.1.0`. Use a traceable tag
or a digest.

A manually published image is validation-only and is not release-admitted. Do
not promote it to production until the N/N-1 bidirectional cache/writeback
compatibility gate in the mount-survival design has landed and passed.

To build a validation image with the latest published Drive9 CLI:

```sh
gh workflow run publish-image.yml \
  -R drive9-ai/k8s-csi \
  --ref <csi-branch>
```

To build a local image using the Dockerfile's public-release downloader:

```sh
make image \
  IMAGE=ghcr.io/drive9-ai/drive9-csi:local
```

Install that preloaded local image with the local overlay:

```sh
kubectl apply -k deploy/overlays/local
```

For a published validation image, read the immutable reference from the workflow
summary and use it only in a non-production validation overlay. The tag or
digest is traceability evidence, not release-admission evidence. Do not apply
the fail-closed base directly.

Create a Drive9 Secret in the workload namespace before creating each PVC:

```sh
kubectl -n default create secret generic drive9-workspace-secret \
  --from-literal=server=https://api.drive9.ai \
  --from-literal=apiKey=drive9_api_key_redacted \
  --dry-run=client -o yaml | kubectl apply -f -
```

Create a PVC with the `drive9.ai/secret-name` annotation pointing to the Secret:

```sh
kubectl apply -f deploy/examples/kubernetes/volumeattributesclass.example.yaml
kubectl -n default apply -f deploy/examples/kubernetes/pvc.example.yaml
```

Because the default `StorageClass` uses `WaitForFirstConsumer`, a PVC can remain
`Pending` until the first Pod uses it.

Mount the PVC in a normal workload Pod:

```sh
kubectl -n default apply -f deploy/examples/kubernetes/pod.example.yaml
kubectl -n default wait --for=condition=Ready \
  pod/drive9-workspace-smoke --timeout=180s
kubectl -n default logs drive9-workspace-smoke
kubectl -n default exec drive9-workspace-smoke -- cat /workspace/hello.txt
```

Expected output:

```text
hello-drive9
```

The shared-PVC example is a single-file application bundle containing its own
Namespace, StorageClass, Secret, PVC, writer Deployment, and two writer
replicas. Replace `replace-with-drive9-api-key` in the file, then apply it once:

```sh
kubectl apply -f deploy/examples/kubernetes/shared-pvc.example.yaml
kubectl -n drive9-shared-pvc-example rollout status \
  deployment/drive9-workspace-writer --timeout=180s
kubectl -n drive9-shared-pvc-example exec \
  deployment/drive9-workspace-writer -- ls -1 /workspace
```

Both writer replicas update a file named after their Pod every five seconds.
Kubernetes may place the replicas on the same node or different nodes. The PVC
uses `ReadWriteMany` without RWX-specific mount-parameter defaults; visibility
semantics follow the Drive9 arguments configured by the user. See
`deploy/examples/kubernetes/README.md` for the scenario, resource roles,
credential flow, verification steps, consistency limits, and retention
behavior.

RWX does not provide distributed file locks.
It does not merge concurrent writes to the same file. Applications that require
those semantics must supply their own coordination.

Use the PVC from application Pods the same way:

```yaml
volumeMounts:
  - name: workspace
    mountPath: /workspace
volumes:
  - name: workspace
    persistentVolumeClaim:
      claimName: drive9-workspace
```

Clean up the examples:

```sh
kubectl -n default delete pod drive9-workspace-smoke
kubectl -n default delete pvc drive9-workspace-tuned
kubectl delete -f deploy/examples/kubernetes/volumeattributesclass.example.yaml
kubectl delete -f deploy/examples/kubernetes/shared-pvc.example.yaml
```

The default example `StorageClass` uses `Retain`, so deleting the PVC keeps the
PV and Drive9 workspace data for safety. Even with `reclaimPolicy: Delete`, the
default workspace-root mode does not delete Drive9 workspace data.

Example StorageClass, VolumeAttributesClass, Secret, PVC, and workload manifests
live under `deploy/examples/kubernetes/` so that applying `deploy/kubernetes/`
does not create placeholder credentials or demo workloads in production
clusters.
Apply the example Secret with
`kubectl -n <workload-namespace> apply -f deploy/examples/kubernetes/secret.example.yaml`
after replacing the API key. Each PVC references its Secret via the
`drive9.ai/secret-name` annotation — multiple PVCs can share a Secret or use
different ones.

## StorageClass and VolumeAttributesClass

The checked-in manifests separate provisioning identity from mount behavior.
The `drive9-rwo` and `drive9-rwx` StorageClasses have empty parameters:

```yaml
provisioner: csi.drive9.ai
parameters: {}
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: false
```

PVCs opt into mount behavior with `spec.volumeAttributesClassName`. The base
installs the optional `drive9-coding-agent` VolumeAttributesClass; the separate
tuned example installs `drive9-coding-agent-tuned`:

```yaml
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: drive9-coding-agent-tuned
driverName: csi.drive9.ai
parameters:
  profile: coding-agent
  gvisorCompat: "false"
  attrTTL: 30s
  entryTTL: 30s
  dirTTL: 30s
  perfEnabled: "false"
  readdirPrefetch: "true"
  readdirPrefetchMaxFiles: "64"
  readdirPrefetchMaxFileBytes: "50000"
  readdirPrefetchMaxBytes: "4194304"
  writebackBatchWindow: 20ms
```

```yaml
spec:
  storageClassName: drive9-rwo
  volumeAttributesClassName: drive9-coding-agent-tuned
```

StorageClass mount parameters remain supported for compatibility. At volume
creation, VolumeAttributesClass parameters override matching StorageClass
parameters. The driver stores the effective values in PV `volumeAttributes` so
the node uses a fixed mount configuration for that volume.

This VAC support is creation-time only. The driver advertises CSI
`MODIFY_VOLUME` because external-provisioner requires that capability when
provisioning a PVC with a VAC. After validating the requested keys and values,
`ControllerModifyVolume` returns `Unimplemented` for a valid dynamic update.
Changing `spec.volumeAttributesClassName` on an existing PVC does not remount or
reconfigure it; recreate the volume to apply different mount parameters.

Parameter ownership is:

| Parameter | Preferred source | Behavior |
| --------- | ---------------- | -------- |
| `remoteRootPrefix` | StorageClass | Creates a CSI-managed directory and affects volume identity |
| `remoteRoot` | PVC annotation; legacy StorageClass fallback | Selects an existing workspace path and affects volume identity |
| `profile`, `durability` | VolumeAttributesClass | Forwarded to `drive9 mount` when explicitly set |
| `gvisorCompat` | VolumeAttributesClass | Strict boolean; always forwarded as `--gvisor-compat=<true|false>` and defaults to `false` |
| `localOnlyPatterns` | VolumeAttributesClass | Newline-delimited additional local-only rules |
| `remoteOnlyPatterns` | VolumeAttributesClass | Newline-delimited remote-persistent overrides; wins over local routing |
| `appendLogPatterns` | VolumeAttributesClass | Newline-delimited append-log optimization rules for remote-persistent files; empty by default |
| `attrTTL`, `entryTTL`, `dirTTL` | VolumeAttributesClass | Positive Go durations; each defaults to `30s` |
| `perfEnabled` | VolumeAttributesClass | Boolean; defaults to `false` |
| Read-directory and writeback tuning | VolumeAttributesClass | Optional; no CSI defaults |

Credentials are not valid StorageClass or VolumeAttributesClass parameters.
They are resolved from the PVC's `drive9.ai/secret-name` annotation, which
keeps Secret binding explicit and auditable.

`Retain` is the default because this is customer data. To use managed-directory
mode, create a separate StorageClass containing only the identity parameter:

```yaml
parameters:
  remoteRootPrefix: /k8s/pvc
```

In managed-directory mode, `CreateVolume` creates a unique child directory
under that prefix and writes CSI metadata. Even with `reclaimPolicy: Delete`,
the driver removes only the metadata and refuses cleanup without both a matching
index entry and `.drive9-csi-volume.json` marker. It never deletes user data.

The optional `attrTTL`, `entryTTL`, and `dirTTL` values control the matching
`drive9 mount --attr-ttl`, `--entry-ttl`, and `--dir-ttl` flags. Each uses Go
duration syntax, for example `500ms`, `1s`, `30s`, or `2m`.

`gvisorCompat` and the three pattern lists are fixed per volume. The driver
normalizes each policy list by trimming lines, removing blanks, and preserving
the first occurrence of exact duplicates. It persists the canonical values in
PV `volumeAttributes`, then reconstructs deterministic mount arguments during
staging and recovery. Native CSI users configure these options through a VAC;
the CSI Node DaemonSet does not forward the matching `DRIVE9_MOUNT_*`
environment variables.

`localOnlyPatterns` and `remoteOnlyPatterns` require an overlay profile;
`profile: none` and `profile: interactive` are rejected when either routing
list is non-empty. `appendLogPatterns` alone also supports those two profiles.
The combined raw values of all three lists and their expanded mount arguments
are each limited to 64 KiB; their JSON-encoded arguments are limited to 256 KiB.
Oversized policies are rejected during volume creation before the driver
accesses the PVC or creates remote volume metadata.

For example, apply
`deploy/examples/kubernetes/volumeattributesclass-path-policy.example.yaml`
and reference `drive9-gvisor-persistent-tmp` from a new PVC. Its effective
arguments include:

```text
--gvisor-compat=true
--local-only=**/local-build-cache/**
--remote-only=**/tmp/**
--remote-only=**/.tmp/**
```

The equals form keeps each pattern in one argv entry. A local/remote overlap is
valid; Drive9 applies remote-only precedence. An explicit empty VAC policy value
clears the complete matching legacy StorageClass value.

To opt into append-log, apply
`deploy/examples/kubernetes/volumeattributesclass-append-log.example.yaml`
and reference `drive9-append-log` from a new PVC. The example uses
`profile: none` and emits `--append-log=data/app.db-wal` and
`--append-log=logs/events.log`. Patterns refer to paths inside the mounted
filesystem, not the host mountpoint or a path prefixed with `remoteRoot`.

Append-log does not change local/remote routing or require a WAL filename
suffix. A matching local-only file stays local. Drive9 decides whether a
remote-persistent file and its writes qualify for the optimization, including
server capability checks and fallback behavior. Empty lists emit no append-log
flags. Changing an existing PVC's VAC remains unsupported; recreate the volume
to apply different patterns.

When `perfEnabled` is `"true"`, `NodeStageVolume` passes `--perf-dir` with a
driver-generated path under `/var/lib/drive9-csi/perf/<volume-id>`. The driver
does not accept a user-provided perf path or automatically delete perf output.

The optional tuning parameters have no CSI defaults:

| Parameter                     | `drive9 mount` flag                 |
| ----------------------------- | ----------------------------------- |
| `readdirPrefetch`             | `--readdir-prefetch`                |
| `readdirPrefetchMaxFiles`     | `--readdir-prefetch-max-files`      |
| `readdirPrefetchMaxFileBytes` | `--readdir-prefetch-max-file-bytes` |
| `readdirPrefetchMaxBytes`     | `--readdir-prefetch-max-bytes`      |
| `writebackBatchWindow`        | `--writeback-batch-window`          |

`readdirPrefetch` accepts `"true"` or `"false"`. Integer values must be
positive. `writebackBatchWindow` uses a positive Go duration such as `20ms` and
requires a compatible Drive9 CLI.

If you use `reclaimPolicy: Delete`, keep the per-PVC workload namespace Secret
until Kubernetes has deleted the PV. If the Secret is removed first,
`DeleteVolume` cannot resolve credentials and metadata cleanup requires manual
intervention.

## Sidecar Fallback

If a customer cannot install a CSI driver yet, use
`deploy/sidecar/deployment.yaml` as a fallback. The checked-in manifest uses the
fail-closed `registry.invalid/drive9-csi:unpublished` image; override it with a
strict-capable immutable image reference before applying it. It runs a
privileged Drive9 mounter sidecar and exposes the mounted directory to the app
container.

This is less clean than CSI because it requires privileged pods and hostPath
mount propagation. Use it for pilots or constrained clusters, not as the default
production path. The fallback example also mounts the Drive9 workspace root by
default; set `DRIVE9_REMOTE_ROOT` only when a subpath is intentional.

The sidecar uses the same fixed
`--supervise-foreground --direct-mount-strict --allow-other` contract. Drive9's
in-binary supervisor restarts an unhealthy FUSE worker and performs bounded
TERM/KILL escalation and mount cleanup. Its 30-second stop timeout leaves
cleanup headroom inside the Pod's 60-second termination grace period.

The sidecar fallback exposes the same mount TTL behavior through
`DRIVE9_ATTR_TTL`, `DRIVE9_ENTRY_TTL`, and `DRIVE9_DIR_TTL`. Each defaults to
`30s` and maps to the corresponding `drive9 mount` flag.

Set `DRIVE9_PERF_ENABLED` to `"true"` to enable `drive9 mount --perf-dir` in the
sidecar fallback. The path is fixed to `/perf`; use a Kubernetes volume mount to
choose where `/perf` is stored. The example manifest mounts it from
`/var/lib/drive9-sidecar/demo/perf`.

The sidecar fallback also supports explicit mount-option and tuning environment
variables. Compatibility defaults to `false`; policy and tuning values default
to empty:

<!-- markdownlint-disable MD013 -->

| Environment variable                     | `drive9 mount` flag                 |
| ---------------------------------------- | ----------------------------------- |
| `DRIVE9_MOUNT_GVISOR_COMPAT`             | `--gvisor-compat`                   |
| `DRIVE9_MOUNT_LOCAL_ONLY_PATTERNS`        | Repeated `--local-only` rules       |
| `DRIVE9_MOUNT_REMOTE_ONLY_PATTERNS`       | Repeated `--remote-only` rules      |
| `DRIVE9_MOUNT_APPEND_LOG_PATTERNS`        | Repeated `--append-log` rules       |
| `DRIVE9_READDIR_PREFETCH`                | `--readdir-prefetch`                |
| `DRIVE9_READDIR_PREFETCH_MAX_FILES`      | `--readdir-prefetch-max-files`      |
| `DRIVE9_READDIR_PREFETCH_MAX_FILE_BYTES` | `--readdir-prefetch-max-file-bytes` |
| `DRIVE9_READDIR_PREFETCH_MAX_BYTES`      | `--readdir-prefetch-max-bytes`      |
| `DRIVE9_WRITEBACK_BATCH_WINDOW`          | `--writeback-batch-window`          |

<!-- markdownlint-enable MD013 -->

Example:

```yaml
env:
  - name: DRIVE9_MOUNT_GVISOR_COMPAT
    value: "true"
  - name: DRIVE9_MOUNT_REMOTE_ONLY_PATTERNS
    value: |-
      **/tmp/**
      **/.tmp/**
  - name: DRIVE9_PERF_ENABLED
    value: "true"
  - name: DRIVE9_READDIR_PREFETCH
    value: "true"
  - name: DRIVE9_READDIR_PREFETCH_MAX_FILES
    value: "64"
  - name: DRIVE9_READDIR_PREFETCH_MAX_FILE_BYTES
    value: "50000"
  - name: DRIVE9_READDIR_PREFETCH_MAX_BYTES
    value: "4194304"
  - name: DRIVE9_WRITEBACK_BATCH_WINDOW
    value: 20ms
```

Create the sidecar Secret in the target namespace before applying the fallback
deployment:

```sh
kubectl create secret generic drive9-sidecar-secret \
  --from-literal=server=https://api.drive9.ai \
  --from-literal=apiKey=drive9_api_key_redacted \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -k deploy/sidecar
```

The sidecar Secret example lives under `deploy/examples/sidecar/` and is
intentionally not part of the sidecar kustomization.

## Perf Support Bundle Upload

When `perfEnabled` is `"true"`, the CSI node plugin writes `drive9 mount`
profiling output under:

```text
/var/lib/drive9-csi/perf/<volume-id>/
```

CSI does not upload or delete this data automatically. To send it to Drive9
support, use the `drive9` CLI already present in the node plugin container with
a short-lived support upload token provided by Drive9 support. Do not use the
workload Drive9 API key for this upload.

Find the node plugin pod on the target node:

```sh
kubectl -n drive9-csi get pods -l app=drive9-csi-node -o wide
```

Run the helper inside the `drive9-csi` container:

```sh
kubectl -n drive9-csi exec -it <drive9-csi-node-pod> -c drive9-csi -- \
  drive9-csi-upload-perf --case-id <case-id>
```

The helper prompts for the support upload token without echoing it, creates
`/var/lib/drive9-csi/perf/<case-id>.tgz`, uploads it to:

```text
:/support-inbox/<case-id>/<node-name>/<volume-id>.tgz
```

and verifies the uploaded bundle with `drive9 fs stat`. If more than one perf
volume directory exists, rerun with `--volume-id <volume-id>`.

For non-interactive use, pass the token on stdin:

```sh
printf '%s' "${DRIVE9_SUPPORT_UPLOAD_TOKEN}" | \
  kubectl -n drive9-csi exec -i <drive9-csi-node-pod> -c drive9-csi -- \
    drive9-csi-upload-perf --case-id <case-id> --token-stdin
```

Do not pass the support upload token as a command-line argument. The helper uses
the token only for the upload and verification commands.

If the helper is unavailable, create a bundle manually inside the `drive9-csi`
container:

```sh
kubectl -n drive9-csi exec <drive9-csi-node-pod> -c drive9-csi -- \
  tar czf /var/lib/drive9-csi/perf/<case-id>.tgz \
    -C /var/lib/drive9-csi/perf <volume-id>
```

Then upload and verify the bundle with the support-owned Drive9 token:

```sh
kubectl -n drive9-csi exec <drive9-csi-node-pod> -c drive9-csi -- \
  env DRIVE9_SERVER=https://api.drive9.ai \
    DRIVE9_API_KEY="${DRIVE9_SUPPORT_UPLOAD_TOKEN}" \
    drive9 fs cp /var/lib/drive9-csi/perf/<case-id>.tgz \
      :/support-inbox/<case-id>/<node-name>/<volume-id>.tgz \
      --tag case=<case-id> \
      --tag source=k8s-csi \
      --description "Drive9 CSI perf bundle"
```

```sh
kubectl -n drive9-csi exec <drive9-csi-node-pod> -c drive9-csi -- \
  env DRIVE9_SERVER=https://api.drive9.ai \
    DRIVE9_API_KEY="${DRIVE9_SUPPORT_UPLOAD_TOKEN}" \
    drive9 fs stat :/support-inbox/<case-id>/<node-name>/<volume-id>.tgz
```

For sidecar fallback, perf output is written to `/perf` in the mounter container
when `DRIVE9_PERF_ENABLED` is `"true"`. Bundle and upload the mounted `/perf`
directory with the same support upload token flow.

After support confirms receipt, remove the local bundle and perf directory
manually:

```sh
kubectl -n drive9-csi exec <drive9-csi-node-pod> -c drive9-csi -- \
  rm -rf /var/lib/drive9-csi/perf/<volume-id> \
    /var/lib/drive9-csi/perf/<case-id>.tgz
```

## Limitations

- Linux only.
- `ReadWriteOnce` by default. `SINGLE_NODE_MULTI_WRITER` supported for same-node
  multi-pod access.
- `ReadWriteMany` does not add `profile` or `durability` defaults or validation.
- One Drive9 principal per mounted volume lifecycle.
- No volume expansion or quota enforcement.
- RWX does not provide distributed file locks, same-file merge, byte-range
  write ordering, or a strong POSIX/database-workload guarantee.
- Cross-node visibility may require bounded polling.
- `drive9 mount` must be present in the driver image.

## Tests

```sh
make test             # production Go unit tests
make build-check      # binary/ELF acceptance
make manifest-check   # deployment manifest contracts
make script-check     # shell helper behavior
make e2e-check        # non-mutating E2E safety checks
make check            # complete local validation
```

## Real Kubernetes E2E Gate

Static checks are not enough for customer distribution. Before calling a build
production-safe, run a real Kubernetes cluster against a real Drive9 server and
a pre-provisioned Secret containing its API key:

```sh
export DRIVE9_CSI_E2E_CONTEXT=dev-dat9-eks-ap-southeast-1
export DRIVE9_CSI_E2E_DRIVER_NAMESPACE=drive9-csi
export DRIVE9_CSI_E2E_SECRET_NAME=drive9-csi-secret-flags-test
export DRIVE9_CSI_E2E_CONFIRM=1
e2e/prepare.sh --image-tag drive9-a53e497-csi-d91bfe3

e2e/basic-lifecycle.sh
e2e/mount-survival.sh
e2e/multi-node-rwx.sh
```

`prepare.sh` idempotently creates or updates the persistent Driver environment
with the completed publishing workflow's bare trace tag. Cases can then run
repeatedly against that environment. They reuse a pre-provisioned namespace and
Secret, create and clean only their own StorageClass, VolumeAttributesClass,
PVC, and Pod resources, and never delete the prepared Driver.

All four scripts require explicit context and Driver namespace values. The
current kubectl context is never used implicitly, and production-like context
names are rejected.

`basic-lifecycle.sh` covers provisioning, mount/write/read, readonly Pod access
where reads succeed and writes are denied, workload Pod remount, same-node
multi-Pod access, one-Pod multi-PVC behavior, unpublish, unstage, and deletion.
`mount-survival.sh` keeps workload I/O active while
replacing the matching CSI node Pod and verifies the host mount identity does
not change. `multi-node-rwx.sh` requires two eligible nodes, schedules one
writer on each, verifies cross-node visibility through one RWX PVC, and checks
that deleting one writer does not interrupt the other. See `e2e/README.md` and
`e2e/AGENTS.md` for the complete safety and execution contract.

## Multiple Workspaces per Namespace

Each PVC maps to exactly one Drive9 workspace via its `drive9.ai/secret-name`
annotation. Different PVCs can point to different Secrets (and therefore
different API keys / workspaces) without requiring multiple StorageClasses.

To mount multiple workspaces in one namespace, create one Secret + PVC pair per
workspace:

```sh
# Secret for workspace A
kubectl -n myapp create secret generic secret-workspace-a \
  --from-literal=server=https://api.drive9.ai \
  --from-literal=apiKey=<api-key-for-workspace-a>

cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: workspace-a
  namespace: myapp
  annotations:
    drive9.ai/secret-name: secret-workspace-a
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: drive9-rwo
  resources:
    requests:
      storage: 1Gi
EOF

# Secret for workspace B
kubectl -n myapp create secret generic secret-workspace-b \
  --from-literal=server=https://api.drive9.ai \
  --from-literal=apiKey=<api-key-for-workspace-b>

cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: workspace-b
  namespace: myapp
  annotations:
    drive9.ai/secret-name: secret-workspace-b
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: drive9-rwo
  resources:
    requests:
      storage: 1Gi
EOF
```

Multiple PVCs can also share the same Secret if they use the same API key.

A single pod can mount both PVCs:

```yaml
containers:
  - name: app
    volumeMounts:
      - name: ws-a
        mountPath: /workspace-a
      - name: ws-b
        mountPath: /workspace-b
volumes:
  - name: ws-a
    persistentVolumeClaim:
      claimName: workspace-a
  - name: ws-b
    persistentVolumeClaim:
      claimName: workspace-b
```

## Troubleshooting

**PVC stuck in Pending with `failed to provision volume` or
`missing required annotation`:**

The driver reads the `drive9.ai/secret-name` annotation from the PVC. If your
PVC does not have this annotation, `CreateVolume` will reject it. Add the
annotation:

```yaml
metadata:
  annotations:
    drive9.ai/secret-name: my-drive9-secret
```

Then make sure the named Secret exists in the same namespace:

```sh
kubectl -n <namespace> create secret generic my-drive9-secret \
  --from-literal=server=https://api.drive9.ai \
  --from-literal=apiKey=<your-api-key>
```

**Multiple PVCs sharing the same API key:**

Multiple PVCs can reference the same Secret by name. Just set the same
`drive9.ai/secret-name` annotation value on each PVC. No per-PVC Secret naming
convention is required.

## GHCR Visibility

The image package must be public before customers can pull it without a GitHub
token. After the first successful publish, open:

```text
https://github.com/orgs/drive9-ai/packages/container/package/drive9-csi
```

Then use package settings to change the package visibility to public. Verify
anonymous access against the published trace tag or digest before distributing
the release manifests.
