---
title: CSI Subtree Mount Recovery Experiment Design
---

## Goal

This document records an experimental alternative to the repeated bind mount recovery design.

The repeated bind design keeps the existing user path contract, where the workload mounts the Drive9 PVC directly at `/workspace`. It can recover a running Pod by stacking a new bind mount on top of the existing CSI publish target. The cost is that every successful node-plugin recovery can add one more mount layer at the publish target until the Pod is deleted.

This experiment changes the published volume layout:

```text
/drive9              # stable propagation anchor
/drive9/workspace    # Drive9 FUSE content
```

The goal is to verify whether Drive9 CSI can recover a running Pod by replacing a child mount under a stable propagated parent, instead of stacking a new mount directly on the volume root.

## Non-Goals

First experiment scope does not include:

1. Preserving `/workspace` as the primary mount path.
2. Making the child directory name configurable.
3. Supporting dynamic VolumeAttributesClass updates.
4. Removing the repeated bind design from the existing branch.
5. Supporting multiple child mounts under `/drive9`.
6. Changing `NodeStageVolume` or Drive9 FUSE process startup behavior.

The first experiment fixes the child directory name to `workspace`.

## User Contract

Workload Pods mount the PVC at `/drive9`, not `/workspace`:

```yaml
volumeMounts:
  - name: workspace
    mountPath: /drive9
    mountPropagation: HostToContainer
```

Applications and agents use:

```text
/drive9/workspace
```

The path `/drive9` is a stable volume root and propagation anchor. It is not the Drive9 workspace itself.

The path `/drive9/workspace` is the Drive9 workspace and should contain the FUSE-backed data.

## Target Host Layout

CSI receives a kubelet `targetPath` in `NodePublishVolume`. With the subtree layout, that target path becomes the stable anchor:

```text
targetPath/              # stable publish anchor
└── workspace/           # bind mount of stagingTarget
```

The staged Drive9 FUSE mount remains unchanged:

```text
stagingTarget            # drive9 mount --foreground --mode=fuse
```

Publishing should create:

```text
mkdir targetPath
self-bind targetPath -> targetPath
make-rshared targetPath
mkdir targetPath/workspace
bind stagingTarget -> targetPath/workspace
```

Kubelet and the container runtime then expose `targetPath` to the container as `/drive9`. Since `/drive9/workspace` is a child mount under the propagated volume root, later child mount changes should be visible inside the running Pod.

The host-side anchor must be explicit. The experiment should not rely on propagation mode inherited from `/var/lib/kubelet` or from the node plugin container's hostPath mount. After the self-bind makes `targetPath` a mount point, the driver should mark it recursively shared:

```go
unix.Mount(targetPath, targetPath, "", unix.MS_BIND, "")
unix.Mount("", targetPath, "", unix.MS_SHARED|unix.MS_REC, "")
```

## Recovery Hypothesis

The current root-publish layout is:

```text
targetPath = bind(stagingTarget)
container /workspace = bind(targetPath)
```

When the Drive9 FUSE process dies, replacing `targetPath` on the host does not repoint the existing mount object already present in the Pod mount namespace.

The subtree layout changes the recovery event from "replace the volume root" to "replace a child mount below a stable root":

```text
targetPath/                 # remains stable
└── workspace/              # child mount can be replaced
```

Recovery sequence:

```text
recover stagingTarget
repair targetPath/workspace
```

The expected behavior is:

1. `targetPath` remains mounted as the stable anchor.
2. `targetPath/workspace` is repaired to point at the recovered staging mount.
3. The child mount event propagates through `HostToContainer`.
4. The running Pod sees a recovered `/drive9/workspace` path.

## Publish Flow

`NodePublishVolume` should change from:

```text
bind stagingTarget -> targetPath
```

to:

```text
mkdir targetPath
self-bind targetPath -> targetPath
make-rshared targetPath
mkdir targetPath/workspace
bind stagingTarget -> targetPath/workspace
```

Idempotency should validate both layers:

1. `targetPath` is mounted as the anchor.
2. `targetPath/workspace` is mounted.
3. The top mount at `targetPath/workspace` refers to the same mount as `stagingTarget`.
4. Publish state matches the requested volume, staging target, target path, readonly flag, and access mode.

If `targetPath` exists but is not mounted, `NodePublishVolume` may recreate the anchor.

If `targetPath/workspace` exists but is not mounted, `NodePublishVolume` should bind the staging target there.

### Crash Safety

Subtree publishing has two mount operations, so publish must preserve the existing write-before-bind pattern:

```text
write pending publish state
ensure targetPath anchor: mkdir + self-bind + make-rshared
ensure targetPath/workspace child: mkdir + bind stagingTarget
promote state to published
```

If publishing fails before promotion, cleanup should run in child-to-parent order:

```text
if targetPath/workspace is mounted: unmountAllAt(targetPath/workspace)
remove empty targetPath/workspace
if targetPath is mounted: unmountAllAt(targetPath)
remove empty targetPath
remove pending state
```

Retry behavior:

| Observed state | Action |
|---|---|
| pending state + anchor mounted + child missing | Cleanup anchor, then publish from scratch. |
| pending state + anchor mounted + child mounted | Cleanup child and anchor, then publish from scratch. |
| no state + anchor mounted | If child is missing, cleanup anchor, then publish from scratch. |
| published state + anchor mounted + child missing | Bind child and keep published state. |
| published state + stale child | Repair child. |
| published state + child already matches staging | Return success. |

## Recovery Flow

`recoverStagedMount` should continue to recover the staging target first.

For each matching publish state:

1. Validate `targetPath` is under `/var/lib/kubelet`.
2. Validate or recreate the stable anchor at `targetPath`, including `make-rshared`.
3. Compute `workspaceTarget = targetPath/workspace`.
4. If `workspaceTarget` is already mounted and its top mount matches `stagingTarget`, return success.
5. If `workspaceTarget` is mounted but points at the old FUSE instance, unmount `workspaceTarget`.
6. Bind mount `stagingTarget -> workspaceTarget`.

If regular unmount of `workspaceTarget` fails with `EBUSY`, the experiment should record the behavior and test a lazy unmount fallback only for the child mount. The fallback must still prove that `/drive9/workspace` recovers inside the already-running Pod.

The recovery code must not unmount `targetPath` while a Pod is still published.

## Unpublish Flow

`NodeUnpublishVolume` should clean up in child-to-parent order:

```text
unmountAllAt(targetPath/workspace)
remove empty targetPath/workspace
unmountAllAt(targetPath)
remove empty targetPath
remove publish state
```

This keeps normal Pod deletion cleanup strict and deterministic. The removals must use empty-directory removal semantics, not recursive deletion. If either directory contains unexpected content, unpublish should return an error rather than deleting it.

Kubernetes CSI drivers commonly use the `k8s.io/mount-utils` helper `CleanupMountPoint` for this final mount-point cleanup. For example, JuiceFS unmounts repeated bind layers and then calls `CleanupMountPoint(target, ...)` to remove the target path. Drive9 CSI keeps its own `unmountAllAt` helper because subtree recovery and earlier repeated-bind experiments need multi-layer unmount behavior, but it should mirror the same final cleanup semantics:

```text
unmount all known layers
remove the now-empty mount point directory
```

If `targetPath/workspace` is not mounted, unpublish should continue to the anchor cleanup.

If `targetPath` is not mounted, unpublish should still remove empty publish directories and then remove matching publish state.

## Publish State

Publish state should include enough information to avoid confusing subtree publish targets with legacy root publish targets:

```go
type publishState struct {
	VolumeID      string `json:"volumeID"`
	StagingTarget string `json:"stagingTarget"`
	Target        string `json:"target"`
	Layout        string `json:"layout,omitempty"`
	WorkspaceDir  string `json:"workspaceDir,omitempty"`
	Readonly      bool   `json:"readonly"`
	AccessMode    string `json:"accessMode,omitempty"`
	Status        string `json:"status,omitempty"`
	PublishedAt   string `json:"publishedAt"`
}
```

Field meanings:

| Field | First experiment value |
|---|---|
| `Layout` | `subtree` |
| `WorkspaceDir` | `workspace` |

Legacy state files without `Layout` should be treated as the existing root layout. The experiment branch can either reject legacy root states during subtree repair or keep the old repeated bind repair only for those states.

## Code Changes

Main implementation points:

1. `internal/driver/driver.go:731` `NodePublishVolume`
   - Create, self-bind, and `make-rshared` the publish anchor at `targetPath`.
   - Bind `stagingTarget` to `targetPath/workspace`.
   - Write `publishState.Layout = "subtree"` and `publishState.WorkspaceDir = "workspace"`.

2. `internal/driver/driver.go:809` `NodeUnpublishVolume`
   - Unmount `targetPath/workspace` first.
   - Remove empty `targetPath/workspace`.
   - Then unmount the self-bound `targetPath` anchor.
   - Remove empty `targetPath`.

3. `internal/driver/driver.go:862` `validatePublishedMount`
   - Validate the child mount instead of the publish root for subtree state.
   - Use `topMountsReferToSameMount(stagingTarget, targetPath/workspace)`.

4. `internal/driver/node_recovery.go:223` `repairPublishTargets`
   - Route `Layout = "subtree"` states to subtree repair.
   - Repair the child mount under the stable anchor.
   - Do not run repeated bind on the publish root for subtree states.

5. `internal/driver/mount_linux.go:231`
   - Add a helper for self-binding the anchor.
   - Add a helper for `make-rshared`.
   - Reuse existing bind, mountinfo, and unmount helpers where possible.

6. `deploy/examples/kubernetes/pod.example.yaml`
   - Change `mountPath` from `/workspace` to `/drive9`.
   - Add `mountPropagation: HostToContainer`.
   - Change the smoke command to use `/drive9/workspace`.

7. Tests
   - Add focused unit tests for subtree target path calculation.
   - Add `NodePublishVolume` tests for pending state, published state, and idempotency.
   - Add recovery tests for matching child mount, stale child mount, and missing child mount.
   - Add unpublish tests for child-first cleanup.

## Dev Validation Plan

Use the dev cluster to validate behavior before considering productionization:

1. Build and publish an experiment image from this branch.
2. Deploy controller and node DaemonSet with `--recover-node-mounts`.
3. Create a Pod with PVC mounted at `/drive9` and `mountPropagation: HostToContainer`.
4. Verify reads and writes through `/drive9/workspace`.
5. Restart `drive9-csi-node`.
6. Verify the running Pod can read and write through `/drive9/workspace` after recovery.
7. Check host mountinfo for `targetPath` and `targetPath/workspace`.
8. Confirm recovery does not add layers at `targetPath`.
9. Confirm whether layers are added at `targetPath/workspace`.
10. Delete the Pod and confirm both child and anchor mounts are cleaned.

## Open Questions

1. Does regular `umount(targetPath/workspace)` succeed while the running Pod has the child mount active?
2. If regular unmount returns `EBUSY`, does lazy unmount of the child mount propagate well enough to recover `/drive9/workspace`?
3. Does the container runtime expose existing child mounts under `/drive9` on first Pod start in all supported runtimes?
4. Should legacy root-layout states be ignored on this experiment branch, or repaired with the existing repeated bind logic?

## Acceptance Criteria

The subtree experiment is useful only if all of these pass:

1. A fresh Pod can use `/drive9/workspace`.
2. A running Pod recovers `/drive9/workspace` after `drive9-csi-node` restart.
3. Recovery does not stack new mounts on the publish root `targetPath`.
4. Pod deletion removes the child mount and the anchor mount.
5. The branch keeps this experiment separate from the repeated bind branch for review.
