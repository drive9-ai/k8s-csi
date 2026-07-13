---
title: CSI Host PID Process-Control Simplification Design
status: draft
date: 2026-07-11
---

## Status

This document is a discussion draft. It records a proposed simplification and
the evidence collected so far. It does not authorize implementation,
deployment, commit, or push.

The proposal has not been implemented: the current node DaemonSet does not set
`hostPID: true`. Its FUSE-helper assumptions are historical and were superseded
by `2026-07-12-csi-direct-mount-strict-helper-removal-design.md`.

## Problem

The CSI node Pod currently does not use the host PID namespace. It enters the
host mount namespace and selects the host root with `nsenter`, but the client
process remains in the Pod PID namespace and Pod cgroup.

This boundary caused three implementation complications observed during EKS
validation:

1. The CSI process cannot enter the ancestor host PID namespace. Host PID
   signals therefore run through short-lived systemd transient services.
2. Direct host `systemctl` calls fail with
   `Failed to connect to bus: No data available`. The driver therefore asks a
   short-lived host transient service to run `systemctl`.
3. Direct asynchronous `systemd-run` failed from the original Pod context. The
   driver therefore uses a synchronous outer systemd bridge which invokes a
   second asynchronous `systemd-run` to create the real mount service.

The current implementation works, but process control now has two different
classes of transient services:

```text
Short-lived bridge/action units
  -> host systemctl
  -> host PID signals
  -> inner asynchronous systemd-run

Long-lived mount unit
  -> drive9-mount-<volume-hash>.service
  -> drive9-csi-launcher
  -> drive9 mount --foreground
```

The bridge layer increases command construction, preflight, failure modes, and
unit-test surface without changing the core requirement: Drive9 must remain in
a host systemd service cgroup so it survives CSI Pod replacement.

## Goals

1. Use the host PID namespace as an explicit CSI node runtime requirement.
2. Execute host `systemctl` and asynchronous `systemd-run` directly after
   entering the host mount namespace and selecting the host root.
3. Send signals directly to fully verified host PIDs without creating signal
   transient units.
4. Preserve the long-lived `drive9-mount-*.service` lifecycle and rolling
   upgrade mount-survival behavior.
5. Preserve all existing PID, binary, systemd unit, volume, and mount ownership
   checks.
6. Make the Pod-wide trust expansion explicit and add compensating controls.

## Non-Goals

1. Removing host systemd as the Drive9 mount process supervisor.
2. Running Drive9 as a direct child of the CSI container.
3. Changing CSI Controller, volume identity, credential, staging, or publishing
   semantics.
4. Enabling go-fuse `DirectMount` or removing the host `fusermount3` helper.
5. Splitting the registrar into a separate DaemonSet in the proposed default
   design.
6. Adding a heavy E2E suite or moving deployment validation into the image
   publishing workflow.

## Verified Evidence

The following probes ran against
`dev-dat9-eks-ap-southeast-1`, namespace `drive9-csi`, on Amazon Linux 2023
worker nodes.

### Direct host systemctl

The same command was tested before, during, and after a temporary
`hostPID: true` rollout:

| Pod configuration | Result |
| --- | --- |
| Original `hostPID` absent | `Failed to connect to bus: No data available` |
| `hostPID: true`, node 1 | `systemd-journald.service` returned `active/running` |
| `hostPID: true`, node 2 | `systemd-journald.service` returned `active/running` |
| Original configuration restored | The original D-Bus error returned |

While `hostPID` was enabled, `/proc/self/ns/pid` and `/proc/1/ns/pid` had the
same namespace inode on both nodes.

### Direct asynchronous systemd-run

With `hostPID: true`, the CSI container directly invoked host
`systemd-run` without an outer `--wait --pipe` bridge:

```text
systemd-run \
  --service-type=exec \
  --collect \
  --unit=drive9-hostpid-async-probe-20260711-a.service \
  --description=drive9-hostpid-async-probe \
  --property=Restart=no \
  -- /bin/sleep 30
```

The command returned before `sleep` exited. A subsequent direct `systemctl
show` observed:

```text
MainPID=1822155
ControlGroup=/system.slice/drive9-hostpid-async-probe-20260711-a.service
LoadState=loaded
ActiveState=active
SubState=running
```

After `sleep` exited, `--collect` removed the unit and `LoadState` became
`not-found`.

### Pod cgroup remains unchanged

An exec process in the `hostPID: true` CSI container remained under a
`kubepods.slice/.../cri-containerd-*.scope` cgroup. `hostPID` changes process
visibility but does not detach container children from the Pod cgroup.

This confirms that the long-lived Drive9 process must still be created by host
systemd.

### Evidence limits

1. Direct `systemctl` was tested on both target nodes.
2. Direct asynchronous `systemd-run` was tested on one target node.
3. A harmless `kill(pid, 0)` is safe to use in preflight, but direct TERM/KILL
   has not yet been exercised against a disposable verified process.
4. The full Drive9 mount path has not yet run through a simplified image.
5. Other supported systemd-based Linux distributions have not been tested.

## Proposed Decision

Set `hostPID: true` on the CSI node DaemonSet and treat the complete Pod as a
node-level trusted workload.

The long-lived mount service remains unchanged in purpose:

```text
CSI node Pod (host PID namespace, Pod cgroup)
  -> nsenter host mount namespace + host root
  -> direct host systemd-run
  -> system.slice/drive9-mount-<volume-hash>.service
  -> drive9-csi-launcher
  -> drive9 mount --foreground
```

The host systemd manager, not `hostPID`, moves Drive9 into the host-managed
service cgroup. `hostPID` only removes the PID/D-Bus boundary that required the
short-lived bridge layer.

## Namespace and Filesystem Model

`hostPID: true` provides the host PID namespace, but it does not provide the
host mount namespace or host root filesystem.

Host commands must therefore retain an explicit namespace/root prefix:

```text
nsenter \
  --mount=/proc/1/ns/mnt \
  --root=/proc/1/root \
  --wd=/proc/1/root \
  -- <absolute-host-command> <validated-args...>
```

The proposed end state uses the normal `/proc` mount because it already exposes
the host PID namespace. The `/host-proc` hostPath volume becomes redundant and
should be removed from the final design. A migration may temporarily keep the
existing path to reduce the size of one code change, but there must not be two
permanent host-process path models.

No `hostNetwork` or `hostIPC` setting is required.

## Direct systemctl

All systemctl operations run directly through the host mount/root command:

```text
nsenter <host-mount-root> -- /usr/bin/systemctl <validated-args...>
```

This replaces the current flow:

```text
nsenter
  -> systemd-run --wait --pipe --collect
  -> /usr/bin/systemctl
```

The following behavior remains unchanged:

1. Commands use the absolute host `/usr/bin/systemctl` path.
2. Unit names must match the existing Drive9 unit-name pattern.
3. Queries request only explicit properties.
4. Output parsers reject missing, duplicate, malformed, or unexpected fields.
5. Query ambiguity fails closed.
6. Stop and reset operations require prior unit and process ownership
   validation.

## Direct asynchronous mount launch

The driver directly invokes the production transient service:

```text
nsenter <host-mount-root> -- \
  /usr/bin/systemd-run \
    --service-type=exec \
    --collect \
    --unit=drive9-mount-<volume-hash>.service \
    --description=drive9-csi:<volume-id>:<attempt-id> \
    --property=Restart=no \
    --property=TimeoutStopSec=120s \
    -- \
    /var/lib/drive9-csi/bin/drive9-csi-launcher \
    <env-file> \
    <args-file>
```

The synchronous outer `systemd-run --wait --pipe` bridge is removed.

`Type=exec` remains required so the client only reports success after systemd
has successfully executed the launcher. A successful `systemd-run` return is
still not mount readiness. The existing readiness and ownership promotion must
continue to verify:

1. mountpoint state;
2. process-state file;
3. control socket;
4. systemd unit state and description;
5. `MainPID` and PID start time;
6. exact binary path, argv, UID, cgroup, volume, and staging target.

## Direct host PID signals

The driver can invoke `kill(2)` directly because its process is in the host PID
namespace. Signal transient services are removed.

The execution mechanism changes, but authorization does not. The driver must
retain the following gates before TERM or KILL:

1. signal allowlist: probe `0`, graceful `TERM`, final `KILL`;
2. positive PID validation;
3. PID start-time validation to prevent PID reuse;
4. expected systemd unit description;
5. systemd `MainPID` agreement when a unit is present;
6. exact argv, UID, cgroup, binary path, volume ID, and staging target
   ownership;
7. state-machine permission for the requested signal.

The runtime abstraction should expose a narrow signal operation rather than a
general host-command or shell interface. Preflight uses only `kill(pid, 0)`.

## Preflight

Node startup must fail closed when the manifest and runtime do not satisfy the
new contract.

Required checks are:

1. `/proc/self/ns/pid` and `/proc/1/ns/pid` identify the same PID namespace.
2. `/proc/1/ns/mnt` and `/proc/1/root` can be opened.
3. `nsenter` into the host mount namespace/root can execute `/bin/true`.
4. Direct host `systemctl` can query a known-absent Drive9 unit and return a
   well-formed `not-found` observation.
5. Direct asynchronous host `systemd-run --service-type=exec --collect` can
   execute a harmless absolute command under a unique preflight unit.
6. `kill(1, 0)` succeeds without sending a signal.
7. Existing `/dev/fuse`, installed binary, Drive9 execution, helper, runtime
   directory, and journal checks continue to pass.

There is no compatibility fallback to the bridge path. A Pod missing
`hostPID: true` must fail preflight with a specific configuration error. Keeping
both process-control paths would preserve the complexity this design is meant
to remove.

## State, Recovery, and Cleanup

The durable state model remains unchanged:

1. `starting`, `active`, and `stopping` phases remain authoritative CSI state.
2. The state records `systemdUnit`, PID, PID start time, binary path, startup
   files, volume ID, and staging target.
3. Reconciliation still treats the systemd unit, process, mountpoint,
   process-state file, and control socket as independent evidence.
4. A live ownership mismatch fails closed.
5. `systemctl stop` remains the normal service termination path.
6. Direct verified TERM/KILL remains orphan or timeout cleanup only.
7. `Restart=no`, `TimeoutStopSec=120s`, and `--collect` remain unchanged.

Host PID visibility does not make state-file-only adoption safe and does not
permit adopting an arbitrary process into a new unit.

## Security and Trust Model

### Pod-wide effect

`hostPID` is a Pod-level setting. It applies to:

1. `install-host-binaries` init container;
2. `node-driver-registrar`;
3. `drive9-csi`.

All three can observe host PIDs through their normal `/proc`. Access to process
arguments, environment, file descriptors, root links, and signals is further
controlled by Linux UID, capability, ptrace, procfs, and LSM rules, but PID
namespace isolation no longer protects the host from these containers.

The project namespace already enforces the `privileged` Pod Security level,
and the CSI container already uses privileged mode, `SYS_ADMIN`, hostPath
mounts, bidirectional mount propagation, host `/proc`, host systemd, and
`/dev/fuse`. The incremental trust change for the CSI and installer containers
is therefore limited.

The main incremental trust change is the registrar. The current
`csi-node-driver-registrar:v2.13.0` image config runs as UID 0, and the manifest
does not define a registrar security context. With `hostPID`, the registrar
must be considered part of the node trusted computing base.

If that trust decision is unacceptable, this design must be rejected or the
registrar must be moved to a separate Pod without `hostPID`. Container-level
securityContext settings reduce exploitability but cannot recreate a separate
PID namespace inside one `hostPID` Pod.

### Required compensating controls

#### Tini subreaper

The CSI image currently launches `tini` without subreaper mode. Under
`hostPID`, Tini is not PID 1. The entrypoint must register it as a subreaper:

```text
/usr/bin/tini -s -- /usr/local/bin/drive9-csi
```

This preserves orphan adoption and zombie reaping for the CSI and installer
process trees.

#### Registrar hardening

The registrar image must be pinned to an immutable digest. Its compatibility
with the following minimum security context must be validated:

```yaml
securityContext:
  allowPrivilegeEscalation: false
  capabilities:
    drop: ["ALL"]
  readOnlyRootFilesystem: true
  seccompProfile:
    type: RuntimeDefault
```

This is defense in depth. A root registrar in the host PID namespace remains a
node-trusted process even after capabilities are dropped.

#### ServiceAccount token isolation

The Pod currently uses the `drive9-csi-node` ServiceAccount, whose ClusterRole
can read PersistentVolumes and Secrets. Kubernetes mounts the Pod's Service
Account credentials by default.

The proposed manifest should disable automatic token mounting at Pod level and
mount an explicit projected ServiceAccount token, root CA, and namespace only
into the `drive9-csi` container. The installer and registrar do not require
Kubernetes API credentials.

#### Command and secret constraints

1. Use absolute host executable paths.
2. Never invoke a shell for lifecycle commands.
3. Validate unit names, paths, PIDs, signals, and output formats before use.
4. Keep the Drive9 API key out of argv, systemd unit properties, and
   environment properties visible through systemd.
5. Continue using root-only startup files consumed by the launcher.
6. Remove startup files only after the launcher has read and unlinked them or
   terminal cleanup proves the attempt is no longer live.

## Compatibility and Operational Contract

1. The driver remains Linux-only.
2. Nodes must use systemd and support the currently required transient service
   properties.
3. `hostPID: true` becomes a required manifest contract, not an optional
   optimization.
4. The host mount namespace and host root are still required.
5. The Drive9 mount process remains outside the Pod cgroup in its own host
   systemd service cgroup.
6. The CSI and registrar container processes remain in the Pod cgroup and keep
   Kubernetes resource accounting and termination behavior.
7. The namespace already uses the `privileged` Pod Security level, so the new
   field does not require a higher admission profile.
8. macOS remains a supported local development environment for code quality,
   unit tests, and cross-builds; Linux namespace behavior is validated through
   mocks and targeted dev-cluster probes.

## Deployment and Rollback Contract

The simplified binary requires `hostPID: true`; the new image and manifest must
therefore roll out together as one DaemonSet template revision.

Preflight must reject these split states:

| Image | Manifest | Required behavior |
| --- | --- | --- |
| Old bridge image | Old manifest | Existing supported behavior |
| New direct image | New `hostPID` manifest | Proposed supported behavior |
| New direct image | Old manifest | Fail preflight; do not stage new mounts |
| Old bridge image | New manifest | Rollback-only transient state; not a target configuration |

Rollback restores both the previous image and previous manifest. No automatic
runtime fallback to the bridge implementation is retained.

## Alternatives

| Alternative | Benefits | Costs | Assessment |
| --- | --- | --- | --- |
| Keep the current bridge | Preserves PID isolation for registrar; already validated | Nested systemd launch, transient action units, more failure paths and tests | Safe fallback if Pod-wide host PID trust is rejected |
| `hostPID: true` in the existing Pod | Removes the bridge and matches the node-agent role directly | Makes registrar and init containers host-PID-visible | Proposed, conditional on trust and hardening |
| Split registrar into another DaemonSet | Gives only CSI host PID access | Extra DaemonSet, scheduling, socket ownership, upgrade coordination | Stronger isolation but defeats much of the simplicity goal |
| Static host service or separate node agent | Strong host lifecycle ownership | Requires node provisioning and a second delivery channel | Rejected for CSI Lite |
| Direct container child with `hostPID` | Simple launch | Remains in the Pod cgroup and dies during Pod cleanup | Rejected; violates mount survival |

## Affected Surfaces

The design affects these areas without changing the CSI API contract:

1. `deploy/kubernetes/node.yaml`: host PID contract, redundant host-proc volume,
   registrar hardening, and scoped ServiceAccount credentials.
2. `Dockerfile`: Tini subreaper mode.
3. `internal/driver/systemd.go`: direct systemctl path and removal of the
   synchronous manager bridge.
4. `internal/driver/mount_lifecycle.go`: direct asynchronous systemd-run.
5. `internal/driver/mount_stop.go` and recovery: direct narrow signal
   operation.
6. `internal/driver/host_process.go`: host process paths under `/proc`.
7. `internal/driver/node_preflight.go`: required host PID and direct command
   capability checks.
8. Manifest, command-construction, ownership, recovery, and cleanup unit tests.
9. The rolling-upgrade mount-survival design, which currently documents the
   no-hostPID bridge architecture.

## Acceptance and Validation

### Required implementation acceptance

Implementation acceptance focuses on code quality and unit tests:

1. `make test` passes on the macOS development environment.
2. `make build` produces the Linux CSI binary.
3. Manifest tests require `hostPID: true`, the selected registrar controls,
   token isolation, and removal or intentional retention of host-proc.
4. Command tests prove direct systemctl and direct asynchronous systemd-run do
   not contain an outer bridge.
5. Signal tests prove only `0`, `TERM`, and `KILL` are accepted and all
   ownership gates run before destructive signals.
6. Recovery and cleanup tests preserve the existing fail-closed state matrix.
7. Tests prove API keys never enter argv or systemd properties.
8. The implementation and this design remain consistent.

No Drive9 FUSE baseline, Linux host image, `hdiutil`, or heavy E2E suite is part
of local implementation acceptance.

### Separate targeted runtime validation

Runtime namespace validation is a manual dev-cluster activity, not part of the
image publishing workflow or the local code-quality gate:

1. Node preflight reports the host PID namespace and direct systemd path as
   healthy.
2. One disposable mount enters
   `system.slice/drive9-mount-*.service`, not `kubepods.slice`.
3. Direct systemctl observes the expected description, state, and MainPID.
4. A verified disposable process exercises direct TERM and, if needed, KILL.
5. One CSI DaemonSet rolling replacement confirms the mount PID, PID start
   time, unit, workload Pod UID, and ongoing reads/writes remain unchanged.
6. Cleanup leaves no probe units, startup files, test PVCs, or test Pods.

This is a focused runtime smoke test for behavior that cannot execute on macOS;
it is not a general E2E framework.

## Open Questions

The following questions must be resolved before implementation approval:

1. Is the root node-driver-registrar explicitly accepted as part of the node
   trusted computing base?
2. Which registrar securityContext fields pass against the pinned image and
   host registration directory permissions?
3. Is scoped ServiceAccount token projection mandatory in the same change or a
   separately approved prerequisite?
4. Should `/host-proc` be removed in the first implementation or retained for
   one migration revision before converging on `/proc`?
5. Should the direct signal operation use the Go `kill(2)` syscall abstraction
   or an absolute host `/bin/kill` command? The design prefers the narrower
   syscall abstraction.
6. Which additional systemd-based distributions, if any, must be included in
   compatibility validation beyond the target Amazon Linux 2023 EKS nodes?

## Discussion Gate

Implementation must not begin until this draft is reviewed and the trust,
registrar hardening, ServiceAccount token, and `/proc` questions above are
resolved.
