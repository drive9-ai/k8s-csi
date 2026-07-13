---
title: Drive9 CSI End-to-End Tests
---

## Purpose

These cases validate Drive9 CSI behavior against a real Kubernetes cluster and
a real Drive9 server. They are manually invoked and intentionally separate from
image publishing and local unit-test acceptance.

## Prepare the Reusable Driver

```sh
export DRIVE9_CSI_E2E_CONTEXT=dev-dat9-eks-ap-southeast-1
export DRIVE9_CSI_E2E_DRIVER_NAMESPACE=drive9-csi
export DRIVE9_CSI_E2E_CONFIRM=1

e2e/prepare.sh --image-tag drive9-a53e497-csi-d91bfe3
```

Preparation creates the Driver namespace when absent and otherwise updates the
existing environment in place. It applies the current repository's namespace,
CSIDriver, RBAC, controller, and node manifests, waits for rollout, and leaves
the environment installed. Running it again is supported.

`--image-tag` accepts only the bare
`drive9-<cli-sha7>-csi-<csi-sha7>` tag emitted by a completed image publishing
workflow. Copy that tag literally into the command. Preparation maps it to
`ghcr.io/drive9-ai/drive9-csi:<tag>` without local digest resolution.

For a manual, non-Codex invocation, an immutable `@sha256:<digest>` image may
still be supplied through `DRIVE9_CSI_IMAGE` while calling `prepare.sh` without
arguments. Do not combine `DRIVE9_CSI_IMAGE` with `--image-tag`; preparation
rejects conflicting image inputs before any cluster access.

`DRIVE9_CSI_E2E_CONTEXT` and `DRIVE9_CSI_E2E_DRIVER_NAMESPACE` are mandatory.
Every kubectl invocation receives the context explicitly, so the current
kubectl context is ignored. Context names containing `prod` or `production`
are rejected.

## Cases

Before running a case, pre-provision a Secret containing `server` and `apiKey`
in the case namespace, then configure only its non-sensitive name:

```sh
export DRIVE9_CSI_E2E_SECRET_NAME=drive9-csi-secret-flags-test
```

By default, the case namespace is
`DRIVE9_CSI_E2E_DRIVER_NAMESPACE`. To keep workloads in a separate namespace,
create that namespace and its Secret first, then set:

```sh
export DRIVE9_CSI_E2E_NAMESPACE=drive9-csi-cases
```

Cases never create, copy, render, or delete the namespace or Secret. The Secret
must be in the case namespace because each PVC resolves its credentials from
its own namespace.

Run the standard CSI lifecycle case:

```sh
e2e/basic-lifecycle.sh
```

Run the CSI node Pod mount-survival case:

```sh
e2e/mount-survival.sh
```

Run the two-node RWX case on a cluster with at least two eligible nodes:

```sh
e2e/multi-node-rwx.sh
```

The RWX case creates two Pods with required hostname anti-affinity and fails if
they cannot become Ready on distinct nodes. It writes a uniquely named file in
each Pod, polls with a bounded timeout until the other Pod reads it, deletes Pod
A, and verifies Pod B can still read and write. Cleanup removes only the case's
labeled StorageClass, VolumeAttributesClass, PVC, and Pods.

Cases verify that the prepared controller, node, RBAC bindings, and CSIDriver
are ready. The controller, node, and host-binary installer must all use the
same accepted validation image reference. A case never updates or deletes the
prepared Driver.

Each case creates labeled StorageClass, VolumeAttributesClass, PVC, and Pod
resources. It cleans up only resources confirmed to belong to that run and
never deletes the reusable namespace or Secret. Prepare once, then run any case
repeatedly or in any order.

The shared Kubernetes wrapper retries only recognized client and API transport
failures, such as EOF, TLS handshake timeout, connection reset, and temporary
DNS failures. It makes at most four attempts with bounded backoff. Kubernetes
semantic errors and workload assertions are not retried. Retried case commands
are idempotent, and an ambiguous top-level resource create is accepted only
after its per-run ownership label is verified. Cleanup uses the same transport
retry and ownership checks.

`DRIVE9_CSI_E2E_KEEP=1` keeps case-owned resources and generated non-secret
manifests for debugging. The pre-provisioned Secret remains environment-owned
regardless of this setting.

`basic-lifecycle.sh` validates provisioning, write/read, readonly Pod access
where reads succeed and writes are denied, workload Pod remount, same-node
multi-Pod access, one-Pod multi-PVC behavior, unpublish, unstage, and PV
deletion.

`mount-survival.sh` creates a live workload, records its host mount identity,
deletes the matching CSI node Pod, waits for the replacement, verifies workload
I/O continued, and confirms the mount PID, PID start time, mount ID, systemd
unit, and Drive9 binary path did not change. Its background I/O loop stops
through explicit stop and stopped markers; expected case teardown does not use
signals or treat a forced process exit as a test failure.

`multi-node-rwx.sh` uses `profile=none`, `durability=close-sync`, and
`ReadWriteMany`. It validates observed cross-node visibility for separate files;
it does not claim distributed locking, concurrent same-file merge, or immediate
cache coherence.

Set `DRIVE9_REMOTE_ROOT_PREFIX=/k8s/pvc-e2e` only when testing managed-directory
mode. The default is workspace-root mode. The lifecycle case requires
`kubectl`; the mount-survival case additionally requires `jq` and `diff`.

## Non-Mutating Validation

```sh
make e2e-check
```

This checks shell syntax, executable bits, explicit-context enforcement, and
the prepare/case ownership boundary. It does not contact a cluster.
