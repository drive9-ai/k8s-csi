---
title: CSI In-Binary Mount Supervisor Design
status: implemented
date: 2026-08-17
---

## Scope

Every Drive9 FUSE mount started by this repository uses the in-binary foreground
supervisor introduced by Drive9 CLI `dac2d62`.

This design supersedes foreground-worker ownership in
`csi-rolling-upgrade-mount-survival-v2.md` and the custom fallback-sidecar
supervisor in `2026-07-12-csi-direct-mount-strict-helper-removal-design.md`.
Their unrelated state-machine, direct-mount, credential, and release-admission
requirements remain authoritative.

There is no deployed compatibility requirement. Pre-change `--foreground`
mount state and supervisor-less Drive9 binaries are rejected rather than
migrated.

Expected production change: approximately 100 net LoC plus removal of roughly
347 LoC of obsolete supervisor code. Size class: Medium boundary.

## Upstream Contract

Drive9 CLI `dac2d62` adds `--supervise-foreground`. The invoked process remains
in the foreground as a supervisor and starts a child worker with
`--foreground --supervised`.

The supervisor:

1. Publishes authoritative process state with its PID and creation identity.
2. Restarts workers after abnormal exit or repeated local health failures.
3. Uses a bounded restart budget and circuit-open state.
4. Handles TERM or INT by stopping the worker and cleaning the mount.
5. Keeps the control-socket path stable across worker replacement.

## CSI Node Lifecycle

```text
CSI node Pod
  -> host mount namespace and root
  -> transient systemd service
  -> drive9-csi-launcher
  -> drive9 mount --supervise-foreground
  -> replaceable drive9 mount --foreground --supervised worker
  -> /dev/fuse and mount(2)
```

The host launcher and transient systemd service remain unchanged. After the
launcher calls `execve`, the Drive9 supervisor is systemd `MainPID`. CSI durable
state records that supervisor PID, not the worker PID, so worker replacement
does not invalidate CSI ownership.

All newly constructed and persisted mount argv must contain exactly one
`--supervise-foreground`. Worker-only `--foreground`, internal `--supervised`,
`--no-supervise`, duplicates, and boolean variants are rejected.

## Process Ownership

The Drive9 process-state file must identify the supervisor:

| Field | Required value |
| --- | --- |
| `component` | `drive9-fuse-supervisor` |
| `mount_kind` | `fuse` |
| `role` | `supervisor` |
| `supervise` | `true` |
| `supervisor_pid` | Equal to `pid` |
| `supervisor_creation_time` | Equal to `creation_time` |
| `mount_point` | Exact CSI staging target |
| `control_socket` | Exact root runtime socket path |

CSI also requires the recorded creation identity to equal the host
`/proc/<pid>/stat` start time, and retains its existing immutable executable,
systemd cgroup, unit description, and stable-PID checks. A worker PID may change
or temporarily be absent without changing supervisor ownership.

## Stop and Recovery

`NodeUnstageVolume` remains CSI-owned:

1. Drain through the stable Drive9 control socket.
2. Stop the verified systemd unit.
3. Let the Drive9 supervisor stop its worker and clean the mount.
4. Retain CSI's direct normal/lazy kernel-unmount fallback.
5. Verify mount, supervisor PID, systemd unit, process state, control socket, and
   startup files are absent before deleting durable CSI state.

Starting, active, stopping, desired-first recovery, content-addressed binaries,
and binary garbage collection keep their existing state-machine behavior. All
runtime artifact paths now validate only supervisor-owned process state.

## Fallback Sidecar

The fallback manifest directly execs:

```text
drive9 mount --supervise-foreground --stop-timeout=30s
```

The old `drive9-csi supervise-sidecar-mount` command is removed. Drive9 now owns
worker restart and termination cleanup. The 30-second stop bound leaves headroom
inside the Pod's existing 60-second termination grace period.

## Capability Gates

Both the runtime image build and node preflight execute:

```text
drive9 mount --supervise-foreground --direct-mount-strict --help
```

An older CLI therefore fails before mount side effects. Image publication waits
for `dac2d62` to appear as a complete Drive9 release; this repository does not
pin or download an unpublished artifact.

## Validation

Local acceptance requires:

1. Mount argv and durable-state tests for the supervisor-only contract.
2. Process ownership tests for every required supervisor identity field.
3. Lifecycle, recovery, stop, no-state cleanup, and binary-GC tests using
   supervisor-owned process state.
4. Node preflight and Dockerfile capability contracts.
5. Manifest checks proving direct sidecar use and absence of the old wrapper.
6. `make test`, `make build-check`, `make manifest-check`, and `make check`.

Image build and cluster E2E remain deferred until Drive9 CLI `dac2d62` is
published and separately approved for execution.
