# Drive9 CSI Lite

This repository provides a minimal Kubernetes integration for `github.com/mem9-ai/drive9`.

It intentionally ships a small stable surface first:

- Dynamic directory volumes backed by Drive9 remote paths.
- `ReadWriteOnce` only.
- API key passed through Kubernetes CSI Secrets only.
- `CreateVolume` writes a marker file.
- `DeleteVolume` deletes only a path with both a matching metadata index entry and a matching root marker.
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
`drive9-csi-drive9-workspace` in the same namespace. This lets one namespace
create many PVCs backed by different Drive9 API keys or workspaces without
requiring one cluster-scoped `StorageClass` per workspace.

For the CSI path, do not put API keys in `StorageClass.parameters`, PV attributes, pod env, or annotations. The example `StorageClass` uses CSI secret references so Kubernetes passes the namespace-local secret to `CreateVolume`, `DeleteVolume`, and `NodeStageVolume`.

The sidecar fallback necessarily injects the secret into the mounter sidecar environment. Use CSI for production when the customer can install a node plugin.

The node plugin needs privileged FUSE access. Treat it like other node storage plugins: restrict who can modify its DaemonSet and workload namespace Secrets.

The controller service account needs `get` access to Secrets so the CSI
provisioner can resolve per-PVC credentials. Kubernetes RBAC cannot constrain
`resourceNames` with the `drive9-csi-${pvc.name}` template, so the default RBAC
does not grant `list` or `watch`, but it also cannot restrict reads to one fixed
Secret name.

The first version stores CSI metadata under `/k8s/.drive9-csi/volumes`. If you use a scoped Drive9 token instead of an owner key, its scope must cover both the volume prefix, for example `/k8s/pvc`, and the metadata index path `/k8s/.drive9-csi/volumes`.

## Install

The default manifests use the public customer image:

```text
ghcr.io/drive9-ai/drive9-csi:0.1.0
```

Release images are also published with a traceable tag:

```text
ghcr.io/drive9-ai/drive9-csi:drive9-<drive9-short-sha>-csi-<csi-short-sha>
```

For example, a build from Drive9 CLI commit `7fb7c58...` and CSI commit
`abcdef0...` is tagged:

```text
ghcr.io/drive9-ai/drive9-csi:drive9-7fb7c58-csi-abcdef0
```

The publish workflow does not publish `:latest`. Use `0.1.0`, a traceable tag,
or a digest.

To build and publish your own image instead:

```sh
make image IMAGE=ghcr.io/drive9-ai/drive9-csi:0.1.0
docker push ghcr.io/drive9-ai/drive9-csi:0.1.0
```

Install the CSI driver:

```sh
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl apply -k deploy/kubernetes
```

Create a Drive9 Secret in the workload namespace before creating each PVC. The
default Secret name is `drive9-csi-<pvc-name>`:

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

The default example `StorageClass` uses `Retain`, so deleting the PVC keeps the PV and Drive9 remote directory for data safety. Use a separate `StorageClass` with `reclaimPolicy: Delete` only when automatic backend deletion is intended.

Example Secret, PVC, and smoke Pod manifests live under `deploy/examples/kubernetes/` so that applying `deploy/kubernetes/` does not create placeholder credentials or demo workloads in production clusters. Apply the example Secret with `kubectl -n <workload-namespace> apply -f deploy/examples/kubernetes/secret.example.yaml` after replacing the API key. For another PVC, copy the Secret and name it `drive9-csi-<pvc-name>`.

## StorageClass

Default example:

```yaml
provisioner: csi.drive9.ai
parameters:
  remoteRootPrefix: /k8s/pvc
  profile: coding-agent
  csi.storage.k8s.io/provisioner-secret-name: drive9-csi-${pvc.name}
  csi.storage.k8s.io/provisioner-secret-namespace: ${pvc.namespace}
  csi.storage.k8s.io/node-stage-secret-name: drive9-csi-${pvc.name}
  csi.storage.k8s.io/node-stage-secret-namespace: ${pvc.namespace}
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
```

`Retain` is the default example because this is customer data. If a customer wants automatic deletion, they can switch to `Delete`; the driver still refuses to delete paths without both a matching metadata index entry and a matching `.drive9-csi-volume.json` marker.

If you use `reclaimPolicy: Delete`, keep the per-PVC workload namespace Secret
in place until Kubernetes has deleted the PV. If the namespace or Secret is
removed first, the CSI sidecar cannot pass credentials to `DeleteVolume`, and
backend cleanup will require manual intervention.

## Sidecar Fallback

If a customer cannot install a CSI driver yet, use `deploy/sidecar/deployment.yaml` as a fallback. It runs a privileged Drive9 mounter sidecar and exposes the mounted directory to the app container.

This is less clean than CSI because it requires privileged pods and hostPath mount propagation. Use it for pilots or constrained clusters, not as the default production path.

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
- `ReadWriteOnce` only.
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
export DRIVE9_CSI_IMAGE=ghcr.io/drive9-ai/drive9-csi:0.1.0
export DRIVE9_CSI_E2E_CONFIRM=1
hack/e2e-k8s.sh
```

The script intentionally requires `DRIVE9_CSI_E2E_CONFIRM=1` because it mutates
the current Kubernetes context. It deploys the CSI driver into an isolated
`drive9-csi-e2e-driver` namespace, creates Secret
`drive9-csi-drive9-workspace-e2e` in `drive9-csi-e2e`, creates PVC
`drive9-workspace-e2e`, mounts it into one pod, writes and reads a file, deletes
that pod, remounts the same PVC into a second pod, reads the same token again,
then deletes the pod and PVC and waits for the PV to be deleted. It fails closed
if either e2e namespace or cluster-scoped CSI resources already exist, because
the e2e should run on a clean validation cluster. Do not use `:latest` for
`DRIVE9_CSI_IMAGE`; use an immutable tag or digest for customer evidence.

## GHCR Visibility

The image package must be public before customers can pull it without a GitHub
token. After the first successful publish, open:

```text
https://github.com/orgs/drive9-ai/packages/container/package/drive9-csi
```

Then use package settings to change the package visibility to public. The image
is anonymously pullable only after `docker manifest inspect
ghcr.io/drive9-ai/drive9-csi:0.1.0` works without `docker login`.
