---
title: Drive9 CSI Deployment on GKE COS
---

This example installs the driver in `drive9-csi` using the opt-in COS overlay.
Use a GKE cluster with COS nodes and permission to run privileged containers
with hostPath volumes. Run the commands below from the repository root.

## Select the Image

Edit `kustomization.yaml`: replace the digest placeholder with an immutable
image containing the COS fix. Change `newName` if using another registry, such
as Artifact Registry. Nodes must be able to pull that image. For production,
select a release-admitted image; a validation image alone is not sufficient.

The placeholder is intentionally unusable. One image override updates all
four CSI containers: COS preparation, host installer, node, and controller.

## Install

Set the target context explicitly and inspect the rendered resources:

```sh
GKE_CONTEXT=replace-with-your-gke-context
kustomize build deploy/examples/kubernetes/gke-cos
```

Create the namespace and priority quota before starting the DaemonSet:

```sh
kubectl --context "$GKE_CONTEXT" apply -f deploy/kubernetes/namespace.yaml
kubectl --context "$GKE_CONTEXT" apply \
  -f deploy/examples/kubernetes/gke-cos/resourcequota.yaml
kubectl --context "$GKE_CONTEXT" apply -k deploy/examples/kubernetes/gke-cos
```

The scope-only ResourceQuota allows `system-node-critical` in this namespace.
It has no `hard.pods` limit, so it does not cap how many node-plugin Pods the
DaemonSet can create as the cluster grows.

Wait for the driver and inspect COS preparation:

```sh
kubectl --context "$GKE_CONTEXT" -n drive9-csi rollout status \
  deployment/drive9-csi-controller --timeout=180s
kubectl --context "$GKE_CONTEXT" -n drive9-csi rollout status \
  daemonset/drive9-csi-node --timeout=180s
kubectl --context "$GKE_CONTEXT" -n drive9-csi logs \
  daemonset/drive9-csi-node -c prepare-cos-exec
```

`prepare-cos-exec` runs before `install-host-binaries`. It enters the host
mount namespace and prepares `/var/lib/drive9-csi/bin` as an executable
self-bind mount while preserving the parent `/var` mount's `noexec` policy.
The binary path stays the same; no `--host-state-dir` override is needed.
Pod replacement reuses the verified bind. Do not unmount it on Pod exit,
because host-managed Drive9 mounts may still be running.

## Mount a Volume

After the driver is ready, use the [Kubernetes workload examples](../README.md)
to create a workspace Secret, PVC, and workload. Those resources are separate
from this driver installation. Use the same explicit GKE context for their
commands. A default PVC does not need a VolumeAttributesClass for COS.

The existing `e2e/prepare.sh` installs the base deployment and does not select
this COS overlay; use the installation above for this configuration.
