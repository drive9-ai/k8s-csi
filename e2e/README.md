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
export DRIVE9_CSI_IMAGE='ghcr.io/drive9-ai/drive9-csi@sha256:<manifest-digest>'

e2e/prepare.sh
```

Preparation creates the Driver namespace when absent and otherwise updates the
existing environment in place. It applies the current repository's namespace,
CSIDriver, RBAC, controller, and node manifests, waits for rollout, and leaves
the environment installed. Running it again is supported.

`DRIVE9_CSI_E2E_CONTEXT` and `DRIVE9_CSI_E2E_DRIVER_NAMESPACE` are mandatory.
Every kubectl invocation receives the context explicitly, so the current
kubectl context is ignored. Context names containing `prod` or `production`
are rejected.

## Cases

After preparation, configure the case credentials:

```sh
export DRIVE9_SERVER=https://api.drive9.ai
export DRIVE9_API_KEY=drive9_api_key_redacted
```

Run the standard CSI lifecycle case:

```sh
e2e/basic-lifecycle.sh
```

Run the CSI node Pod mount-survival case:

```sh
e2e/mount-survival.sh
```

Cases verify that the prepared controller, node, RBAC bindings, and CSIDriver
are ready. The controller, node, and host-binary installer must all use the
same immutable image digest. A case never updates or deletes the prepared
Driver.

Each case creates its own workload namespace, StorageClass, and
VolumeAttributesClass with per-run ownership metadata. It cleans up only
resources confirmed to belong to that run unless `DRIVE9_CSI_E2E_KEEP=1` is
set. Prepare once, then run either case repeatedly or in any order.

The shared Kubernetes wrapper retries only recognized client and API transport
failures, such as EOF, TLS handshake timeout, connection reset, and temporary
DNS failures. It makes at most four attempts with bounded backoff. Kubernetes
semantic errors and workload assertions are not retried. Retried case commands
are idempotent, and an ambiguous top-level resource create is accepted only
after its per-run ownership label is verified. Cleanup uses the same transport
retry and ownership checks.

`DRIVE9_CSI_E2E_KEEP=1` keeps the generated Secret and local temporary
manifests containing the API key. Use it only for debugging, then remove both
the cluster resources and the printed temporary directory.

`basic-lifecycle.sh` validates provisioning, write/read, workload Pod remount,
same-node multi-Pod access, one-Pod multi-PVC behavior, unpublish, unstage, and
PV deletion.

`mount-survival.sh` creates a live workload, records its host mount identity,
deletes the matching CSI node Pod, waits for the replacement, verifies workload
I/O continued, and confirms the mount PID, PID start time, mount ID, systemd
unit, and Drive9 binary path did not change. Its background I/O loop stops
through explicit stop and stopped markers; expected case teardown does not use
signals or treat a forced process exit as a test failure.

Set `DRIVE9_REMOTE_ROOT_PREFIX=/k8s/pvc-e2e` only when testing managed-directory
mode. The default is workspace-root mode. The lifecycle case requires
`kubectl`; the mount-survival case additionally requires `jq` and `diff`.

## Non-Mutating Validation

```sh
make e2e-check
```

This checks shell syntax, executable bits, explicit-context enforcement, and
the prepare/case ownership boundary. It does not contact a cluster.
