# Drive9 CSI Lite

This repository provides a minimal Kubernetes integration for `github.com/mem9-ai/drive9`.

It intentionally ships a small stable surface first:

- Dynamic directory volumes backed by Drive9 remote paths.
- `ReadWriteOnce` only.
- API key passed through Kubernetes CSI Secrets only.
- `CreateVolume` writes a marker file.
- `DeleteVolume` deletes only a path with a matching marker.
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
  name: drive9-csi-secret
  namespace: drive9-csi
type: Opaque
stringData:
  server: https://drive9.example.com
  apiKey: sk-redacted
```

For the CSI path, do not put API keys in `StorageClass.parameters`, PV attributes, pod env, or annotations. The example `StorageClass` uses CSI secret references so Kubernetes passes the secret to `CreateVolume`, `DeleteVolume`, and `NodeStageVolume`.

The sidecar fallback necessarily injects the secret into the mounter sidecar environment. Use CSI for production when the customer can install a node plugin.

The node plugin needs privileged FUSE access. Treat it like other node storage plugins: restrict who can modify its DaemonSet and Secret.

The first version stores CSI metadata under `/k8s/.drive9-csi/volumes`. If you use a scoped Drive9 token instead of an owner key, its scope must cover both the volume prefix, for example `/k8s/pvc`, and the metadata index path `/k8s/.drive9-csi/volumes`.

## Install

Build and publish the image:

```sh
make image IMAGE=ghcr.io/drive9-ai/drive9-csi:dev
docker push ghcr.io/drive9-ai/drive9-csi:dev
```

Edit `deploy/kubernetes/node.yaml` and `deploy/kubernetes/controller.yaml` to use your image, then:

```sh
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl apply -f deploy/kubernetes/secret.example.yaml
kubectl apply -f deploy/kubernetes/
```

Create a PVC:

```sh
kubectl apply -f deploy/kubernetes/pvc.example.yaml
```

## StorageClass

Default example:

```yaml
provisioner: csi.drive9.ai
parameters:
  remoteRootPrefix: /k8s/pvc
  profile: coding-agent
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
```

`Retain` is the default example because this is customer data. If a customer wants automatic deletion, they can switch to `Delete`; the driver still refuses to delete paths without both a matching metadata index entry and a matching `.drive9-csi-volume.json` marker.

## Sidecar Fallback

If a customer cannot install a CSI driver yet, use `deploy/sidecar/deployment.yaml` as a fallback. It runs a privileged Drive9 mounter sidecar and exposes the mounted directory to the app container.

This is less clean than CSI because it requires privileged pods and hostPath mount propagation. Use it for pilots or constrained clusters, not as the default production path.

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
