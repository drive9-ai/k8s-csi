# Drive9 CSI Lite

This repository provides a minimal Kubernetes integration for `github.com/mem9-ai/drive9`.

It intentionally ships a small stable surface first:

- PVCs mount the Drive9 workspace root selected by the per-PVC API key by default.
- Optional managed directory volumes backed by Drive9 remote paths.
- `ReadWriteOnce` by default. `SINGLE_NODE_MULTI_WRITER` supported for same-node multi-pod access.
- Credentials are resolved from PVC annotation `drive9.ai/secret-name` → Kubernetes Secret.
- Default workspace-root volumes do not create or delete Drive9 workspace data.
- Managed directory volumes write a marker file.
- `DeleteVolume` detaches CSI ownership only: it removes CSI metadata (marker, index, name index) but never deletes Drive9 workspace data.
- `NodeStageVolume` runs `drive9 mount --mode=fuse`.
- `NodePublishVolume` bind-mounts the staged path into the pod.
- No snapshots, expansion, RWX, or automatic tenant provisioning.

## Why CSI Lite

Customers usually already run business workloads in Kubernetes pods and want normal PVCs. CSI Lite gives them that path without requiring application-level changes.

The driver does not reimplement Drive9 FUSE. It uses the official `drive9` CLI inside the node plugin image and keeps CSI focused on Kubernetes lifecycle, idempotency, mount orchestration, and secret handling.

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

The sidecar fallback necessarily injects the secret into the mounter sidecar environment. Use CSI for production when the customer can install a node plugin.

The node plugin needs privileged FUSE access. Treat it like other node storage plugins: restrict who can modify its DaemonSet and workload namespace Secrets.

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
run an older, incompatible driver. The manually triggered publish workflow
resolves the latest complete Drive9 CLI release, builds and pushes the image,
then reports its trace tag, manifest-list digest, and immutable reference in the
workflow summary. It does not generate deployment manifests.
Deployment and validation must inject that immutable reference through a local,
environment-specific overlay.

Release images also have a traceable tag:

```text
ghcr.io/drive9-ai/drive9-csi:drive9-<drive9-short-sha>-csi-<csi-short-sha>
```

The publish workflow does not publish `:latest` or `0.1.0`. Use a traceable tag
or a digest.

To build a validation image with the latest published Drive9 CLI:

```sh
gh workflow run publish-image.yml \
  -R drive9-ai/k8s-csi \
  --ref <csi-branch>
```

To build a local image from the same immutable Drive9 CLI release:

```sh
make image \
  IMAGE=ghcr.io/drive9-ai/drive9-csi:local \
  DRIVE9_CLI_RELEASE_COMMIT=<full-drive9-fe-commit>
```

Install that preloaded local image with the local overlay:

```sh
kubectl apply -k deploy/overlays/local
```

For a published image, read the immutable reference from the workflow summary
and use it in the local validation or deployment overlay. Do not apply the
fail-closed base directly.

Create a Drive9 Secret in the workload namespace before creating each PVC:

```sh
kubectl -n default create secret generic drive9-workspace-secret \
  --from-literal=server=https://api.drive9.ai \
  --from-literal=apiKey=drive9_api_key_redacted \
  --dry-run=client -o yaml | kubectl apply -f -
```

Create a PVC with the `drive9.ai/secret-name` annotation pointing to the Secret:

```sh
kubectl apply -f deploy/examples/kubernetes/pvc.example.yaml
```

Because the default `StorageClass` uses `WaitForFirstConsumer`, a PVC can remain `Pending` until the first Pod uses it.

Mount the PVC in a normal workload Pod:

```sh
kubectl apply -f deploy/examples/kubernetes/pod.example.yaml
kubectl wait --for=condition=Ready pod/drive9-workspace-smoke --timeout=180s
kubectl logs drive9-workspace-smoke
kubectl exec drive9-workspace-smoke -- cat /workspace/hello.txt
```

Expected output:

```text
hello-drive9
```

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

Clean up the smoke workload:

```sh
kubectl delete pod drive9-workspace-smoke
kubectl delete pvc drive9-workspace
```

The default example `StorageClass` uses `Retain`, so deleting the PVC keeps the
PV and Drive9 workspace data for safety. Even with `reclaimPolicy: Delete`, the
default workspace-root mode does not delete Drive9 workspace data.

Example StorageClass, Secret, PVC, and smoke Pod manifests live under `deploy/examples/kubernetes/` so that applying `deploy/kubernetes/` does not create placeholder credentials or demo workloads in production clusters. Apply the example Secret with `kubectl -n <workload-namespace> apply -f deploy/examples/kubernetes/secret.example.yaml` after replacing the API key. Each PVC references its Secret via the `drive9.ai/secret-name` annotation — multiple PVCs can share a Secret or use different ones.

## StorageClass

Default example:

```yaml
provisioner: csi.drive9.ai
parameters:
  profile: coding-agent
  attrTTL: 30s
  entryTTL: 30s
  dirTTL: 30s
  perfEnabled: "false"
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
```

The StorageClass does **not** contain any secret template parameters. Credentials
are resolved from PVC annotations, not StorageClass templates. This avoids the
implicit `drive9-csi-${pvc.name}` naming convention and makes the Secret binding
explicit and auditable.

`Retain` is the default example because this is customer data. If a customer
wants a PVC to mount a CSI-managed subdirectory instead of the Drive9 workspace
root, create a separate `StorageClass` with `remoteRootPrefix`:

```yaml
parameters:
  remoteRootPrefix: /k8s/pvc
  profile: coding-agent
  attrTTL: 30s
  entryTTL: 30s
  dirTTL: 30s
  perfEnabled: "false"
```

In managed directory mode, `CreateVolume` creates a unique child directory under
that prefix and writes CSI metadata. If that separate `StorageClass` uses
`reclaimPolicy: Delete`, the driver still refuses to delete paths without both a
matching metadata index entry and a matching `.drive9-csi-volume.json` marker.

The optional `attrTTL`, `entryTTL`, and `dirTTL` parameters control the matching
`drive9 mount --attr-ttl`, `--entry-ttl`, and `--dir-ttl` flags. Each value uses
Go duration syntax, for example `500ms`, `1s`, `30s`, or `2m`. If omitted, the
CSI driver defaults each value to `30s`.

The optional `perfEnabled` parameter defaults to `"false"`. When set to
`"true"`, `NodeStageVolume` passes `--perf-dir` with a driver-generated path
under `/var/lib/drive9-csi/perf/<volume-id>`. The driver does not accept a
user-provided perf path and does not automatically delete perf output. Remove
the perf directory manually after collecting support data.

The following optional tuning parameters have no CSI defaults. The driver passes
only values explicitly set in the `StorageClass`:

| StorageClass parameter | `drive9 mount` flag |
| --- | --- |
| `readdirPrefetch` | `--readdir-prefetch` |
| `readdirPrefetchMaxFiles` | `--readdir-prefetch-max-files` |
| `readdirPrefetchMaxFileBytes` | `--readdir-prefetch-max-file-bytes` |
| `readdirPrefetchMaxBytes` | `--readdir-prefetch-max-bytes` |
| `writebackBatchWindow` | `--writeback-batch-window` |

`readdirPrefetch` accepts `"true"` or `"false"`. The integer values must be
positive. `writebackBatchWindow` uses Go duration syntax, for example `20ms`.
The `--writeback-batch-window` flag requires a `drive9` CLI version that
supports it.

Example with explicit tuning enabled:

```yaml
parameters:
  profile: coding-agent
  attrTTL: 30s
  entryTTL: 30s
  dirTTL: 30s
  perfEnabled: "true"
  readdirPrefetch: "true"
  readdirPrefetchMaxFiles: "64"
  readdirPrefetchMaxFileBytes: "50000"
  readdirPrefetchMaxBytes: "4194304"
  writebackBatchWindow: 20ms
```

If you use `reclaimPolicy: Delete`, keep the per-PVC workload namespace Secret in
place until Kubernetes has deleted the PV. If the Secret is removed first,
`DeleteVolume` cannot resolve credentials and backend cleanup will require manual
intervention.

## Sidecar Fallback

If a customer cannot install a CSI driver yet, use `deploy/sidecar/deployment.yaml` as a fallback. It runs a privileged Drive9 mounter sidecar and exposes the mounted directory to the app container.

This is less clean than CSI because it requires privileged pods and hostPath mount propagation. Use it for pilots or constrained clusters, not as the default production path. The fallback example also mounts the Drive9 workspace root by default; set `DRIVE9_REMOTE_ROOT` only when a subpath is intentional.

The sidecar fallback exposes the same mount TTL behavior through
`DRIVE9_ATTR_TTL`, `DRIVE9_ENTRY_TTL`, and `DRIVE9_DIR_TTL`. Each defaults to
`30s` and maps to the corresponding `drive9 mount` flag.

Set `DRIVE9_PERF_ENABLED` to `"true"` to enable `drive9 mount --perf-dir` in the
sidecar fallback. The path is fixed to `/perf`; use a Kubernetes volume mount to
choose where `/perf` is stored. The example manifest mounts it from
`/var/lib/drive9-sidecar/demo/perf`.

The sidecar fallback also supports explicit mount tuning env vars with no
defaults:

| Environment variable | `drive9 mount` flag |
| --- | --- |
| `DRIVE9_READDIR_PREFETCH` | `--readdir-prefetch` |
| `DRIVE9_READDIR_PREFETCH_MAX_FILES` | `--readdir-prefetch-max-files` |
| `DRIVE9_READDIR_PREFETCH_MAX_FILE_BYTES` | `--readdir-prefetch-max-file-bytes` |
| `DRIVE9_READDIR_PREFETCH_MAX_BYTES` | `--readdir-prefetch-max-bytes` |
| `DRIVE9_WRITEBACK_BATCH_WINDOW` | `--writeback-batch-window` |

Example:

```yaml
env:
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

Create the sidecar Secret in the target namespace before applying the fallback deployment:

```sh
kubectl create secret generic drive9-sidecar-secret \
  --from-literal=server=https://api.drive9.ai \
  --from-literal=apiKey=drive9_api_key_redacted \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -k deploy/sidecar
```

The sidecar Secret example lives under `deploy/examples/sidecar/` and is intentionally not part of the sidecar kustomization.

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

For sidecar fallback, perf output is written to `/perf` in the mounter
container when `DRIVE9_PERF_ENABLED` is `"true"`. Bundle and upload the mounted
`/perf` directory with the same support upload token flow.

After support confirms receipt, remove the local bundle and perf directory
manually:

```sh
kubectl -n drive9-csi exec <drive9-csi-node-pod> -c drive9-csi -- \
  rm -rf /var/lib/drive9-csi/perf/<volume-id> \
    /var/lib/drive9-csi/perf/<case-id>.tgz
```

## Limitations

- Linux only.
- `ReadWriteOnce` by default. `SINGLE_NODE_MULTI_WRITER` supported for same-node multi-pod access.
- One Drive9 principal per mounted volume lifecycle.
- No volume expansion or quota enforcement.
- No cross-node cache-consistency guarantee.
- `drive9 mount` must be present in the driver image.

## Tests

```sh
make test
```

## Real Kubernetes E2E Gate

Static checks are not enough for customer distribution. Before calling a build
production-safe, run a real Kubernetes cluster against a real Drive9 server and
API key:

```sh
export DRIVE9_SERVER=https://api.drive9.ai
export DRIVE9_API_KEY=drive9_api_key_redacted
export DRIVE9_CSI_IMAGE='ghcr.io/drive9-ai/drive9-csi@sha256:<released-manifest-digest>'
export DRIVE9_CSI_E2E_CONFIRM=1
hack/e2e-k8s.sh
```

The script intentionally requires `DRIVE9_CSI_E2E_CONFIRM=1` because it mutates
the current Kubernetes context. It deploys the CSI driver into an isolated
`drive9-csi-e2e-driver` namespace, creates a Secret in `drive9-csi-e2e`, creates
PVC `drive9-workspace-e2e` with the `drive9.ai/secret-name` annotation, mounts
it into one pod, writes and reads a file, deletes
that pod, remounts the same PVC into a second pod, reads the same token again,
then runs a multi-pod same-node concurrent test: keeps the second pod running,
launches a third pod pinned to the same node, verifies cross-pod read, deletes
the second pod, and confirms the third pod still works. After the multi-pod
test, it runs a one-pod multi-PVC test: creates a second PVC with its own
Secret, launches a single pod mounting both PVCs, and validates mode-specific
behavior — in workspace-root mode (default) it writes through one mount and
reads through the other to verify cross-PVC visibility; in managed-directory
mode it asserts isolation (each PVC's files are not visible through the other
mount). It then cleans up the second PVC. Finally it deletes the first PVC
and PV, recreates the PVC, reads
the same token from the Drive9 workspace root, removes the temporary token
file, then deletes the pod and PVC.
It fails closed if either e2e namespace or cluster-scoped CSI resources already
exist, because the e2e should run on a clean validation cluster. Set
`DRIVE9_REMOTE_ROOT_PREFIX=/k8s/pvc-e2e` only when explicitly testing managed
directory mode. Do not use `:latest` for `DRIVE9_CSI_IMAGE`; use an immutable tag
or digest for customer evidence.

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

**PVC stuck in Pending with `failed to provision volume` or `missing required annotation`:**

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
