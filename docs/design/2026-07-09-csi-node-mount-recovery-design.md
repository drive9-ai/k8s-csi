---
title: CSI Node Mount Recovery Design
status: superseded
superseded_by: docs/design/csi-rolling-upgrade-mount-survival-v2.md
---

## Status

The defensive remount approach recorded here was implemented and tested, then
superseded by host-systemd mount survival. Current mounts outlive CSI node Pod
replacement; use the superseding design and current runtime documentation for
the active contract.

## Goal

This document records the Option B node mount recovery design and the follow-up test result.

The original goal was to recover Drive9 FUSE mounts after the `drive9-csi-node` Pod restarts. Testing showed that this design can repair node-local staging state, but it cannot recover the mount already visible inside a running workload Pod.

The immediate production problem is:

1. `NodeStageVolume` starts `drive9 mount --foreground --mode=fuse` as a child process of the node plugin container.
2. Workload Pods access the volume through CSI publish bind mounts.
3. When the node plugin container is replaced, the old `drive9 mount` process may exit.
4. The workload Pod may remain Running, but its mounted path can point at a dead FUSE mount.

This design covers Option B from issue 20: stale mount detection and automatic recovery, using the state files that already exist under `/var/lib/drive9-csi`.

Post-implementation conclusion:

1. Recreating the FUSE mount on the same host staging path works.
2. Rebinding the kubelet publish target works in the host mount namespace.
3. An already-running workload Pod can still see the old dead FUSE mount at its mounted path and continue to fail with `Transport endpoint is not connected`.
4. Setting application `volumeMounts[].mountPropagation` to `HostToContainer` does not fix this layout, because the recovery replaces the CSI volume root mount instead of creating a new submount under the mounted volume.
5. Therefore `--recover-node-mounts` is not a safe rolling-upgrade mechanism for `drive9-csi-node`.

## Non-Goals

First implementation scope includes startup recovery and graceful node shutdown for recorded Drive9 mounts.

First implementation scope does not include:

1. Periodic background reconciliation.
2. Watching Pods, PVCs, or PVs.
3. Saving Drive9 credentials in state files.
4. Adding a new state file format.
5. Splitting the binary into explicit controller and node roles.
6. Changing normal `NodeUnpublishVolume` unmount semantics.
7. Probing FUSE health with `stat`, `readdir`, or path reads that may hang.
8. Recovering already-running workload Pod mount namespaces after the old FUSE instance has died.
9. Making `drive9-csi-node` DaemonSet rolling updates transparent to running workload Pods.

The first implementation is startup one-shot recovery, graceful `drive9-csi-node` shutdown, and better `NodeStageVolume` idempotency. After testing, this should be treated as host-side defensive repair only, not workload continuity.

## Terminology

CSI node mounting has two layers:

```text
NodeStageVolume:
  drive9 mount --foreground --mode=fuse -> stagingTarget

NodePublishVolume:
  bind mount stagingTarget -> publish target
```

`stagingTarget` is the node-local, volume-level mount point. A volume usually has one staging target per node.

`publish target` is the Pod-facing bind mount target. This is the path kubelet exposes to a workload Pod. One staged volume may have multiple publish targets.

The publish target binds to the mount instance that existed when the bind mount was created. If the staging target path is later unmounted and mounted again, an existing publish bind mount does not automatically switch to the new FUSE mount instance.

Rebinding publish targets updates the host-side kubelet target. It does not replace the mount object already present in a running workload Pod's mount namespace. A running Pod can therefore remain broken even when the host-side publish target has been repaired.

## Historical Behavior

The same `drive9-csi` binary runs in both the controller Deployment and node DaemonSet.

The binary registers Identity, Controller, and Node services in one gRPC server:

```text
internal/driver/driver.go:91
csi.RegisterIdentityServer(server, d)
csi.RegisterControllerServer(server, d)
csi.RegisterNodeServer(server, d)
```

The deployed sidecars decide which RPCs are actually called:

| Deployment | Caller | Expected RPCs |
|---|---|---|
| controller | `csi-provisioner` | Controller RPCs |
| node | kubelet through `node-driver-registrar` | Node RPCs |

This is acceptable for passive CSI RPC handlers, but node mount recovery is active startup behavior. It must only run in the node environment.

Current `NodeStageVolume` returns success when `stagingTarget` is already a mount point:

```text
internal/driver/driver.go:556
if mounted, err := isMountPoint(stagingTarget); err != nil {
	...
} else if mounted {
	if err := d.validateStagedMount(volumeID, remoteRoot, stagingTarget); err != nil {
		return nil, err
	}
	return &csi.NodeStageVolumeResponse{}, nil
}
```

That path validates the state file identity, but does not check whether the recorded `drive9 mount` process is still alive.

## Existing State Files

The driver already stores two classes of state files under `StateDir`, defaulting to `/var/lib/drive9-csi`.

### Mount State

Mount state path:

```text
internal/driver/driver.go:1063
{stateDir}/{safeVolumeID}.json
```

Mount state content:

```text
internal/driver/driver.go:1231
type mountState struct {
	PID           int    `json:"pid"`
	PIDStartTime  string `json:"pidStartTime"`
	VolumeID      string `json:"volumeID"`
	RemoteRoot    string `json:"remoteRoot"`
	StagingTarget string `json:"stagingTarget"`
	StartedAt     string `json:"startedAt"`
}
```

This file is the recovery index for staged Drive9 FUSE mounts.

### Publish State

Publish state path:

```text
internal/driver/driver.go:1087
{stateDir}/published-{sha256(target)}.json
```

Publish state records:

1. `volumeID`
2. `stagingTarget`
3. `target`
4. `readonly`
5. `accessMode`
6. `status`

This file is the recovery index for Pod-facing bind mounts.

## State Trust Model

State files are not ground truth. They are node-local recovery indexes.

The recovery flow must validate state against stronger sources before acting:

| Source | Role |
|---|---|
| PV `spec.csi.volumeAttributes` | Desired volume identity and mount behavior |
| Secret referenced by PV attributes | Drive9 server and API key |
| `/proc/self/mountinfo` | Actual mount path state |
| `pid` plus `pidStartTime` | Recorded `drive9 mount` process identity |

The state file may tell the driver what to inspect, but it must not be trusted to drive arbitrary unmounts or arbitrary remounts.

## Startup Flag

Add a startup flag:

```text
--recover-node-mounts=auto|enabled|disabled
```

Default:

```text
auto
```

Semantics:

| Value | Behavior |
|---|---|
| `disabled` | Never run node mount recovery |
| `enabled` | Require node recovery prerequisites, then run startup recovery |
| `auto` | Run only when node recovery prerequisites are detected; otherwise log and skip |

Deploy manifests should set this explicitly:

Controller Deployment:

```yaml
args:
  - --endpoint=unix:///csi/csi.sock
  - --node-id=$(NODE_NAME)
  - --recover-node-mounts=disabled
```

Node DaemonSet:

```yaml
args:
  - --endpoint=unix:///csi/csi.sock
  - --node-id=$(NODE_NAME)
  - --recover-node-mounts=enabled
```

This flag controls only startup node mount recovery. It does not change which CSI services are registered.

This flag must not be documented as a workload availability guarantee. It cannot make a running workload Pod switch from the dead FUSE mount object to the new FUSE mount object.

## Node Environment Detection

For `enabled` and `auto`, recovery requires the node mount environment:

1. `/dev/fuse` exists and is usable as a FUSE device.
2. `/var/lib/kubelet` exists.
3. `StateDir` exists.

Behavior:

| Flag value | Missing prerequisite behavior |
|---|---|
| `disabled` | Skip without checking |
| `auto` | Log skip reason and continue serving |
| `enabled` | Fail startup before serving |

The controller Deployment should use `disabled`. The node DaemonSet should use `enabled` so manifest mistakes fail loudly.

## Startup Order

The driver should not block CSI serving on historical mount recovery.

`server.Serve(listener)` is a blocking call, so implementation should start the recovery goroutine immediately before entering `Serve`. Missing prerequisites for `--recover-node-mounts=enabled` should still fail before `Serve`.

```text
Run()
  -> validate config
  -> create state dir
  -> create Driver
  -> validate recovery prerequisites if required
  -> start background startup recovery if enabled
  -> server.Serve(listener)
```

Recovery must use the existing per-volume mutex. That keeps recovery serialized with concurrent kubelet `NodeStageVolume`, `NodePublishVolume`, `NodeUnstageVolume`, and `NodeUnpublishVolume` calls for the same volume.

`--recover-node-mounts=enabled` means the node environment must be valid. It does not mean every historical volume must recover successfully. Single-volume recovery failures should be logged and should not crash the node plugin.

It also does not mean running workload Pods are recovered. Startup recovery runs in the node plugin mount namespace, while existing containers keep their own mount namespace view.

## Recovery Flow

Startup recovery scans mount state files:

```text
listMountStates()
  -> for each mountState:
       recoverOneVolume(state)
```

Per volume:

```text
recoverOneVolume(state):
  lock volumeID
  validate state paths are under /var/lib/kubelet
  resolve PV by CSI volumeHandle == state.volumeID
  read PV spec.csi.volumeAttributes
  validate state.remoteRoot == volumeAttributes["remoteRoot"]
  read Secret referenced by PV volumeAttributes
  evaluate staging health
  recover staging if needed
  optionally repair host-side publish targets if staging was recovered
```

### PV Resolution

Add a helper similar to `resolveSecretRefFromPV`, but returning the full CSI volume attributes:

```text
resolveVolumeContextFromPV(ctx, volumeID) map[string]string
```

The helper should list PVs, find `pv.Spec.CSI.VolumeHandle == volumeID`, validate `pv.Spec.CSI.Driver == cfg.DriverName`, and return `pv.Spec.CSI.VolumeAttributes`.

The node ClusterRole must allow PV read:

```yaml
- apiGroups: [""]
  resources: ["persistentvolumes"]
  verbs: ["get", "list"]
```

### Staging Health

For each mount state:

| Condition | Result |
|---|---|
| `stagingTarget` is mounted and `pidMatchesState(state)` is true | Healthy; skip |
| `stagingTarget` is not mounted and recorded PID is not alive | Recover staging |
| `stagingTarget` is mounted but recorded PID is not alive | Stale staging; unmount and recover |
| State remote root does not match PV attributes | Log warning and skip |
| PV or Secret cannot be resolved | Log warning and skip |

Recovery should not probe the filesystem contents under the FUSE mount. PID liveness plus mountinfo is enough for the restart failure mode and avoids potentially hanging on a dead FUSE mount.

### Staging Recovery

Recover staging by rebuilding the same inputs as `NodeStageVolume`:

1. `remoteRoot` from PV volume attributes.
2. `profile` from PV volume attributes.
3. TTL parameters from PV volume attributes.
4. perf parameters from PV volume attributes.
5. tuning parameters from PV volume attributes.
6. Drive9 server and API key from the Secret referenced by PV volume attributes.

Then call the existing `startDrive9Mount` with the same `stagingTarget` path.

If the stale staging target is still mounted, try a regular unmount first. If regular unmount fails, log the error and skip that volume. Do not use `MNT_DETACH` on the staging target in the first implementation because the staging target is the source for publish bind mounts and detaching it while publish targets still exist can make recovery harder to reason about.

### Publish Target Repair

If staging was recovered, scan matching publish state files.

For each publish state matching `(volumeID, stagingTarget)`:

1. Validate `target` is under `/var/lib/kubelet`.
2. If `target` is not mounted, bind mount `stagingTarget -> target`.
3. If `target` is mounted, try regular unmount.
4. If regular unmount fails with `EBUSY`, use lazy unmount with `MNT_DETACH`.
5. Bind mount `stagingTarget -> target`.
6. Preserve existing `readonly` behavior.
7. Preserve existing `accessMode` state.

This `MNT_DETACH` fallback applies only to startup recovery. Normal `NodeUnpublishVolume` should keep strict regular unmount behavior.

`MNT_DETACH` does not repair old open file descriptors in already-running processes. It also does not repair the mounted volume root already present inside a running workload Pod.

The tested behavior is:

1. Host-side staging target is recovered.
2. Host-side kubelet publish target is recovered and readable from the node plugin container.
3. The already-running workload Pod still sees the old dead FUSE mount and reads fail with `Transport endpoint is not connected`.
4. A Pod with `mountPropagation: HostToContainer` on the Drive9 PVC volume mount still fails after recovery.

The reason is that mount propagation handles mount and unmount events under a propagated mount. The current CSI layout exposes the FUSE filesystem as the root of the workload volume mount. Recovery replaces that root mount object on the host side; it does not create a new child mount under the already-mounted `/workspace` path inside the container.

Therefore publish target repair is only host-side cleanup. It is not sufficient for workload continuity.

## Graceful Node Shutdown

The node plugin should make a best-effort attempt to cleanly stop recorded Drive9 FUSE mounts before the `drive9-csi-node` container exits.

This does not make rolling upgrades transparent. If running workload Pods still use Drive9 volumes on the node, stopping the FUSE process can break those Pods permanently until they are recreated.

Cleanup should run only when node mount recovery is enabled or auto-detected for the process. The controller Deployment should not run this logic because it has no node FUSE mounts.

### Signal Handling

The process should handle `SIGTERM` and `SIGINT`:

```text
signal received
  -> stop accepting new CSI requests
  -> run graceful node mount shutdown if enabled
  -> exit
```

The implementation should use `grpc.Server.GracefulStop()` with a bounded timeout, then `Stop()` if needed. Mount shutdown must also be bounded by the Pod termination grace period.

The node DaemonSet should set an explicit `terminationGracePeriodSeconds` large enough for best-effort unmount attempts.

### Shutdown Scope

Shutdown scans existing mount state files and acts only on recorded staging mounts:

```text
listMountStates()
  -> for each mountState:
       lock volumeID
       validate stagingTarget under /var/lib/kubelet
       run Drive9 umount
       verify kernel mount state
       wait for recorded PID to exit
```

Shutdown should not:

1. Delete mount state files.
2. Delete publish state files.
3. Unmount publish targets.
4. Query PVs or Secrets.

State files must remain available for startup recovery in the next node plugin process. Startup recovery may repair host-side staging and publish state after the staging mount is recreated, but it cannot repair already-running workload Pods.

### Drive9 Umount First

The preferred graceful shutdown operation is the Drive9 CLI unmount command:

```text
drive9 umount --timeout=<duration> --no-auto-pack <stagingTarget>
```

The command is `umount`, not `unmount`.

`drive9 umount` is preferred over direct kernel unmount because it owns Drive9-specific shutdown behavior. `--no-auto-pack` avoids triggering profile pack upload during a DaemonSet lifecycle event.

After `drive9 umount`, shutdown should verify whether `stagingTarget` is still mounted. If it is still mounted, fall back to the existing kernel unmount helper. Finally, wait for the recorded PID to exit using the existing process identity check.

```text
try drive9 umount --timeout=30s --no-auto-pack stagingTarget
if stagingTarget is still mounted:
  try kernel unmountPath(stagingTarget)
wait for recorded PID to exit
keep state files
```

This fallback is not in conflict with `drive9 umount`. If the CLI already unmounted the path, `unmountPath(stagingTarget)` is a no-op because it checks mountinfo first.

### Shutdown Error Handling

Shutdown is best effort:

1. Malformed mount state: log and skip.
2. Path outside `/var/lib/kubelet`: log and skip.
3. `drive9 umount` failure: log and try kernel fallback if the path is still mounted.
4. Kernel unmount failure: log and continue.
5. PID wait timeout: log and continue.

Failures should not delete state files. The next startup recovery should treat remaining state as recovery input.

## NodeStageVolume Idempotency

Startup recovery is best effort. `NodeStageVolume` should also handle stale staging mounts when kubelet calls it later.

Change the idempotent mounted path from:

```text
mounted + valid state -> return success
```

to:

```text
mounted + valid state + pid alive -> return success
mounted + valid state + pid dead -> recover staging, then return success
```

This makes future staging attempts defensive even when startup recovery has skipped or failed a volume. It does not repair an already-running Pod that still holds the old mount object.

`NodeStageVolume` already receives the full `VolumeContext`, so it does not need PV lookup for this path. It can reuse the same stale staging recovery helper with request-derived volume attributes.

This is the part of the recovery design that remains useful for normal CSI correctness:

1. If kubelet calls `NodeStageVolume` and the staging target is a dead FUSE mount, the driver should not return idempotent success.
2. The driver should verify the recorded `drive9 mount` PID identity.
3. If the PID is dead, the driver should unmount the stale staging target and create a new FUSE mount before returning success.
4. This protects future mount requests from binding a dead FUSE staging mount.

It does not make `drive9-csi-node` restart safe while workload Pods are still running on the node.

## Path Safety

Recovery may execute unmount and bind mount operations based on state files. It must validate all paths before acting.

First implementation should only handle:

1. `stagingTarget` under `/var/lib/kubelet/`
2. publish `target` under `/var/lib/kubelet/`

If a path is outside this root, recovery should log a warning and skip it. It should not delete the state file automatically.

Path checks must use cleaned absolute paths.

## Error Handling

Startup recovery should prefer forward progress:

1. Malformed mount state: log and skip.
2. Malformed publish state: log and skip.
3. Missing PV: log and skip.
4. Missing Secret: log and skip.
5. Remote root mismatch: log and skip.
6. Unmount failure: log and skip that path.
7. Bind mount failure: log and keep state for future retry.

The only startup-fatal errors are invalid configuration or missing node prerequisites when `--recover-node-mounts=enabled`.

Graceful shutdown should also prefer forward progress. It should log per-volume failures and continue to the next recorded mount until its shutdown deadline is reached.

## Operational Impact

Current architecture ties each `drive9 mount --foreground --mode=fuse` process to the `drive9-csi-node` container lifecycle. Updating the node DaemonSet can therefore kill the FUSE process used by running workload Pods.

Because the running workload Pod mount cannot be repaired in place, Drive9 CSI node upgrades must be treated as disruptive maintenance unless Drive9 workloads are drained first.

Safe per-node upgrade sequence:

1. Prevent new Drive9 workload Pods from scheduling onto the node.
2. Evict or delete workload Pods on that node that use Drive9 PVCs.
3. Wait for kubelet to call `NodeUnpublishVolume` and `NodeUnstageVolume` for those volumes.
4. Confirm there are no active Drive9 publish mounts on the node.
5. Replace or restart the `drive9-csi-node` Pod on that node.
6. Wait for the new node plugin to become Ready.
7. Allow Drive9 workload Pods to schedule or restart.

Reversing this order is unsafe. If the node plugin is upgraded first, the old FUSE process can die while workload Pods still hold the old mount object. Restarting those workload Pods afterward is the only reliable recovery for those Pods.

Long-term transparent upgrades require changing the FUSE process lifecycle so that it no longer dies with the node plugin container.

## Implementation Plan

Implemented production code changes:

1. Add `RecoverNodeMounts` to `driver.Config`.
2. Parse `--recover-node-mounts` in `cmd/drive9-csi/main.go`.
3. Add node recovery mode validation: `auto`, `enabled`, `disabled`.
4. Add node environment prerequisite checks.
5. Add `listMountStates()`.
6. Add `resolveVolumeContextFromPV()`.
7. Add recovery helpers for staging and publish targets.
8. Start the recovery goroutine from `Run()` immediately before `server.Serve(listener)`.
9. Add signal handling and graceful gRPC shutdown.
10. Add graceful node mount shutdown that calls `drive9 umount --no-auto-pack` first.
11. Add kernel unmount and PID wait fallback for graceful shutdown.
12. Update `NodeStageVolume` idempotency to check `pidMatchesState`.
13. Add node RBAC for PV `get/list`.
14. Add manifest args for controller and node.
15. Add node `terminationGracePeriodSeconds`.

Test changes:

1. Unit test flag parsing and mode validation.
2. Unit test `auto`, `enabled`, and `disabled` prerequisite behavior.
3. Unit test mount state listing ignores publish state files.
4. Unit test PV volume attribute resolution.
5. Unit test `NodeStageVolume` detects mounted staging with dead PID and attempts recovery.
6. Unit test publish target repair uses regular unmount first and `MNT_DETACH` only as recovery fallback.
7. Unit test graceful shutdown invokes `drive9 umount --no-auto-pack` before kernel fallback.
8. Unit test graceful shutdown keeps state files.
9. Unit test graceful shutdown does not unmount publish targets.
10. Manifest test controller has `--recover-node-mounts=disabled`.
11. Manifest test node has `--recover-node-mounts=enabled`.
12. Manifest test node RBAC includes PV `get/list`.
13. Manifest test node has explicit `terminationGracePeriodSeconds`.

Follow-up production code changes:

1. Reword user-facing documentation so `--recover-node-mounts` is not presented as running Pod recovery.
2. Consider disabling startup publish target repair by default if it is not needed for a concrete failure mode.
3. Keep or simplify `NodeStageVolume` stale PID handling as defensive idempotency.
4. Add an explicit safe upgrade procedure for `drive9-csi-node`.
5. Decide whether the node DaemonSet should use an update strategy that avoids automatic rolling replacement.

Estimated follow-up production code scope: `30-150 LoC`, depending on whether startup recovery is disabled or removed.

## Open Questions

Blocking open questions for the next design:

1. Should startup recovery remain enabled on the node DaemonSet, or should it be disabled until there is a proven host-side failure mode it fixes?
2. Should publish target repair be removed, given that it does not recover running Pods and may create false confidence?
3. Should the node DaemonSet use `OnDelete` or another upgrade strategy to prevent accidental rolling replacement?
4. Should Drive9 CSI include an opt-in controller or script that performs per-node Drive9 workload drain before replacing `drive9-csi-node`?
5. Which long-term architecture should decouple FUSE lifecycle from the node plugin: per-volume mounter Pod, host-level supervisor, or FUSE fd handoff?

Decisions that still stand:

1. State files are indexes, not truth.
2. Recovery paths must validate PV attributes, Secret references, `/proc/self/mountinfo`, and PID identity before acting.
3. `NodeStageVolume` should not return idempotent success for a mounted staging target whose recorded `drive9 mount` PID is dead.
4. `MNT_DETACH` must not be used for staging target recovery in this design.
5. The current design does not support transparent `drive9-csi-node` rolling upgrades while Drive9 workload Pods are running.
