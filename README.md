# Drive9 CSI Lite

This repository provides a minimal Kubernetes integration for `github.com/mem9-ai/drive9`.

It intentionally ships a small stable surface first:

- PVCs mount the Drive9 workspace root selected by the per-PVC API key by default.
- Optional managed directory volumes backed by Drive9 remote paths.
- `ReadWriteOnce` by default. `SINGLE_NODE_MULTI_WRITER` supported for same-node multi-pod access.
- API key passed through Kubernetes CSI Secrets only.
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
  name: drive9-csi-drive9-workspace
type: Opaque
stringData:
  server: https://api.drive9.ai
  apiKey: drive9_api_key_redacted
```

Create this Secret in the workload namespace before creating the matching PVC.
With the default `StorageClass`, a PVC named `drive9-workspace` uses Secret
`drive9-csi-drive9-workspace` in the same namespace and mounts the root of the
Drive9 workspace selected by that API key. This lets one namespace create many
PVCs backed by different Drive9 API keys or workspaces without requiring one
cluster-scoped `StorageClass` per workspace.

The optional Secret key `remoteRoot` can mount an existing subpath of that
workspace instead of `/`. Omit it for the normal workspace-root behavior.

For the CSI path, do not put API keys in `StorageClass.parameters`, PV attributes, pod env, or annotations. The example `StorageClass` uses CSI secret references so Kubernetes passes the namespace-local secret to `CreateVolume`, `DeleteVolume`, and `NodeStageVolume`.

The sidecar fallback necessarily injects the secret into the mounter sidecar environment. Use CSI for production when the customer can install a node plugin.

The node plugin needs privileged FUSE access. Treat it like other node storage plugins: restrict who can modify its DaemonSet and workload namespace Secrets.

The controller service account needs `get` access to Secrets so the CSI
provisioner can resolve per-PVC credentials. Kubernetes RBAC cannot constrain
`resourceNames` with the `drive9-csi-${pvc.name}` template, so the default RBAC
does not grant `list` or `watch`, but it also cannot restrict reads to one fixed
Secret name.

The default workspace-root mode does not write CSI metadata into the Drive9
workspace and `DeleteVolume` is a no-op for Drive9 data. If you opt into managed
directory mode with `remoteRootPrefix`, the driver stores CSI metadata under
`/k8s/.drive9-csi/volumes`. A scoped Drive9 token for that mode must cover both
the volume prefix, for example `/k8s/pvc`, and the metadata index path
`/k8s/.drive9-csi/volumes`.

## Install

The default manifests use the public customer image:

```text
ghcr.io/drive9-ai/drive9-csi:drive9-cbf73aa-csi-a163416
```

Release images are also published with a traceable tag:

```text
ghcr.io/drive9-ai/drive9-csi:drive9-<drive9-short-sha>-csi-<csi-short-sha>
```

For example, the current default image was built from Drive9 CLI commit
`cbf73aa...` and CSI commit `a163416...`:

```text
ghcr.io/drive9-ai/drive9-csi:drive9-cbf73aa-csi-a163416
```

The publish workflow does not publish `:latest` or `0.1.0`. Use a traceable tag
or a digest.

To build and publish your own image instead:

```sh
make image IMAGE=ghcr.io/drive9-ai/drive9-csi:drive9-cbf73aa-csi-a163416
docker push ghcr.io/drive9-ai/drive9-csi:drive9-cbf73aa-csi-a163416
```

Install the CSI driver:

```sh
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl apply -k deploy/kubernetes
```

Create a Drive9 Secret in the workload namespace before creating each PVC. The
default Secret name is `drive9-csi-<pvc-name>`. The API key selects the Drive9
workspace, and the PVC mounts that workspace root:

```sh
kubectl -n default create secret generic drive9-csi-drive9-workspace \
  --from-literal=server=https://api.drive9.ai \
  --from-literal=apiKey=drive9_api_key_redacted \
  --dry-run=client -o yaml | kubectl apply -f -
```

Create a PVC:

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

Example Secret, PVC, and smoke Pod manifests live under `deploy/examples/kubernetes/` so that applying `deploy/kubernetes/` does not create placeholder credentials or demo workloads in production clusters. Apply the example Secret with `kubectl -n <workload-namespace> apply -f deploy/examples/kubernetes/secret.example.yaml` after replacing the API key. For another PVC, copy the Secret and name it `drive9-csi-<pvc-name>`.

## StorageClass

Default example:

```yaml
provisioner: csi.drive9.ai
parameters:
  profile: coding-agent
  csi.storage.k8s.io/provisioner-secret-name: drive9-csi-${pvc.name}
  csi.storage.k8s.io/provisioner-secret-namespace: ${pvc.namespace}
  csi.storage.k8s.io/node-stage-secret-name: drive9-csi-${pvc.name}
  csi.storage.k8s.io/node-stage-secret-namespace: ${pvc.namespace}
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
```

`Retain` is the default example because this is customer data. If a customer
wants a PVC to mount a CSI-managed subdirectory instead of the Drive9 workspace
root, create a separate `StorageClass` with `remoteRootPrefix`:

```yaml
parameters:
  remoteRootPrefix: /k8s/pvc
  profile: coding-agent
```

In managed directory mode, `CreateVolume` creates a unique child directory under
that prefix and writes CSI metadata. If that separate `StorageClass` uses
`reclaimPolicy: Delete`, the driver still refuses to delete paths without both a
matching metadata index entry and a matching `.drive9-csi-volume.json` marker.

If you use `reclaimPolicy: Delete`, keep the per-PVC workload namespace Secret in
place until Kubernetes has deleted the PV. If the namespace or Secret is removed
first, the CSI sidecar cannot pass credentials to `DeleteVolume`, and backend
cleanup will require manual intervention.

## Sidecar Fallback

If a customer cannot install a CSI driver yet, use `deploy/sidecar/deployment.yaml` as a fallback. It runs a privileged Drive9 mounter sidecar and exposes the mounted directory to the app container.

This is less clean than CSI because it requires privileged pods and hostPath mount propagation. Use it for pilots or constrained clusters, not as the default production path. The fallback example also mounts the Drive9 workspace root by default; set `DRIVE9_REMOTE_ROOT` only when a subpath is intentional.

Create the sidecar Secret in the target namespace before applying the fallback deployment:

```sh
kubectl create secret generic drive9-sidecar-secret \
  --from-literal=server=https://api.drive9.ai \
  --from-literal=apiKey=drive9_api_key_redacted \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -k deploy/sidecar
```

The sidecar Secret example lives under `deploy/examples/sidecar/` and is intentionally not part of the sidecar kustomization.

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
export DRIVE9_CSI_IMAGE=ghcr.io/drive9-ai/drive9-csi:drive9-cbf73aa-csi-a163416
export DRIVE9_CSI_E2E_CONFIRM=1
hack/e2e-k8s.sh
```

The script intentionally requires `DRIVE9_CSI_E2E_CONFIRM=1` because it mutates
the current Kubernetes context. It deploys the CSI driver into an isolated
`drive9-csi-e2e-driver` namespace, creates Secret
`drive9-csi-drive9-workspace-e2e` in `drive9-csi-e2e`, creates PVC
`drive9-workspace-e2e`, mounts it into one pod, writes and reads a file, deletes
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

The default `StorageClass` uses `drive9-csi-${pvc.name}` to resolve per-PVC
credentials. Each PVC maps to exactly one Drive9 workspace via its own Secret.
This is intentional for multi-workspace scenarios: different PVCs can use
different API keys (and therefore different workspaces) without requiring
multiple StorageClasses.

To mount multiple workspaces in one namespace, create one Secret + PVC pair per
workspace:

```sh
# Workspace A
kubectl -n myapp create secret generic drive9-csi-workspace-a \
  --from-literal=server=https://api.drive9.ai \
  --from-literal=apiKey=<api-key-for-workspace-a>

cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: workspace-a
  namespace: myapp
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: drive9-rwo
  resources:
    requests:
      storage: 1Gi
EOF

# Workspace B
kubectl -n myapp create secret generic drive9-csi-workspace-b \
  --from-literal=server=https://api.drive9.ai \
  --from-literal=apiKey=<api-key-for-workspace-b>

cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: workspace-b
  namespace: myapp
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: drive9-rwo
  resources:
    requests:
      storage: 1Gi
EOF
```

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

**PVC stuck in Pending with `failed to provision volume` or Secret not found:**

The CSI provisioner resolves the Secret name from the StorageClass template
`drive9-csi-${pvc.name}`. If you created a PVC named `my-data`, the provisioner
looks for Secret `drive9-csi-my-data` in the PVC's namespace. Create it:

```sh
kubectl -n <namespace> create secret generic drive9-csi-my-data \
  --from-literal=server=https://api.drive9.ai \
  --from-literal=apiKey=<your-api-key>
```

**Multiple PVCs sharing the same API key:**

Each PVC still needs its own Secret (e.g. `drive9-csi-pvc-1`, `drive9-csi-pvc-2`)
even if the API key is identical. This is a CSI StorageClass template limitation.
For scripted provisioning, loop over PVC names:

```sh
for name in pvc-1 pvc-2 pvc-3; do
  kubectl -n myapp create secret generic "drive9-csi-$name" \
    --from-literal=server=https://api.drive9.ai \
    --from-literal=apiKey="$DRIVE9_API_KEY"
done
```

## GHCR Visibility

The image package must be public before customers can pull it without a GitHub
token. After the first successful publish, open:

```text
https://github.com/orgs/drive9-ai/packages/container/package/drive9-csi
```

Then use package settings to change the package visibility to public. The image
is anonymously pullable only after `docker manifest inspect
ghcr.io/drive9-ai/drive9-csi:drive9-cbf73aa-csi-a163416` works without
`docker login`.
