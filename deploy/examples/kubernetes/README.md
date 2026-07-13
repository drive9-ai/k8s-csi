---
title: Drive9 CSI Kubernetes Examples
---

## Which Example to Use

<!-- markdownlint-disable MD013 -->

| Example | Purpose | Apply model |
| ------- | ------- | ----------- |
| `shared-pvc.example.yaml` | Two read-write replicas share one RWX PVC | Replace the API key, then apply one file |
| Separate `*.example.yaml` files | Inspect or customize Secret, StorageClass, VolumeAttributesClass, PVC, and Pod independently | Apply selected files in dependency order |

<!-- markdownlint-enable MD013 -->

The examples assume the Drive9 CSI driver is already installed with a compatible
image. The checked-in driver base uses the fail-closed
`registry.invalid/drive9-csi:unpublished` image and cannot run as-is. Use an
immutable, release-admitted image for production; a validation workflow tag or
digest alone is not release-admission evidence.

## Shared-PVC Scenario

`shared-pvc.example.yaml` models an application in which multiple replicas of
one Deployment concurrently read and write the same workspace. Typical uses
include horizontally scaled coding agents or workers that need a common
workspace.

The replicas may run on the same node or different nodes:

```text
writer Deployment
  replica A (read-write) --\
                           +--> one RWX PVC --> Drive9 workspace
  replica B (read-write) --/
```

The PVC uses Kubernetes `ReadWriteMany` without a VolumeAttributesClass, so the
CSI driver adds no RWX-specific `profile` or `durability` values. Any mount
parameters configured by the user are forwarded to `drive9 mount`. The
Deployment intentionally has no affinity or anti-affinity; a two-node cluster
is not required to use the example.

## Resources in the Single-File Example

<!-- markdownlint-disable MD013 -->

| Kind | Name | Role |
| ---- | ---- | ---- |
| Namespace | `drive9-shared-pvc-example` | Isolates all namespaced example resources |
| StorageClass | `drive9-shared-pvc-example` | Dynamically provisions the PVC with `WaitForFirstConsumer` and retains the PV on cleanup |
| Secret | `drive9-workspace-secret` | Holds the Drive9 server and API key in the workload namespace |
| PersistentVolumeClaim | `drive9-workspace-shared` | Resolves the Secret through `drive9.ai/secret-name` and represents the shared workspace |
| Deployment | `drive9-workspace-writer` | Runs two read-write replicas; each updates a file named after its Pod |

<!-- markdownlint-enable MD013 -->

The StorageClass is cluster-scoped. Its example-specific name avoids changing
the driver's default classes. The Secret, PVC, and Deployment are namespaced.

Credentials flow only through the PVC annotation and Secret:

```text
PVC annotation drive9.ai/secret-name
  -> controller resolves the namespaced Secret
  -> PV volume attributes store only the Secret reference
  -> node resolves the Secret when staging the volume
```

The API key is not stored in the StorageClass, VolumeAttributesClass, or PV
attributes.

## Install and Apply Once

Requirements:

1. Select a compatible Drive9 CSI image and install the Driver first.
2. Replace `replace-with-drive9-api-key` in `shared-pvc.example.yaml` with a
   valid Drive9 API key.

The shared-PVC example does not require `VolumeAttributesClass`; only the
separate tuned examples use it.

Driver installation and workload creation are separate operations. For local
validation, first build and preload `ghcr.io/drive9-ai/drive9-csi:local` as
described in the repository README, then install the image-pinned local overlay:

```sh
kubectl apply -k deploy/overlays/local
```

For production, copy that overlay pattern and replace `:local` with the
release-admitted immutable image reference selected by your release process. Do
not apply the fail-closed base directly.

After replacing the placeholder API key, apply all five example resources with
one command:

```sh
kubectl apply -f deploy/examples/kubernetes/shared-pvc.example.yaml
```

Wait for the Deployment:

```sh
kubectl -n drive9-shared-pvc-example rollout status \
  deployment/drive9-workspace-writer --timeout=180s
```

## Verify Sharing

Inspect where Kubernetes scheduled the replicas. They may use the same node or
different nodes:

```sh
kubectl -n drive9-shared-pvc-example get pods \
  -o custom-columns=NAME:.metadata.name,NODE:.spec.nodeName
```

List the files through either replica. What is visible follows the configured
Drive9 mount arguments:

```sh
kubectl -n drive9-shared-pvc-example exec \
  deployment/drive9-workspace-writer -- \
  sh -c 'ls -1 /workspace/drive9-workspace-writer-*.txt'
```

The CSI driver does not add a data-consistency policy for RWX. It forwards mount
parameters and does not provide distributed locks, merge concurrent writes, or
define cross-node visibility semantics.

## Cleanup

Delete the resources declared in the example:

```sh
kubectl delete -f deploy/examples/kubernetes/shared-pvc.example.yaml
```

The example StorageClass uses `Retain`. Deleting the bundle therefore removes
the Namespace and workloads but intentionally leaves the dynamically created PV
and Drive9 workspace data. Inspect the released PV and delete the PV object
manually only after confirming that retention is no longer needed.

## Separate Manifests

The remaining files expose the same concepts as individually customizable
resources:

<!-- markdownlint-disable MD013 -->

| File | Role |
| ---- | ---- |
| `secret.example.yaml` | Namespaced Drive9 endpoint and API key template |
| `storageclass.example.yaml` | Default workspace-root provisioning policy |
| `volumeattributesclass.example.yaml` | Optional tuned mount profile |
| `pvc.example.yaml` | PVC that binds the Secret and tuned VolumeAttributesClass |
| `pod.example.yaml` | Single-Pod read/write smoke workload |

<!-- markdownlint-enable MD013 -->

The tuned VolumeAttributesClass parameters are consumed when the PVC's volume is
created. The driver does not dynamically apply a later
`spec.volumeAttributesClassName` change; recreate the volume to use a different
mount configuration. Legacy StorageClass mount parameters remain supported, but
VAC values take precedence for new volumes.

Use these files when testing one resource at a time or integrating the PVC into
an existing namespace. Use `shared-pvc.example.yaml` when the goal is a complete
RWX multi-Pod demonstration with one apply.
