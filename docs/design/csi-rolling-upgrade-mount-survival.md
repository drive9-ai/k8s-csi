# CSI DaemonSet Rolling Upgrade — Mount Survival Design

## Problem

When the `drive9-csi-node` DaemonSet is updated (rolling update), the CSI Pod
restarts. Currently, `shutdownNodeMounts()` runs on Pod exit and actively
unmounts all `drive9 mount` processes + staging targets. After restart, recovery
re-mounts the staging targets, but existing business Pods lose their mount
points because:

1. The old staging target is unmounted → kubelet's bind mount (staging → publish
   target) loses its source → business Pod's HostToContainer bind mount goes
   stale (ESTALE/EIO).
2. Recovery creates a new mount on the same staging target path, which
   propagates to the kubelet publish target, but **not** into already-running
   business Pods — HostToContainer propagation is established at Pod creation
   time and is one-way.

## Root Cause

`shutdownNodeMounts()` does not distinguish between "CSI Pod rolling upgrade"
and "intentional volume cleanup." It unconditionally calls `drive9 umount` +
`unmountPath` for every recorded mount, destroying the FUSE connection.

## Solution

### Core Idea

Run `drive9 mount` processes in the **host mount namespace** AND in a
**host-managed systemd transient service** so they are fully decoupled from the
CSI Pod's container lifecycle. The CSI driver process never actively unmounts on
exit; all mount cleanup is driven by the CSI lifecycle API (`NodeUnstageVolume`).

### Why both nsenter and systemd-run are required

- **`nsenter --mount`** changes the mount namespace so the FUSE mount happens
  directly on the host filesystem. But nsenter alone does NOT move the process
  out of the CSI container's cgroup — kubelet's cgroup cleanup on Pod deletion
  would still kill it.
- **`systemd-run` (transient service)** places the process into a host-managed
  systemd unit (`system.slice/drive9-mount-<vol>.service`), outside the CSI
  Pod's cgroup hierarchy. This is what makes the process survive Pod deletion.

### Why transient service, not scope

`systemd-run --scope` executes the command synchronously — `systemd-run`
itself blocks until the scoped process exits. With `drive9 mount --foreground`,
this means the CSI `NodeStageVolume` goroutine would block forever.

`systemd-run` without `--scope` (default mode) creates a **transient service
unit**. With `--service-type=exec`, `systemd-run` returns as soon as the
service's main process calls `exec()` successfully (the `ExecStart` binary is
running). This gives CSI a clear "process launched" signal without blocking.

The unit type is `.service` (not `.scope`). All unit naming, cgroup
verification, cleanup commands, and e2e expectations use `.service` throughout.

### Support matrix and prerequisites

**Supported**: Linux nodes with systemd as init system (Ubuntu, Amazon Linux,
CentOS, RHEL, Flatcar, Bottlerocket). This covers >95% of production K8s nodes.

**Not supported**: Non-systemd nodes (Alpine-based, custom init). These will
**fail fast** at mount time with a clear error:
`"drive9-csi: host systemd required for mount process lifecycle management"`.

**Security posture**: The CSI node plugin runs with `privileged: true` +
`SYS_ADMIN` + host kubelet path + host `/proc` access. This is node-root level
capability, which is the standard security model for CSI FUSE drivers (same as
JuiceFS CSI, s3-csi, etc.). Code paths are restricted to:
- `/host-proc/1/ns/mnt` — enter host mount namespace
- `systemd-run` / `systemctl` — manage drive9 mount services only
- No arbitrary host `/proc` operations

### Design

#### 1. Startup preflight

On CSI driver startup (before accepting any gRPC calls), run preflight checks:

```
1. Open /host-proc/1/ns/mnt
   Failure: "host /proc not mounted at /host-proc or PID 1 namespace inaccessible"

2. nsenter --mount=/host-proc/1/ns/mnt -- /bin/true
   Check: exit code == 0
   Failure: "nsenter into host mount namespace failed (exit=%d): %s"

3. nsenter --mount=/host-proc/1/ns/mnt -- \
       systemd-run --wait --collect --unit=drive9-preflight -- /bin/true
   Check: exit code == 0 (--wait blocks until /bin/true exits, --collect
   auto-removes the unit on completion, exit code reflects the command's
   exit status, not systemd-run's own status)
   Note: preflight uses --wait (acceptable for short-lived /bin/true).
   Production mounts use --service-type=exec without --wait.
   Failure classification:
     - systemd-run not found: "host does not have systemd"
     - D-Bus connection refused: "host systemd D-Bus inaccessible"
     - unit creation failed: "host systemd rejected transient unit"
     - /bin/true failed (exit != 0): "preflight command failed in transient unit"
```

If any check fails → log error with the specific failure classification,
set driver to degraded mode (reject `NodeStageVolume` with
`FAILED_PRECONDITION` and actionable error message). Do not silently fall
back to in-container mount.

#### 2. Host-namespace + host-cgroup mount process

Change `startDrive9Mount` to launch `drive9 mount` via `nsenter` + `systemd-run`
(transient service):

```
nsenter --mount=/host-proc/1/ns/mnt -- \
    systemd-run --service-type=exec \
    --unit=drive9-mount-<hex-volume-id> \
    --remain-after-exit=false -- \
    /var/lib/drive9-csi/run/<hex-volume-id>.sh
```

`systemd-run` returns as soon as the service's main process has successfully
called `exec()`. This is the "process launched" signal. CSI then proceeds to
`waitForMount` to poll for FUSE readiness.

The mount process:
- Runs in the host's mount namespace (FUSE mount directly on host)
- Lives in a host systemd service cgroup (`system.slice/drive9-mount-<vol>.service`)
- Survives CSI Pod restart — kubelet cleans Pod cgroup, not host systemd services

##### Host PID discovery and identity (B6)

After `systemd-run` returns, CSI cannot use `cmd.Process.Pid` — that is the
`nsenter`/`systemd-run` wrapper PID, not the long-running `drive9 mount` PID.

**Source of truth**: `drive9 mount --foreground` writes a **pidfile** and
creates a **control socket** on hostPath-backed paths that persist across
CSI Pod restarts.

**Path encoding and ownership**:
- Volume IDs are encoded using a **collision-resistant** scheme: hex-encode
  the volume ID (`hex.EncodeToString([]byte(volumeID))`). This guarantees
  no two distinct volume IDs map to the same filename, unlike the existing
  `safeFileName()` which can collide (e.g., `a/b` and `a_b` → same output).
  The hex encoding is used for pidfile, env file, wrapper script, and systemd
  unit naming. Mount state JSON files continue to use the existing
  `safeFileName()` for backward compatibility.
- Pidfile: `/var/lib/drive9-csi/run/<hex-volume-id>.pid` (mode `0600`, owned
  by root). CSI passes `--pid-file` flag to `drive9 mount`.
- Control socket: `drive9 mount` currently writes the control socket to the
  staging target directory (e.g., `<staging-target>/.drive9.sock`). This is
  the **source of truth** — CSI reads the socket path from the staging target
  dir, not from the run dir. No change to Drive9 CLI needed. Mount state
  records the resolved control socket path for recovery.
- Run dir: `/var/lib/drive9-csi/run/` (mode `0700`, created by CSI driver on
  startup)
- **Stale file handling**: before starting a new mount, CSI checks for existing
  pidfile/socket at the expected path. If found, verify PID is alive and
  matches the expected volume's staging target. If stale (PID dead or wrong
  volume), delete pidfile/socket before proceeding.
- **Cross-volume safety**: pidfile path includes the hex-encoded volume ID,
  so different volumes cannot collide. Before trusting a pidfile, CSI
  performs a three-way ownership check:
  1. Parse `/host-proc/<pid>/cmdline` (NUL-separated argv) and verify the
     exact staging target path appears as the final positional argument
     (not a substring match — exact argv element comparison)
  2. Verify PID belongs to the expected systemd service by checking
     `/host-proc/<pid>/cgroup` contains the expected unit name
  3. Verify `PIDStartTime` matches the recorded value (guards against
     PID reuse after process death)

**PID resolution sequence** (after `waitForMount` confirms mount is ready):
1. Read PID from pidfile at `/var/lib/drive9-csi/run/<hex-volume-id>.pid`
2. Read PID startTime from `/host-proc/<pid>/stat` (field 22)
3. Parse `/host-proc/<pid>/cmdline` (NUL-separated), verify the exact
   staging target path as the final argv element
4. Verify `/host-proc/<pid>/cgroup` contains expected systemd unit name
5. Record `PID`, `PIDStartTime`, `systemdUnit`, `binaryPath`,
   `controlSocketPath` in mount state

**PID verification** (`pidMatchesState`):
- All PID-based checks (`pidStartTime`, `pidAlive`, `/proc/<pid>/cgroup`,
  `/proc/<pid>/exe`) read from **`/host-proc/<pid>/...`**, not `/proc/<pid>/...`
- This is mandatory because the mount process runs in host PID namespace
  (via nsenter) and the CSI driver does not use `hostPID: true`
- Reused-PID protection: `pidStartTime` comparison catches PID reuse —
  kernel guarantees monotonically increasing startTime per boot

##### Environment and secret propagation (B8)

`drive9 mount` requires `DRIVE9_SERVER` and `DRIVE9_API_KEY` env vars.

**Mechanism**: Root-only environment file, not `--setenv`.

Using `systemd-run --setenv=DRIVE9_API_KEY=...` puts the secret in the
`systemd-run` process argv, visible to any node-root process via
`/proc/<pid>/cmdline` during launch. While the CSI node plugin already
runs as node-root (privileged container), minimizing secret surface is
preferred.

Instead, use a root-owned wrapper script that reads an env file and execs:
```
1. Write /var/lib/drive9-csi/run/<hex-volume-id>.env (mode 0600, root:root):
     DRIVE9_SERVER=<server>
     DRIVE9_API_KEY=<key>

2. Write /var/lib/drive9-csi/run/<hex-volume-id>.sh (mode 0700, root:root):
     #!/bin/sh
     set -eu
     . /var/lib/drive9-csi/run/<hex-volume-id>.env
     export DRIVE9_SERVER DRIVE9_API_KEY
     exec /var/lib/drive9-csi/bin/drive9 mount --foreground ...
   (CSI generates this script with the exact mount args)

3. Launch:
     nsenter --mount=/host-proc/1/ns/mnt -- \
       systemd-run --service-type=exec \
       --unit=drive9-mount-<hex-volume-id> -- \
       /var/lib/drive9-csi/run/<hex-volume-id>.sh

   This keeps secrets out of systemd-run's argv entirely. The wrapper
   script sources the env file, exports the vars, then execs drive9.

4. Cleanup of .env and .sh files runs on BOTH success and failure paths:
   - Success: after mount is confirmed ready (pidfile + isMountPoint),
     delete both files. The mount process already has the env vars in
     its address space.
   - Failure (timeout, crash, bad credentials): cleanup runs in the
     same defer/finally block that handles service stop and state cleanup.
     Files are deleted after the service is stopped/confirmed dead.
   In all cases, .env and .sh files are ephemeral — they must not persist
   beyond the mount startup attempt.
```

**Exposure constraints under node-root threat model**:
- Env file exists only during mount startup (seconds), mode 0600
- After deletion, secrets live only in `/proc/<pid>/environ` (readable by
  root only, which is acceptable under node-root threat model)
- `systemctl show` does not expose the env vars
- Journal/logs: `drive9 mount` must not log the API key
- CSI driver logs the launch command with `DRIVE9_API_KEY=<redacted>`
- **E2e check**: after mount ready, verify env file deleted, verify
  `systemctl show` output does not contain the API key

**Secret rotation**: if the K8s Secret changes, `NodeUnstageVolume` +
`NodeStageVolume` re-mount with the new credentials. Old mount processes
continue with old credentials until cleaned up.

##### Readiness and failure attribution (B9)

**Readiness detection**:
1. `systemd-run --service-type=exec` returns once `drive9 mount` has been
   exec'd successfully (the process is running). This confirms the service
   unit is active, but does NOT mean FUSE is ready.
2. `waitForMount(stagingTarget, timeout)` polls `isMountPoint(stagingTarget)`
   until the FUSE mount appears (same as today, no `processDone` channel)
3. If mount appears → read pidfile → record state → clean up .env/.sh →
   return success
4. If timeout → check service status via
   `systemctl is-active drive9-mount-<hex-volume-id>.service`:
   - Service inactive/failed: `drive9 mount` crashed before mounting.
     Read logs via `journalctl --unit=drive9-mount-<hex-volume-id>.service --no-pager -n 50`
     for error attribution. Clean up .env/.sh files, return error.
   - Service active but no mount: `drive9 mount` is running but not mounting.
     Stop service, clean up .env/.sh files, return error with log snippet.

**State write timing**: mount state JSON is written ONLY after ALL of:
- `isMountPoint(stagingTarget)` returns true
- Pidfile exists and PID is alive (verified via `/host-proc/<pid>/stat`)
- `binaryPath`, `systemdUnit` are known

This prevents partial state from persisting if `drive9 mount` crashes
during startup.
Cleanup via `NodeUnstageVolume` → `drive9 umount` (control socket), with
`systemctl stop` as fallback.

#### 3. Systemd unit naming and idempotency

**Canonical volume ID encoding**: `hex.EncodeToString([]byte(volumeID))`.
This single encoding is used for ALL per-volume artifacts:
- Systemd unit: `drive9-mount-<hex-volume-id>.service`
- Pidfile: `/var/lib/drive9-csi/run/<hex-volume-id>.pid`
- Env file: `/var/lib/drive9-csi/run/<hex-volume-id>.env`
- Wrapper script: `/var/lib/drive9-csi/run/<hex-volume-id>.sh`

Hex encoding is collision-free (bijective) and systemd-safe (only `[0-9a-f]`).
No `systemd-escape` needed. Mount state JSON files continue to use the
existing `safeFileName()` for backward compatibility with kubelet conventions.

Mount state JSON records `systemdUnit` field for reverse lookup.

**Idempotency** (`NodeStageVolume` retry):
- If service already exists AND pid matches state AND mount point exists →
  return success (idempotent)
- If service exists but pid doesn't match → `systemctl stop` service, then
  re-mount
- If service doesn't exist but state file exists → state is stale, clean up
  and re-mount

**Recovery split-state handling** (on CSI driver restart):

| Service exists | PID matches | Mount exists | Control socket | Action |
|---|---|---|---|---|
| yes | yes | yes | yes | Skip (healthy) |
| yes | yes | yes | no | Stop service, unmount, re-mount |
| yes | yes | no | - | Stop service, re-mount |
| yes | no | - | - | Stop service, clean state, re-mount |
| no | yes | yes | yes | Stop + re-mount: kill PID via control socket (`drive9 umount`), kernel unmount staging target, then re-mount with a new service. Adopting an orphan PID into a new systemd service is unreliable; clean re-mount is simpler and guaranteed correct. |
| no | yes | no | - | Kill PID, clean state, re-mount |
| no | no | yes | - | Kernel unmount, clean state, re-mount |
| no | no | no | - | Clean state (nothing to recover) |

#### 4. Host `/proc` access (without hostPID)

Mount host `/proc` into the CSI container for `nsenter` namespace entry:

```yaml
volumeMounts:
  - name: host-proc
    mountPath: /host-proc
    readOnly: true
volumes:
  - name: host-proc
    hostPath:
      path: /proc
      type: Directory
```

This avoids `hostPID: true` which would expose all host processes to the
container. The CSI driver only accesses `/host-proc/1/ns/mnt` for namespace
entry.

#### 5. Versioned binary installation (init container)

Install the `drive9` binary to a versioned path on the host:

```yaml
initContainers:
  - name: install-drive9
    image: ghcr.io/drive9-ai/drive9-csi:<tag>
    command:
      - sh
      - -c
      - |
        set -euo pipefail
        # Determine unique version identifier (fail if unavailable)
        VER=$(drive9 version --short-sha 2>/dev/null || true)
        if [ -z "$VER" ] || [ "$VER" = "unknown" ]; then
          # Fallback to content hash for auditability
          VER=$(sha256sum /usr/local/bin/drive9 | cut -c1-12)
        fi
        DEST="/host-state/bin/drive9-${VER}"
        # Atomic install: write to temp, rename (never overwrite existing)
        if [ ! -f "$DEST" ]; then
          TMPF=$(mktemp /host-state/bin/.drive9-install-XXXXXX)
          cp /usr/local/bin/drive9 "$TMPF"
          chmod 755 "$TMPF"
          mv "$TMPF" "$DEST"
        fi
        ln -sf "drive9-${VER}" /host-state/bin/drive9
    volumeMounts:
      - name: state-dir
        mountPath: /host-state
```

Binary layout on host (`/var/lib/drive9-csi/bin/`):
- `drive9-<sha>` — versioned binary (immutable once written, never overwritten)
- `drive9` — symlink to current version (updated on each CSI Pod start)
- Install is atomic (temp file + rename) to prevent partial writes
- Version is determined from `drive9 version --short-sha` or content hash;
  never falls back to an un-auditable name

Mount state records `binaryPath` for auditability. Old mount processes hold
an open fd to the old binary inode; symlink update doesn't affect them.

**Binary GC**: Runs **after** the full recovery scan completes (not
interleaved with mount recovery). Scan all state files for referenced
`binaryPath` values. Remove any `drive9-*` binaries in the bin directory that
are not referenced by any live mount state. This ordering prevents a
concurrent recovery restart from GC-ing a binary still being referenced by
a mount being re-adopted.

#### 6. Remove shutdown unmount on SIGTERM

Remove `shutdownNodeMounts()` from the SIGTERM/exit path. The CSI driver
process exits cleanly (stops gRPC server, deregisters from kubelet) without
touching mount processes.

SIGTERM is sent on: rolling update, `kubectl delete pod`, Pod eviction,
preStop hook timeout. In all cases, mount processes should survive.

#### 7. Cleanup sequence (NodeUnstageVolume)

Fixed ordering — each step runs regardless of previous step's result:

```
1. drive9 umount via control socket (30s timeout)
2. Verify service: systemctl is-active drive9-mount-<hex-volume-id>.service
   If still active: systemctl stop drive9-mount-<hex-volume-id>.service (10s timeout)
   (This runs regardless of step 1 result — control socket umount may
   succeed but leave the service active)
3. Verify: isMountPoint(stagingTarget) == false
   If still mounted: kernel unmount (unix.Unmount)
   If busy: lazy unmount (MNT_DETACH) — existing open fds in business
   Pods continue to work until closed (no ESTALE mid-operation)
4. Verify: PID dead (pidMatchesState == false)
   If still alive: SIGKILL + wait 5s
5. Delete mount state file (only if all above verifications pass)
```

**Terminal state**: mount gone + PID gone + service gone + state file gone.

**State file deletion rule**: state file is deleted ONLY after all of the
following are confirmed:
- `isMountPoint(stagingTarget)` returns false
- PID is dead (`pidMatchesState` returns false)
- Service is inactive or doesn't exist

If any of these conditions is NOT met after all cleanup attempts, the state
file is **preserved** and `NodeUnstageVolume` returns an error. This ensures
the orphaned resource remains manageable by subsequent recovery or
`NodeUnstageVolume` retries. Never delete state to "clean up" an
unresolvable orphan — that makes it permanently unmanageable.

#### 8. Recovery (minimal change to existing code)

`recoverNodeMounts()` enhanced with systemd service awareness:
- Check `pidMatchesState(state)` AND service active → skip (healthy)
- Otherwise → run split-state matrix (section 3 above)
- After successful re-mount → `repairPublishTargets` (existing code, unchanged)

#### 9. Priority class for drain ordering

```yaml
spec:
  template:
    spec:
      priorityClassName: system-node-critical
```

Ensures CSI driver Pod is evicted last during `kubectl drain`, so
`NodeUnstageVolume` calls from consumer Pod evictions are served by a running
CSI driver.

### Mount propagation

With nsenter, the FUSE mount originates in the host mount namespace:
- Staging target: `/var/lib/kubelet/plugins/kubernetes.io/csi/...` — host path
- Kubelet bind mount (staging → publish target): both in host namespace
- Business Pod mount: HostToContainer bind mount established at Pod creation

The CSI Pod still needs `mountPropagation: Bidirectional` on the kubelet-dir
volume mount so the CSI driver (inside the container) can see host mount state
for `isMountPoint()` checks.

### Resource isolation

Mount processes in host systemd services are not constrained by CSI Pod cgroup
limits. This is the standard model for production FUSE mount daemons.

V1: `drive9 mount` manages its own cache memory via `--cache-dir` configuration.
Future: per-service resource limits via `systemd-run --property=MemoryMax=...` and
OOM score adjustment.

### Cross-repo Drive9 CLI requirements

This design depends on `drive9 mount` (in `mem9-ai/drive9`) supporting:

| Capability | Current status | Required for this design |
|---|---|---|
| `--foreground` flag | Exists | Keep mount process in foreground (no double-fork) |
| Pidfile at configurable path | Exists: `--pid-file` flag writes PID to given path | CSI passes `--pid-file /var/lib/drive9-csi/run/<vol>.pid` |
| Control socket | Exists: written to staging target dir (`<staging>/.drive9.sock`) | CSI reads from staging target dir (no change needed) |
| `drive9 umount` via control socket | Exists | No change needed |
| `drive9 version` via control socket | **Not yet implemented** | Add version query to control protocol (paired PR in drive9) |

If `drive9 version` via control socket is not available for V1, the e2e
binary version check falls back to `/host-proc/<pid>/exe` readlink only.
The control socket version query is a nice-to-have, not a blocker.

### Changes required

| File | Change |
|---|---|
| `internal/driver/mount_linux.go` | `startDrive9Mount`: nsenter + systemd-run (transient service); hex-encoded unit naming; PID discovery from pidfile; env via root-only wrapper script (no argv secrets) |
| `internal/driver/mount_linux.go` | `drive9Umount` / cleanup: fixed 5-step sequence with systemctl stop fallback |
| `internal/driver/mount_linux.go` | `pidStartTime`, `pidMatchesState`, `pidAlive`: read from `/host-proc/<pid>/...` instead of `/proc/<pid>/...` |
| `internal/driver/driver.go` | Startup preflight; remove `shutdownNodeMounts()` from SIGTERM handler |
| `internal/driver/node_recovery.go` | Split-state matrix handling; systemd service liveness check |
| `deploy/kubernetes/node.yaml` | Init container (versioned binary), host-proc volume, priorityClassName |
| Mount state JSON | Add `binaryPath` and `systemdUnit` fields |

### Verification plan (e2e)

1. **Preflight**: CSI driver starts, passes all preflight checks, logs success
2. **Mount + cgroup**: `NodeStageVolume` creates mount via systemd service; verify
   `/host-proc/<pid>/cgroup` shows `system.slice/drive9-mount-*.service`, NOT
   `kubepods/` (all `/proc/<pid>` checks in e2e run against `/host-proc/<pid>`
   since mount process is in host PID namespace)
3. **Rolling update**: `kubectl rollout restart ds/drive9-csi-node` — business
   Pod maintains open fd loop (read/write/fsync), verify mount PID/startTime/
   mount id unchanged
4. **Pod delete**: `kubectl delete pod drive9-csi-node-xxx` — same verification
   as rolling update
5. **New Pod mount**: create new PVC + Pod after upgrade, confirm new binary
   version by checking: (a) mount state `binaryPath` points to new versioned
   path, (b) `/host-proc/<pid>/exe` of the new mount process resolves to the
   new binary, (c) `drive9 version` via control socket reports the new version
   (if available, otherwise skip)
6. **NodeUnstageVolume**: delete all consumer Pods → mount cleaned up, service
   stopped, state deleted, mount point gone
7. **Mount crash**: `kill -9` mount PID → recovery re-mounts, service recreated
8. **Idempotency**: call `NodeStageVolume` twice for same volume → second call
   returns success without creating duplicate mount
9. **Binary GC**: after all old mounts cleaned, old binary removed
10. **Node drain**: `kubectl drain` — consumer Pods evicted first, mounts
    cleaned via `NodeUnstageVolume`, CSI Pod evicted last
11. **Secret hygiene (success)**: after mount ready, verify .env and .sh files
    deleted, verify `systemctl show drive9-mount-<hex-volume-id>.service` does
    not contain API key, verify CSI driver logs contain
    `DRIVE9_API_KEY=<redacted>` not the real key
12. **Secret hygiene (failure)**: trigger a mount failure (e.g., bad
    credentials), verify .env and .sh files are still cleaned up after the
    failed mount attempt, verify no secret files persist under
    `/var/lib/drive9-csi/run/`
