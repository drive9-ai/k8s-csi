---
title: CSI Pod Startup Latency Optimization Design
status: proposed
date: 2026-07-15
---

## Status

Proposed. Decisions 1 and 2 now have a local implementation, but the measurement
contract, cluster validation, and rollout are not complete. This status does
not grant release admission or change the existing default StorageClass.

## Scope Baseline

### Objective

Reduce the time from creating a Drive9 RWO workload Pod to starting its
container without changing volume identity, credentials, data durability,
mount recovery, or publish semantics.

### In Scope

1. Add an opt-in RWO StorageClass that uses `Immediate` binding so a caller can
   provision and bind its PVC before creating the workload Pod.
2. Make Drive9 FUSE workspace-root startup validation constant-cost by using
   root `Stat` instead of listing every root entry.
3. Define phase-separated measurements for provisioning, scheduling,
   `NodeStageVolume`, `NodePublishVolume`, and container startup.
4. Preserve the existing `drive9-rwo` behavior for compatibility.

### Out of Scope

1. Changing `drive9-rwo` or `drive9-rwx` from `WaitForFirstConsumer`.
2. Adding an Immediate-binding managed-directory or RWX StorageClass.
3. Removing existing remote-path or managed-volume marker validation.
4. Changing default `profile`, `durability`, TTL, cache, or writeback behavior.
5. Adding a warm-Pod pool, static PV manager, pre-mount controller, watcher,
   background worker, or another reconciliation state machine.
6. Changing image pull, container runtime, scheduler, or application startup.
7. Optimizing WebDAV startup or unrelated Drive9 CLI commands.

### Effort Estimate

Expected production scope is `20-60 LoC`, Small, across the CSI and Drive9
repositories. This excludes tests, manifests, and documentation. Re-estimate
before implementation if the work requires a new CLI flag, RPC contract,
runtime cache, or persistent state; any of those is scope expansion.

## Problem

The reported observation attributes approximately three to four seconds of
extra startup time to CSI attach and mount. That explanation combines distinct
phases and incorrectly includes attach.

The current `drive9-rwo` StorageClass uses `WaitForFirstConsumer` at
`deploy/kubernetes/storageclass.yaml:8`. Kubernetes therefore delays dynamic
provisioning and binding until a Pod consumes the PVC. Provisioning becomes
part of the scheduling path:

```text
Pod created
  -> scheduler selects a candidate node
  -> external-provisioner calls CreateVolume
  -> PVC and PV bind
  -> PodScheduled=True
```

Drive9 does not have a controller attach phase:

1. `deploy/kubernetes/csidriver.yaml:6` sets `attachRequired: false`.
2. `internal/driver/driver.go:282` does not advertise
   `PUBLISH_UNPUBLISH_VOLUME`.
3. The controller Pod contains `csi-provisioner`, not `csi-attacher`.

After scheduling, kubelet performs the node path:

```text
PodScheduled=True
  -> NodeStageVolume
       -> read Secret
       -> validate Drive9 remote root or managed-volume marker
       -> validate the installed Drive9 binary
       -> start host systemd service
       -> start Drive9 FUSE and wait for readiness
  -> NodePublishVolume
       -> verify staged mount identity
       -> bind mount into the Pod target
  -> create and start container
```

`NodePublishVolume` performs a bind mount at
`internal/driver/driver.go:1057`; cold FUSE creation is owned by
`NodeStageVolume`, not by an attach operation.

## Current Synchronous Work

For a default workspace-root volume, the cold path contains these serial
operations:

<!-- markdownlint-disable MD013 -->

| Phase | Synchronous work | Current reference |
| --- | --- | --- |
| `CreateVolume` | Read PVC, read Secret, `HEAD` remote root | `internal/driver/driver.go:358-400` |
| `NodeStageVolume` | Read Secret, `HEAD` remote root | `internal/driver/driver.go:753-834` |
| Drive9 mount | List and decode all entries under `/` | `drive9/pkg/fuse/mount.go:257-286` |
| Drive9 mount | Extra RTT probe when durability is `auto` | `drive9/pkg/fuse/mount_profile.go:100-135` |
| CSI readiness | Poll mount readiness every 250 ms | `internal/driver/mount_lifecycle.go:17` |
| `NodePublishVolume` | Verify state and bind mount | `internal/driver/driver.go:974-1067` |

<!-- markdownlint-enable MD013 -->

Repository evidence establishes that this work exists. It does not establish
how the reported three to four seconds divide among Kubernetes reconciliation,
Drive9 API latency, FUSE startup, image pull, and container runtime work.

## Decision 1: Opt-In Immediate RWO StorageClass

Add a separate StorageClass to the checked-in Kubernetes base:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: drive9-rwo-immediate
provisioner: csi.drive9.ai
parameters: {}
reclaimPolicy: Retain
volumeBindingMode: Immediate
allowVolumeExpansion: false
```

The low-latency workflow is explicit:

```text
create Secret
  -> create PVC using drive9-rwo-immediate
  -> wait for PVC Bound
  -> create workload Pod
```

This moves `CreateVolume` and PVC/PV binding before Pod creation. It does not
make provisioning faster; it removes provisioning from the Pod startup
critical path.

The existing `drive9-rwo` remains unchanged because:

1. Existing users may depend on delayed provisioning.
2. A StorageClass binding mode is not a per-PVC runtime toggle.
3. Creating a new class provides an explicit, reversible rollout boundary.

The new class has empty parameters and therefore does not introduce a new
managed-directory allocation policy. Workspace-root `CreateVolume` validates
the requested remote root but does not allocate or delete Drive9 user data.
Unused Immediate PVCs still create Kubernetes PV objects and may require
operator cleanup because the reclaim policy remains `Retain`.

## Decision 2: Constant-Cost FUSE Root Validation

The current Drive9 FUSE startup path validates `/` with `List("/")`. The server
then reads the directory, serializes every root entry, transfers the response,
and the client decodes entries that mount startup does not use.

The current Drive9 server already implements root `HEAD` as a synthetic
directory response at `drive9/pkg/server/server.go:3046-3056`. The FUSE startup
path should therefore use `Stat("/")` and require `IsDir=true`.

Target request behavior:

```text
current server:
  HEAD /v1/fs/ -> 200, X-Dat9-IsDir=true -> continue

legacy server without root HEAD support:
  HEAD /v1/fs/ -> 404 or 405 -> fall back once to GET /v1/fs/?list=1

authentication, transport, or server failure:
  return the original error -> do not mask it with a list fallback
```

The fallback preserves compatibility while making the supported path
independent of the number and size of root entries. No new CLI flag or CSI
parameter is introduced.

Only the FUSE root validation used by CSI is in scope. Applying the same change
to the separate WebDAV validation helper is a follow-up because it is not
required to reduce CSI Pod startup latency.

## Decision 3: Preserve Duplicate Validation in the First Pass

The workspace-root path is checked during `CreateVolume`, again before
`NodeStageVolume` starts a transaction, and again inside Drive9 mount. Removing
one check could save a network round trip, but the checks currently protect
different boundaries:

1. `CreateVolume` prevents binding an invalid PVC/PV.
2. `NodeStageVolume` fails before durable mount state and systemd side effects.
3. Drive9 mount protects direct CLI callers and validates its own runtime input.

Removing the NodeStage check would move ordinary invalid-root failures into the
mount transaction and weaken the current gRPC error and cleanup behavior.
Skipping the Drive9 check would require a new trusted-input CLI contract. Both
changes exceed this first pass and remain deferred until measurements show that
the duplicate request is material.

Managed-directory marker validation is security-sensitive and is not an
optimization candidate.

## Deferred Candidates

<!-- markdownlint-disable MD013 -->

| Candidate | Maximum expected benefit | Reason deferred |
| --- | --- | --- |
| Set a fixed durability instead of `auto` | One RTT probe | May change durability semantics |
| Reduce the 250 ms readiness poll interval | Less than one poll interval per successful mount | Cannot explain multi-second latency alone |
| Cache Drive9 binary digest validation | Disk read and SHA256 time | Requires a new trust and invalidation contract |
| Keep a PVC staged with a warm Pod | Avoid cold `NodeStageVolume` | Adds workload lifecycle and node-placement policy |
| Static PV or pre-mount pool | Avoid provisioning or mounting | Adds new ownership and reconciliation infrastructure |
| Remove duplicate workspace-root validation | One Drive9 API round trip | Changes failure and cleanup boundaries |

<!-- markdownlint-enable MD013 -->

These are not part of the implementation estimate or acceptance criteria.

## Measurement Contract

The benchmark must separate the following timestamps instead of reporting one
combined `scheduled` duration:

<!-- markdownlint-disable MD013 -->

| Interval | Meaning |
| --- | --- |
| PVC creation to `Bound` | Dynamic provisioning and PV binding |
| Pod creation to `PodScheduled=True` | Scheduler path, including WFFC when applicable |
| `PodScheduled=True` to `NodeStageVolume` completion | Cold node staging and FUSE mount |
| `NodeStageVolume` to `NodePublishVolume` completion | Publish validation and bind mount |
| `NodePublishVolume` completion to container `startedAt` | CRI, image, sandbox, and container startup |

<!-- markdownlint-enable MD013 -->

Use kubelet `csi_operations_seconds` for `NodeStageVolume` and
`NodePublishVolume`. Use PVC/PV events and external-provisioner logs for
provisioning. Use Pod condition timestamps and container `startedAt` for the
outer lifecycle. Drive9 server logs should confirm whether startup issued root
`list` or root `stat` requests.

Run at least 30 successful samples for each case and report p50 and p95:

1. `emptyDir` with the same pre-pulled workload image.
2. Fresh WFFC PVC plus Pod.
3. Pre-bound Immediate PVC plus Pod.
4. Case 3 with a root containing enough entries to expose list-size scaling.

Keep node type, image digest, namespace policy, Secret, Drive9 tenant, and
workload command constant. Failed or retried provisioning attempts are reported
separately instead of being discarded.

## Acceptance Criteria

1. `drive9-rwo` and `drive9-rwx` remain `WaitForFirstConsumer`.
2. A `drive9-rwo-immediate` PVC becomes `Bound` without a consuming Pod.
3. No `VolumeAttachment` owned by `csi.drive9.ai` is created.
4. A supported Drive9 server receives root `HEAD`, not root `list`, during a
   normal CSI FUSE startup.
5. Root startup request count and response size do not grow with root entry
   count on the supported path.
6. Existing workspace-root read/write, Pod recreation, unpublish, unstage, and
   Secret handling tests continue to pass.
7. Managed-directory marker validation and mount transaction state are
   unchanged.
8. The validation report includes the phase-separated p50 and p95 results.
9. Immediate pre-binding improves Pod creation-to-scheduled latency without
   regressing scheduled-to-container-start latency.
10. Root `Stat` does not regress `NodeStageVolume` p50 or p95 and removes the
    root-entry-count dependency.

## Security and Compatibility Invariants

1. Credentials remain sourced from the PVC annotation and Kubernetes Secret;
   they are not added to StorageClass parameters or PV-visible attributes.
2. `attachRequired: false` remains unchanged.
3. The new StorageClass changes only provisioning timing.
4. No mount argument, cache, durability, access mode, or topology contract
   changes.
5. Host systemd ownership, content-addressed binaries, durable mount state,
   readiness verification, and direct kernel mount behavior remain unchanged.
6. The existing WFFC classes provide immediate rollback for users of the new
   class; already-bound PVCs are not migrated between classes.

## Implementation Surfaces

<!-- markdownlint-disable MD013 -->

| Repository surface | Change |
| --- | --- |
| `deploy/kubernetes/storageclass-immediate.yaml` | Add opt-in `drive9-rwo-immediate` |
| `deploy/kubernetes/kustomization.yaml` | Include the new StorageClass |
| `hack/check-manifests.go` | Enforce both existing WFFC and new Immediate contracts |
| `README.md` | Document the pre-bind workflow and trade-off |
| `github.com/mem9-ai/drive9/pkg/fuse/mount.go` | Use root `Stat` with bounded legacy fallback |
| Drive9 FUSE mount tests | Assert HEAD fast path, fallback, and error behavior |

<!-- markdownlint-enable MD013 -->

No CSI Go production file is expected to change in this pass.

## Validation

1. Run Drive9 unit tests covering root mount validation.
2. Run `make manifest-check` in the CSI repository.
3. Run `make test` and `make check` in the CSI repository.
4. Build a validation image with the updated Drive9 CLI.
5. Run the existing basic lifecycle case before the latency benchmark.
6. Record the exact CSI image digest, Drive9 source version, Kubernetes version,
   node type, and benchmark samples.
7. Do not treat the validation image or benchmark result as release admission;
   the existing release gates remain authoritative.

## Rollout and Rollback

1. Ship the root `Stat` change in a traceable Drive9 CLI release and validation
   CSI image.
2. Add the opt-in StorageClass without changing existing PVC manifests.
3. Canary only workloads that can create their Secret and PVC before the Pod.
4. Compare phase-separated results with the WFFC baseline.
5. Roll back a workload by recreating its PVC with `drive9-rwo`; StorageClass is
   immutable for an existing PVC, so no in-place migration is attempted.

## Final Scope Check

The proposed first pass contains one opt-in provisioning-timing manifest and
one bounded Drive9 FUSE validation optimization. Warm mounts, new lifecycle
controllers, duplicate-check removal, durability changes, and persistent state
remain deferred. The final expected production scope remains `20-60 LoC`,
Small.
