---
title: CSI FUSE FD Escrow DaemonSet Options
---

## Status

This document compares two preliminary designs for preserving Drive9 FUSE connections across Kubernetes DaemonSet upgrades:

1. A dedicated FD-holder DaemonSet while `drive9-csi-node` continues to manage FUSE processes.
2. A mount-agent DaemonSet that manages FUSE processes while `drive9-csi-node` holds escrow FDs.

Both designs are feasible. The initial preference is the FD-holder design because it preserves a single mount lifecycle owner, keeps the normal CSI call path direct, requires less production code, and creates less long-term reasoning overhead.

This is an analysis and direction document, not an implementation plan or final decision.

## Problem

The current node plugin directly starts `drive9 mount` as its child process:

```text
NodeStageVolume
  -> startDrive9Mount
  -> exec drive9 mount --foreground
```

Relevant code:

1. `internal/driver/driver.go:553`: `NodeStageVolume` owns the stage operation and per-volume lock.
2. `internal/driver/driver.go:656`: `NodeStageVolume` calls `startDrive9Mount`.
3. `internal/driver/mount_linux.go:63`: `startDrive9Mount` creates the `drive9 mount` child process.
4. `internal/driver/mount_linux.go:85`: CSI records the child PID and mount identity.

When the `drive9-csi-node` Pod is replaced, its FUSE processes are also replaced. A smooth upgrade must keep the same kernel FUSE connection alive while a new FUSE process restores the userspace state required to continue serving it.

## Terminology

### Lifecycle Owner

The lifecycle owner creates, monitors, stops, and restores FUSE processes. It also decides when a mount should exist.

### FD Escrow

The FD escrow process holds a duplicate of each FUSE connection FD without serving FUSE requests. If the active FUSE process exits during a planned upgrade, the escrow duplicate prevents the kernel connection from being closed.

The escrow process does not by itself preserve userspace state. Open-file continuity also requires restoration of file handles, inode mappings, directory handles, locks, negotiated FUSE settings, and other process-local state.

### Handoff Window

The handoff window starts when the old FUSE worker stops serving and ends when the new worker begins serving the same connection. Kernel requests may block during this window.

## Common Requirements

Both designs require the same underlying Drive9 CLI capabilities:

1. Export or duplicate the active FUSE connection FD.
2. Import an existing FUSE connection FD without mounting a new connection.
3. Quiesce new requests and wait for in-flight requests.
4. Export and restore required userspace state.
5. Distinguish `prepare`, `accept`, `rollback`, and permanent `close` outcomes.
6. Avoid unmounting the kernel connection during a successful handoff.

Both designs also require:

1. Same-node Unix domain sockets and `SCM_RIGHTS`.
2. A stable session key containing at least `sessionUUID`, `volumeID`, and `generation`.
3. Serialized upgrades of the two participating DaemonSets.
4. A root-only shared hostPath for sockets and handoff state.
5. Explicit timeouts and rollback behavior.
6. A fallback recovery path when smooth handoff is not safe.

Neither design can preserve a FUSE connection across a node reboot or the simultaneous loss of every process holding an FD duplicate.

## Option 1: FD-Holder DaemonSet

### Responsibilities

```text
drive9-csi-node
  - CSI Node RPCs
  - Secret resolution
  - desired and runtime mount state
  - FUSE process creation and supervision
  - handoff orchestration

drive9-fd-holder
  - FD registry only
  - generation validation
  - FD put/get/delete/list
  - health and readiness
```

The FD holder must remain deliberately unaware of Kubernetes objects, Drive9 credentials, mount options, and FUSE process management.

### Normal Mount Flow

```text
kubelet
  -> NodeStageVolume
  -> drive9-csi-node starts drive9 mount
  -> FUSE worker registers an FD duplicate with drive9-fd-holder
  -> CSI returns stage success after mount readiness and escrow acknowledgement
```

The existing direct `NodeStageVolume -> startDrive9Mount` path remains intact.

### CSI Upgrade Flow

For every Drive9 mount on the affected node:

1. Old CSI asks the FUSE worker to prepare and export userspace state.
2. Old CSI verifies that the FD holder has the correct generation.
3. The old worker stops serving without unmounting.
4. Old CSI exits.
5. The FD holder keeps each kernel FUSE connection alive.
6. New CSI reads desired mount state, retrieves the FD, and starts a replacement worker.
7. The new worker restores state, validates the connection, and accepts ownership.
8. The worker stores a fresh FD duplicate with the holder.

All FUSE workers managed by the CSI Pod on that node are replaced. Existing I/O may block during each handoff window.

The current shutdown path cannot remain unchanged because it actively shuts down node mounts at `internal/driver/driver.go:128` and `internal/driver/node_recovery.go:263`.

### FD-Holder Upgrade Flow

The CSI process and FUSE workers continue running. After a new holder starts, CSI instructs every live worker to register a fresh FD duplicate. FUSE workers do not need to restart.

### Advantages

1. CSI remains the single desired-state and runtime-state owner.
2. The normal mount and unmount path stays within one process.
3. Existing PID, state-file, credential, and per-volume locking patterns remain usable.
4. The holder API is narrow and easy to test independently.
5. The holder never receives credentials or Kubernetes permissions.
6. Fewer retry, version-compatibility, and split-state cases.
7. Lower implementation and maintenance cost.

### Disadvantages

1. Every CSI Pod replacement restarts all FUSE workers on that node.
2. CSI control-plane-only changes still enter the FUSE handoff critical path.
3. A CSI crash affects all FUSE workers owned by that CSI Pod.
4. CSI shutdown and startup code must understand the handoff transaction.
5. CSI and FD-holder rollouts must not overlap on the same node.

## Option 2: Mount-Agent DaemonSet

### Responsibilities

```text
drive9-csi-node
  - CSI Node RPCs
  - Secret resolution
  - desired mount state
  - mount-agent client
  - FD escrow
  - NodePublish and fallback repair

drive9-mount-agent
  - runtime mount registry
  - FUSE process creation and supervision
  - PID and generation ownership
  - worker state snapshot and restore
```

### Normal Mount Flow

```text
kubelet
  -> NodeStageVolume
  -> CSI resolves credentials and mount options
  -> EnsureMount over a node-local Unix socket
  -> mount-agent starts drive9 mount
  -> worker stores an FD duplicate with CSI escrow
  -> mount-agent returns readiness
  -> CSI returns stage success
```

The internal API needs at least:

1. `EnsureMount`
2. `RemoveMount`
3. `ListMounts`
4. `GetMountStatus`
5. `PrepareAgentExit`

Requests must be idempotent because CSI can time out after the agent has already completed an operation.

### CSI Upgrade Flow

The mount agent and FUSE workers remain running. New CSI enumerates sessions from the agent, obtains fresh FD duplicates, rebuilds its escrow registry, and only then becomes ready. Existing FUSE workers do not restart.

### Mount-Agent Upgrade Flow

For every Drive9 mount on the affected node:

1. Old agent asks the worker to prepare and export state.
2. Old agent confirms that CSI escrow holds the correct FD generation.
3. The worker stops serving without unmounting.
4. Old agent exits.
5. CSI keeps the connection alive.
6. New agent obtains desired mount configuration and credentials through CSI.
7. New agent retrieves the FD and starts a replacement worker.
8. The worker restores state, accepts the connection, and stores a fresh duplicate with CSI.

All FUSE workers managed by the mount-agent Pod on that node are replaced. Existing I/O may block during the handoff windows.

### Advantages

1. CSI Pod upgrades do not restart existing FUSE workers.
2. CSI control-plane lifecycle is separated from the FUSE data-plane lifecycle.
3. CSI and mount-agent images can have independent release cadence.
4. Mount supervision can grow independently if it becomes a substantial subsystem.
5. CSI restart recovery does not require rebuilding FUSE userspace state.

### Disadvantages

1. Every normal mount operation crosses an additional process boundary.
2. Desired state and runtime state have different owners.
3. CSI must replay credentials and mount specifications after agent restart.
4. RPC timeout, retry, duplicate request, and partial-success cases become normal-path concerns.
5. CSI and agent protocol versions need compatibility rules.
6. Debugging `NodeStageVolume` and `NodeUnstageVolume` requires tracing two processes.
7. An agent crash affects every Drive9 FUSE worker on that node.
8. More production code and greater long-term reasoning cost.

## Feasibility

Both options are technically feasible under the same core assumptions.

| Scenario | FD-holder | Mount-agent |
| --- | --- | --- |
| Planned lifecycle-owner upgrade | CSI workers restart; holder preserves FDs | Agent workers restart; CSI preserves FDs |
| Planned escrow-process upgrade | Workers remain running; holder registry is rebuilt | Workers remain running; CSI escrow is rebuilt |
| Lifecycle owner exits without a valid snapshot | FD may survive, but open-file restoration is not guaranteed | FD may survive, but open-file restoration is not guaranteed |
| Escrow unavailable before owner termination | Upgrade must stop or use disruptive fallback | Upgrade must stop or use disruptive fallback |
| Both DaemonSets disappear together | Connection is lost | Connection is lost |
| Node reboot | Connection is lost | Connection is lost |
| Successful handoff | Same connection; temporary I/O blocking is possible | Same connection; temporary I/O blocking is possible |

The designs preserve kernel connection continuity, not uninterrupted request processing. During the period in which only escrow holds the FD, no FUSE daemon is reading requests.

## Trade-Off Summary

| Dimension | FD-holder | Mount-agent |
| --- | --- | --- |
| FUSE lifecycle owner | CSI node | Mount agent |
| FD escrow owner | FD holder | CSI node |
| Normal call path | kubelet -> CSI -> worker | kubelet -> CSI -> agent -> worker |
| DaemonSet that triggers worker handoff | CSI node | Mount agent |
| CSI-only upgrade | Replaces workers | Leaves workers running |
| State ownership | CSI owns desired and runtime state | CSI owns desired state; agent owns runtime state |
| Credential flow | Existing flow | CSI-to-agent transfer and replay |
| Normal-path distributed reconciliation | No | Yes |
| Agent and maintainer legibility | Higher | Lower |
| Operational data-plane separation | Lower | Higher |
| Estimated production net LoC | `~700-1,100 LoC` | `~1,100-1,700 LoC` |

## Agent Legibility and Long-Term Cognitive Load

The FD-holder design has two durable invariants:

1. CSI is always the only FUSE process lifecycle owner.
2. The FD holder only keeps validated FD duplicates.

The holder does not need to understand why a mount exists or how to recreate it. Most code investigations continue to follow the existing direct path from CSI RPC to mount process.

The mount-agent design introduces additional durable invariants:

1. CSI desired state and agent runtime state must converge.
2. Every control request must be idempotent across timeouts and restarts.
3. Credential replay must remain secure and recoverable.
4. CSI and agent versions must remain behaviorally compatible.
5. Readiness depends on both processes and their reconciliation state.

These concerns are manageable, but they become permanent normal-path complexity rather than upgrade-only complexity.

## Estimated Work

The estimates below are production net LoC. They include handwritten Go code and necessary non-boilerplate deployment configuration. They exclude tests, documentation, generated code, the shared Drive9 CLI handoff implementation, and subtree fallback work.

### FD-Holder

| Work item | Estimate |
| --- | ---: |
| Holder process, FD registry, and Unix socket protocol | `180-300 LoC` |
| CSI handoff and restore state machine | `250-400 LoC` |
| Generation validation, reconnect, recovery, and GC | `180-280 LoC` |
| DaemonSet, health, readiness, and security configuration | `50-100 LoC` |
| Total | `~700-1,100 LoC` |

### Mount-Agent

| Work item | Estimate |
| --- | ---: |
| Agent RPC server, mount registry, and process supervisor | `350-550 LoC` |
| CSI agent client and NodeStage/NodeUnstage refactor | `220-350 LoC` |
| Desired/runtime state reconciliation and restart recovery | `250-400 LoC` |
| CSI FD escrow, generation validation, and reconnect | `180-280 LoC` |
| DaemonSet, health, readiness, and security configuration | `70-120 LoC` |
| Total | `~1,100-1,700 LoC` |

If subtree recovery is retained as a fallback, add approximately `~450 LoC` of production code based on the current subtree recovery experiment. The fallback repairs future path access but cannot restore application-held FDs from a lost FUSE connection.

## Initial Preference

The initial preference is the FD-holder DaemonSet.

The deciding criteria are agent legibility, engineering simplicity, and long-term cognitive load:

1. It preserves the existing direct CSI-to-worker path.
2. It keeps desired and runtime mount state under one owner.
3. It limits the new cross-process API to a small FD registry protocol.
4. It avoids credentials and Kubernetes access in the second DaemonSet.
5. It is approximately `~400-600 LoC` smaller.
6. It keeps distributed reconciliation out of normal CSI operations.

The accepted cost is that every `drive9-csi-node` replacement performs a smooth handoff for all Drive9 FUSE workers on that node.

The preference should change to the mount-agent design if any of these become hard requirements:

1. A CSI node upgrade must never restart a FUSE worker process.
2. CSI and Drive9 mounter releases need strongly independent lifecycle and ownership.
3. Mount supervision grows into a substantial node service with its own policy, observability, or resource management.
4. CSI control-plane failures must not terminate FUSE worker processes.

## Design Guardrails for the Preferred Option

To preserve the simplicity advantage, the FD-holder implementation must follow these constraints:

1. The holder has no Kubernetes client.
2. The holder never receives Drive9 credentials.
3. The holder never starts, stops, or probes FUSE workers.
4. The holder never decides whether a mount should exist.
5. CSI remains the only desired-state and runtime-state owner.
6. The protocol is keyed by stable session identity and generation, never PID alone.
7. CSI must verify escrow before stopping a worker.
8. Failed preparation must leave the old worker serving.
9. The lifecycle owner and escrow DaemonSets must not roll simultaneously.

## Validation Required Before Implementation

1. Confirm the Drive9 CLI handoff contract covers FD import/export and all state required for active open files.
2. Define the exact `prepare -> escrow-confirmed -> stop -> restore -> accept` protocol and rollback boundaries.
3. Verify a single mount with active open files survives CSI replacement while requests are continuously issued.
4. Verify holder replacement does not restart the FUSE worker.
5. Verify stale generation, duplicate requests, socket loss, and restore timeout fail closed.
6. Verify simultaneous DaemonSet termination is prevented operationally and produces an explicit error when detected.
