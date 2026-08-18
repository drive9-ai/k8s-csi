---
title: CSI DaemonSet Rolling Upgrade — Mount Survival Design
status: implemented-validation-only
---

<!-- markdownlint-disable MD013 -->

## Status

The core mount-survival implementation landed in `92b52a8`. Manually published
images remain validation-only: the N/N-1 bidirectional cache/writeback
compatibility gate below must land and pass before a fallback-capable image is
release-admitted.

## Supersession

The helper-specific image, installer, preflight, PATH, fallback, and cleanup
requirements in this document are superseded by
`docs/design/2026-07-12-csi-direct-mount-strict-helper-removal-design.md`.
The foreground-worker ownership model is superseded by
`docs/design/2026-08-17-csi-in-binary-mount-supervisor-design.md`.
The remaining mount-survival, ownership, state-machine, secret, and
release-admission invariants here remain authoritative.

## Problem

When the `drive9-csi-node` DaemonSet is updated (rolling update), the CSI Pod
restarts. Before this design, `shutdownNodeMounts()` ran on Pod exit and actively
unmounted all `drive9 mount` processes + staging targets. After restart, recovery
re-mounted the staging targets, but existing business Pods lost their mount
points because:

1. The old FUSE connection is destroyed. Existing staging, publish, and
   business-Pod mount references still point at the old mount and begin
   returning filesystem errors such as EIO or ENOTCONN.
2. Recovery creates a new mount on the same staging target and repairs the host
   publish target, but an already-running business Pod normally has the default
   `mountPropagation: None` (`rprivate`). The replacement host mount therefore
   does not propagate into that Pod's existing mount namespace.

## Root Cause

The former `shutdownNodeMounts()` path did not distinguish between "CSI Pod
rolling upgrade" and "intentional volume cleanup." It unconditionally stopped
and unmounted every recorded mount, destroying the FUSE connection.

## Solution

### Core Idea

Run `drive9 mount` processes in the **host mount namespace** AND in a
**host-managed systemd transient service** so they are fully decoupled from the
CSI Pod's container lifecycle. The CSI driver process never actively unmounts on
exit. Normal intentional teardown is driven by the CSI lifecycle API
(`NodeUnstageVolume`); startup recovery may also clean verified inconsistent
state.

### Scope and version semantics

V1 provides two separations:

1. A CSI Node Pod restart or rolling update does not stop an existing Drive9
   FUSE process.
2. The Drive9 CLI version packaged with a new CSI Node image becomes the
   node-local **desired binary**, but does not automatically replace existing
   FUSE processes.

The CSI Node init container installs the packaged Drive9 CLI at a
content-addressed path and atomically updates the node-local `drive9` symlink.
New mounts resolve and use that desired path. Each existing mount remains pinned
to the `binaryPath` recorded when it started, so multiple Drive9 CLI versions
may legitimately run on one node after a CSI rollout.

V1 intentionally does not rebuild a **healthy** existing mount merely because
its active binary differs from the node-local desired binary. Applying the
desired binary to a healthy mount requires a future explicit
`kubectl drive9 mount rebuild` operation. That operation is independent of
`NodeUnstageVolume`; rolling application Pods on one node may keep at least one
publish target alive and therefore may never cause kubelet to issue
`NodeUnstageVolume`.

If an active FUSE process has already been lost and CSI must create a
replacement process anyway, recovery uses **desired-first** semantics: it first
tries the node-local desired binary, and falls back to the previously active
binary only after the failed desired attempt has been fully cleaned. A
successful desired attempt becomes the new active `binaryPath`. This piggybacks
a CLI upgrade on an unavoidable process replacement without introducing a
separate interruption for a healthy mount.

#### First-rollout and release-admission boundary

V1 assumes the node has no prior Drive9 CSI deployment, live pre-V1 mount, or
pre-V2 mount-state file. Before the first V1 rollout, operators must drain and
unstage mounts with the old driver and begin with a clean CSI mount-state
directory. Only mounts created by this implementation are guaranteed to survive
subsequent CSI Node rollouts; V1 does not adopt an old Pod-cgroup mount into a
host systemd service.

If V1 nevertheless finds a pre-V2 state record, it treats the record as invalid,
preserves it for inspection, and refuses recovery, overwrite, or automatic
cleanup. A live mount, process, or systemd unit without matching valid V2
durable state is likewise not adopted or stopped automatically. The affected
node RPC fails closed and the operator must halt the rollout, reinstall a
compatible old CSI Node if needed, drain the workload, and perform explicit
cleanup before retrying V1.

Fallback recovery has two delivery phases:

1. Implementation and manually dispatched immutable validation images may
   exercise fallback only in non-production validation. Such an image is not
   release-admitted.
2. The N/N-1 bidirectional cache/writeback compatibility gate described below
   must land and pass before any fallback-capable image is release-admitted. If
   release admission must proceed without that gate, fallback must be disabled
   first and a desired-binary launch failure ends in Degraded. Shipping active
   fallback and relying on a future compatibility gate is not allowed.

Drive9 FD handoff is not a V1 dependency. A future rebuild can use FD handoff
when supported, or a disruptive stop-and-remount fallback. The latter can
restore host staging state immediately, but transparent path recovery inside an
already-running workload Pod depends on the subtree publish layout from
[`drive9-ai/k8s-csi#24`](https://github.com/drive9-ai/k8s-csi/pull/24). That PR
is planned as a follow-up after this lifecycle design is implemented. Until it
is merged, a disruptive rebuild under the legacy root-publish layout may require
recreating affected workload Pods. Even with subtree recovery, file and
directory descriptors already opened against the dead FUSE connection do not
move to the replacement connection and must be reopened.

### Why both nsenter and systemd-run are required

- **`nsenter --mount`** enters the host mount namespace so mount operations are
  applied to the host mount table. `--root=/host-proc/1/root` and
  `--wd=/host-proc/1/root` separately select the host root and working
  directory, so `systemd-run`, the systemd socket, host tools, and
  `/var/lib/drive9-csi` resolve in the host filesystem. Entering the mount
  namespace alone does not change the process root. These options still do not
  move the client process into the host PID namespace or out of the Pod cgroup.
- **Host `systemd-run` (transient service)** asks the host systemd manager to
  create the long-running process. The host manager starts it in the host
  namespaces and a host-managed unit
  (`system.slice/drive9-mount-<vol>.service`), outside the CSI Pod's cgroup.
  This is what makes the process survive Pod deletion.

### Why transient service, not scope

`systemd-run --scope` executes the command synchronously — `systemd-run` itself
blocks until the scoped process exits. With `drive9 mount --foreground`, this
means the CSI `NodeStageVolume` goroutine would block forever.

`systemd-run` without `--scope` (default mode) creates a **transient service
unit**. With `--service-type=exec`, `systemd-run` returns as soon as the service
manager successfully execs the configured `ExecStart` binary. In this design
that binary is `drive9-csi-launcher`, not yet `drive9`. This gives CSI a clear
"launcher started" signal without blocking; the readiness check below detects a
subsequent launcher or Drive9 failure.

The unit type is `.service` (not `.scope`). All unit naming, cgroup
verification, cleanup commands, and e2e expectations use `.service` throughout.

### Support matrix and prerequisites

**Supported**: Linux nodes that expose all required host capabilities. Support
is feature-based, not inferred from a distribution name:

- systemd 240 or newer, or a distribution build that provides `Type=exec`,
  transient `TimeoutStopSec`, and `systemd-run --collect`
- `systemd-run`, `systemctl`, and `journalctl`
- `/dev/fuse` readable and writable by the host service
- writable `/var/lib/drive9-csi`
- writable host runtime directory `/run/drive9-csi`

Drive9 and the launcher must be static Linux binaries for the target
architecture. The current Drive9 CLI build uses `CGO_ENABLED=0`; the CSI image
build must verify both have the expected ELF machine and no ELF interpreter
(`PT_INTERP`). The packaged and installed Drive9 CLI must accept
`mount --direct-mount-strict --help`; the runtime image and host installer have
no FUSE-helper contract.

V1 does not set `User=` on the transient unit, so the host system manager runs
the mount service as root. The strict direct-mount path applies `--allow-other`
through `mount(2)` and does not depend on `/etc/fuse.conf`. Dropping privileges
would require a separate design and runtime gate.

Nodes missing any required capability fail during startup preflight with a
specific error. A non-systemd node reports:
`"drive9-csi: host systemd required for mount process lifecycle management"`.

**Security posture**: The CSI node plugin runs with `privileged: true`
(including `CAP_SYS_ADMIN` and `CAP_SYS_CHROOT`) + host kubelet path + host
`/proc` access. This is node-root level capability, which is the standard
security model for CSI FUSE drivers (same as JuiceFS CSI, s3-csi, etc.). Host
`/proc` access paths used by this design:

- `/host-proc/1/ns/mnt` — enter host mount namespace (nsenter)
- `/host-proc/1/root` — select the host root and working directory
- `/host-proc/<pid>/stat` — read PID start time for reuse detection
- `/host-proc/<pid>/cmdline` — verify mount process argv ownership
- `/host-proc/<pid>/cgroup` — verify process belongs to expected systemd unit
- `/host-proc/<pid>/exe` — readlink to verify binary ownership and version
- `systemd-run` / `systemctl` / `journalctl` — manage or inspect Drive9 mount
  services only

Host `/proc` access is limited to host PID 1 for mount-namespace/root entry and
to mount PIDs obtained from Drive9 process state and verified by the four-way
ownership check. No enumeration or scanning of arbitrary host PIDs is performed.
Following `/host-proc/1/root` gives this already-privileged plugin access to the
host filesystem; it is an explicit node-root trust boundary, not an additional
sandbox.

### Design

#### 1. Startup preflight

On CSI **Node** startup (before accepting Node RPCs), run preflight checks. The
controller deployment uses the same binary but does not manage host mounts and
must not require host `/proc`, systemd, `/dev/fuse`, or installed host binaries.

```text
1. Open /host-proc/1/ns/mnt and /host-proc/1/root
   Failure: "host /proc not mounted at /host-proc or PID 1 namespace/root inaccessible"

2. nsenter --mount=/host-proc/1/ns/mnt \
       --root=/host-proc/1/root --wd=/host-proc/1/root -- /bin/true
   Check: exit code == 0
   Failure: "nsenter into host mount namespace/root failed (exit=%d): %s"

3. nsenter --mount=/host-proc/1/ns/mnt \
       --root=/host-proc/1/root --wd=/host-proc/1/root -- \
       systemd-run --service-type=exec --wait --collect \
       --unit=drive9-preflight-<random-hex> -- /bin/true
   Check: exit code == 0 (--wait blocks until /bin/true exits, --collect
   auto-removes the unit on completion, exit code reflects the command's
   exit status, not systemd-run's own status)
   Note: preflight uses --wait (acceptable for short-lived /bin/true).
   Production mounts use --service-type=exec without --wait.
   Failure classification:
     - systemd-run not found:
       "host systemd-run client unavailable or PATH misconfigured"
     - D-Bus connection refused: "host systemd D-Bus inaccessible"
     - unit creation failed: "host systemd rejected transient unit"
     - /bin/true failed (exit != 0): "preflight command failed in transient unit"

4. In the same host mount-namespace/root context, verify host FUSE/runtime
   prerequisites:
   - /dev/fuse is a readable/writable character device
   - systemctl and journalctl are executable
   - ensure `/run/drive9-csi` is the expected hostPath-backed directory, reject
     a symlink or other file type, normalize root ownership/mode 0700, and
     verify it is writable

5. In that host context, verify drive9 and drive9-csi-launcher exist and are
   executable:
   stat /var/lib/drive9-csi/bin/drive9
   stat /var/lib/drive9-csi/bin/drive9-csi-launcher
   Resolve the drive9 symlink and require basename drive9-[0-9a-f]{64}; verify
   the target's SHA-256 equals the filename digest. Then run
   `drive9 mount --direct-mount-strict --help` in a short-lived host transient
   service with a unique preflight unit name. Check: both files exist and are
   executable, the content-addressed target is valid, and host systemd can
   execute a strict-capable Drive9 successfully
   Failure classification:
     - missing file: "host Drive9 binaries missing — init container may have failed"
     - bad link/hash: "host Drive9 content-addressed binary validation failed"
     - execution failure: "host systemd cannot execute Drive9 binary"
```

Each command in steps 2-5 is executed through the same
`nsenter --mount=/host-proc/1/ns/mnt --root=/host-proc/1/root --wd=/host-proc/1/root`
prefix. Step 1 is the container-side prerequisite that opens the namespace and
root handles used by those commands.

If any check fails, log the specific failure classification and retain it as a
named unavailable capability. The Node service still starts in degraded mode;
preflight failure is not a global RPC rejection switch. RPCs apply the following
state-first policy:

1. **Healthy `NodeStageVolume`**: after request identity and the local four-way
   ownership, mount, process-state, and control-socket checks pass, return
   success. Do not resolve a Secret, call the Kubernetes or Drive9 API, or
   require the desired binary/systemd launch path. If local verification needs
   an unavailable capability, return its actionable `FAILED_PRECONDITION` and do
   not claim that the mount is healthy.
2. **Creating or recovering in `NodeStageVolume`**: require the complete launch
   capability set before resolving credentials. A missing launch capability
   returns its actionable `FAILED_PRECONDITION` before startup-file creation,
   phase change, process stop, or unmount. Credential/API failures retain their
   normal status and also occur before side effects. Never fall back to an
   in-container mount.
3. **`NodePublishVolume`**: allow the normal bind-publish path when the staging
   mount is locally verified healthy and the publish-specific mount capability
   works. It does not depend on `systemd-run`, the desired binary, or Secret
   availability.
4. **`NodeUnpublishVolume` / `NodeUnstageVolume`**: never reject solely because
   create-path preflight is degraded. Enter normal cleanup with its own
   capability and ownership checks. If safe cleanup cannot proceed, preserve the
   current durable state, or preserve `phase=stopping` after committing its
   intent, and return the specific error.

This distinction lets an already-running healthy mount remain usable during a
temporary Secret, Kubernetes API, desired-binary, or systemd launch-path outage,
without weakening the fail-closed rules for new side effects.

#### 2. Host-namespace + host-cgroup mount process

Change `startDrive9Mount` to launch `drive9 mount` via `nsenter` + `systemd-run`
(transient service):

```text
nsenter --mount=/host-proc/1/ns/mnt \
    --root=/host-proc/1/root --wd=/host-proc/1/root -- \
    systemd-run --service-type=exec --collect \
    --unit=drive9-mount-<vol-hash> \
    --property=Restart=no \
    --property=TimeoutStopSec=<configured-stop-timeout> -- \
    /var/lib/drive9-csi/bin/drive9-csi-launcher \
    /run/drive9-csi/<vol-hash>-<attempt-id>.env \
    /run/drive9-csi/<vol-hash>-<attempt-id>.args
```

`systemd-run` returns after systemd has successfully exec'd
`drive9-csi-launcher`. It does not prove that the launcher's second `execve` of
Drive9 succeeded. CSI therefore treats this only as a "launcher started" signal
and then runs the combined mount-point and Drive9 process-state readiness check
described below.

The mount process:

- Runs in the host's mount namespace (FUSE mount directly on host)
- Lives in a host systemd service cgroup
  (`system.slice/drive9-mount-<vol>.service`)
- Survives CSI Pod restart — kubelet cleans Pod cgroup, not host systemd
  services

##### Host PID discovery and identity

After `systemd-run` returns, CSI cannot use `cmd.Process.Pid` — that is the
`nsenter`/`systemd-run` wrapper PID, not the long-running `drive9 mount` PID.

**Source of truth**: after the FUSE mount and control socket are ready,
`drive9 mount --foreground` writes a root-only **process-state JSON file**. The
file contains the mount PID, canonical mount point, and resolved control socket
path. It is a readiness and recovery artifact, not a configurable plain PID
file.

**Path encoding and ownership**:

- CSI-owned artifacts encode volume IDs using a **bounded, collision-resistant**
  scheme: `sha256(volumeID)[:16]` (first 16 hex chars). This produces a
  fixed-length identifier safe for the systemd unit name and CSI startup
  filenames. The full volume ID is stored in CSI mount state JSON for audit. See
  section 3 for collision analysis and failure-on-collision behavior.
- Drive9-owned artifacts use a separate hash derived from the canonical staging
  target. With `TMPDIR=/run/drive9-csi`, the process-state file is
  `/run/drive9-csi/drive9-mount-<mount-hash>.pid`, where `<mount-hash>` is the
  first 16 hex characters of `sha256(canonicalStagingTarget)`. The file contains
  JSON and is mode `0600`. Drive9 does not expose a `--pid-file` flag.
- With `XDG_RUNTIME_DIR=/run/drive9-csi`, Drive9 creates the control socket at
  `/run/drive9-csi/drive9-mount-<socket-hash>.sock`. `<socket-hash>` is the
  first 16 hex characters of
  `sha256(effectiveUID + "\0" + canonicalStagingTarget)` and is not necessarily
  equal to `<mount-hash>`. CSI reads the exact socket path from the
  process-state JSON. Drive9 does not create a socket inside the staging target.
- Run dir: host `/run/drive9-csi/` (mode `0700`, mounted into the CSI Node Pod
  through a dedicated hostPath). It is runtime state rather than CSI durable
  state: it survives a CSI Pod restart, while a node reboot removes it together
  with the transient systemd service and FUSE process. The launcher supplies
  this directory through both `TMPDIR` and `XDG_RUNTIME_DIR`.
- **Stale file handling**: before starting a new mount, CSI checks for existing
  process-state/socket files at the expected paths. If the recorded PID is dead,
  remove the stale files only after confirming that the expected mount is
  absent. If the PID is alive but the full ownership check does not match the
  requested staging target and unit, return an explicit collision/ownership
  error; never delete another live mount's artifacts or stop its process.
- **Cross-volume safety**: kubelet gives each staged volume a distinct staging
  target, so Drive9's mount-point-derived paths are distinct except for a hash
  collision. Before trusting a process-state file, CSI performs a four-way
  ownership check:

  1. Parse `/host-proc/<pid>/cmdline` (NUL-separated argv) and verify the exact
     staging target path appears as the final positional argument (not a
     substring match — exact argv element comparison)
  2. Verify PID belongs to the expected systemd service by checking
     `/host-proc/<pid>/cgroup` contains the expected unit name
  3. Verify PID identity. For `active`/`stopping` state, require `PIDStartTime`
     to match the recorded value. For a new `starting` candidate, read the start
     time before and after the remaining checks, require it to remain unchanged,
     and record it only during successful promotion
  4. Resolve `/host-proc/<pid>/exe` and require the canonical host path to equal
     the attempt or active state's recorded content-addressed `binaryPath`. A
     missing executable, the kernel's `(deleted)` suffix, or any other path is
     an ownership failure, not permission to stop or adopt the process

**PID resolution sequence** (as part of readiness detection):

1. Derive the Drive9 process-state path from the canonical staging target and
   read its JSON content
2. Read PID startTime from `/host-proc/<pid>/stat` (field 22)
3. Parse `/host-proc/<pid>/cmdline` (NUL-separated), verify the exact staging
   target path as the final argv element
4. Verify `/host-proc/<pid>/cgroup` contains expected systemd unit name
5. Resolve `/host-proc/<pid>/exe` and verify it exactly matches the recorded
   candidate `binaryPath`
6. Verify the process-state mount point and control socket match the expected
   staging target and run directory
7. Read PID startTime again and require the same PID/start-time pair observed in
   step 2
8. Record `PID`, `PIDStartTime`, `systemdUnit`, `binaryPath`,
   `controlSocketPath` in CSI mount state

**PID verification** (`pidMatchesState`):

- All PID-based checks (`pidStartTime`, `pidAlive`, `/proc/<pid>/cgroup`,
  `/proc/<pid>/exe`) read from **`/host-proc/<pid>/...`**, not `/proc/<pid>/...`
- This is mandatory because the host systemd manager creates the mount process
  in the host PID namespace and the CSI driver does not use `hostPID: true`
- Reused-PID protection: compare the PID together with its boot-relative
  `startTime`, rather than treating a numeric PID as a stable identity
- Executable protection: compare the canonical `/host-proc/<pid>/exe` target
  with the state's immutable content-addressed `binaryPath`; an argv/cgroup/PID
  match alone is insufficient

##### Environment and secret propagation

`drive9 mount` receives the API key through `DRIVE9_API_KEY`. The server is also
present in `DRIVE9_SERVER`, but CSI additionally passes the non-secret
`--server <server>` flag so a host-local active Drive9 context cannot override
the requested server.

**Mechanism**: Root-only environment file, not `--setenv`.

Using `systemd-run --setenv=DRIVE9_API_KEY=...` puts the secret in the
`systemd-run` process argv, visible to any node-root process via
`/proc/<pid>/cmdline` during launch. While the CSI node plugin already runs as
node-root (privileged container), minimizing secret surface is preferred.

Instead, use a compiled Go launcher binary (`drive9-csi-launcher`) that reads
env from a root-only file and calls `execve` directly — no shell in the
secret/arg parsing path.

```text
1. Atomically write `/run/drive9-csi/<vol-hash>-<attempt-id>.env` (mode 0600,
   root:root) through a same-directory temporary file created with
   `O_CREATE|O_EXCL`, followed by fsync, close, `linkat` to publish the final
   name without replacement, and unlink of the temporary name. An existing
   final path is reconciled as an idempotency/collision case and is never
   replaced. Format: NUL-separated KEY=VALUE pairs.
     DRIVE9_SERVER=<server>\0DRIVE9_API_KEY=<key>\0
     TMPDIR=/run/drive9-csi\0
     XDG_RUNTIME_DIR=/run/drive9-csi\0
     PATH=/usr/sbin:/usr/bin:/sbin:/bin\0
   The launcher preserves bytes exactly and applies no shell parsing, quoting,
   or escaping. Drive9's credential resolver subsequently trims leading and
   trailing ASCII whitespace from `DRIVE9_SERVER` and `DRIVE9_API_KEY`; the
   CSI credential contract therefore does not support such whitespace.

2. Atomically write `/run/drive9-csi/<vol-hash>-<attempt-id>.args` using the
   same 0600 temporary-file procedure. Format: NUL-separated argv elements.
     /var/lib/drive9-csi/bin/drive9-<sha256>\0mount\0--foreground\0
     --mode=fuse\0--direct-mount-strict\0--server\0<server>\0--allow-other\0
     <all existing cache, TTL, profile, perf, and tuning flags>\0
     :<remote-root>\0<staging-target>\0
   Same binary-safe encoding — no shell interpretation.
   Drive9 has no `--staging-target` or `--pid-file` flags. The remote source
   and staging target are the final two positional arguments, and CSI must
   preserve the complete argument set currently produced by
   `drive9MountArgs`.

   `argv[0]` is the resolved content-addressed path selected before the files
   are written; it is not the mutable `drive9` symlink.

3. Launch:
     nsenter --mount=/host-proc/1/ns/mnt \
       --root=/host-proc/1/root --wd=/host-proc/1/root -- \
       systemd-run --service-type=exec --collect \
       --unit=drive9-mount-<vol-hash> \
       --property=Restart=no \
       --property=TimeoutStopSec=<configured-stop-timeout> -- \
       /var/lib/drive9-csi/bin/drive9-csi-launcher \
       /run/drive9-csi/<vol-hash>-<attempt-id>.env \
       /run/drive9-csi/<vol-hash>-<attempt-id>.args

   drive9-csi-launcher:
   a. Reads .env file, splits on NUL → []string env pairs
   b. Reads .args file, splits on NUL → []string argv
   c. Immediately unlinks both startup files after they have been read
   d. Calls syscall.Exec(argv[0], argv, env) — direct execve(2),
      no shell, no command substitution, no word splitting
   e. If execve fails, exits non-zero (systemd marks service failed)

   The launcher is a small Go binary compiled into the CSI image and copied
   to the host by the init container. It uses the same atomic-copy mechanism
   as Drive9 but is not versioned.
   It never interprets, quotes, or transforms the env/arg values.

4. Startup-file cleanup has two owners:
   - After `systemd-run` starts the launcher, the launcher reads and immediately
     unlinks both files before `execve`.
   - If CSI exits after creating the files but before the launcher consumes
     them, the durable `phase=starting` state records their exact attempt-scoped
     paths. Startup reconciliation removes them only after validating that the
     matching service/mount is absent or after finishing its recovery.
```

**No shell in secret path**: The entire chain from K8s Secret → env file →
`execve` env array is binary-safe and never passes through `/bin/sh`, command
substitution, or word splitting. Values cannot contain NUL bytes because NUL
cannot be represented in `execve(2)` env strings. In addition, the current
Drive9 credential resolver normalizes leading and trailing ASCII whitespace; CSI
validates and rejects credentials containing such whitespace rather than
claiming byte-for-byte preservation through Drive9 itself.

**NUL byte validation**: Before writing `.env` or `.args` files, CSI validates
that every env key, env value, and argv element contains no NUL bytes
(`strings.IndexByte(value, 0) >= 0`). If any value contains NUL:

- `NodeStageVolume` returns `INVALID_ARGUMENT` with error:
  `"drive9-csi: secret value contains NUL byte, cannot pass to mount process"`
- `.env` and `.args` files are NOT written (validation runs before file I/O)
- No cleanup needed since no files were created
- E2e test case: attempt mount with a secret containing a NUL byte, verify
  `NodeStageVolume` returns the expected error and no .env/.args files persist

**Credential whitespace validation**: Before writing startup files, CSI rejects
`DRIVE9_SERVER` or `DRIVE9_API_KEY` values with leading or trailing ASCII
whitespace. This makes Drive9's existing normalization explicit instead of
silently changing the value after CSI validation.

A launcher integration test uses a probe executable and synthetic values with
embedded `$`, backticks, spaces, quotes, newlines, and semicolons to verify that
the launcher introduces no shell interpretation. A real Drive9 mount e2e uses a
valid credential and does not assume that arbitrary bytes are accepted by the
Drive9 server. Leading and trailing ASCII whitespace is tested as an explicitly
rejected input.

**Exposure constraints under node-root threat model**:

- On normal startup paths, the env file exists only during mount startup and is
  mode 0600 under host `/run`. Abrupt CSI termination before launcher start can
  leave it until `phase=starting` reconciliation removes it
- Drive9 removes the credential variables from its logical process environment
  after resolution, but Linux `/proc/<pid>/environ` exposes the initial `execve`
  environment and is not guaranteed to reflect `unsetenv`. This design accepts
  that node-root can inspect the initial environment and process memory; a
  future credential-FD transport may reduce this exposure but is not a V1
  blocker
- Drive9's root-only process-state JSON may contain `api_key` or `token`. This
  is accepted as **runtime credential state** under `/run/drive9-csi`, not CSI
  durable state. It survives CSI Pod replacement but disappears on node reboot,
  when the corresponding host FUSE process also disappears
- CSI durable mount/publish state under `/var/lib/drive9-csi` never contains the
  API key or token
- `systemctl show` does not expose the env vars
- Journal/logs: `drive9 mount` must not log the API key
- CSI driver never logs startup-file contents or the API key; if credential
  metadata is included in a structured log, its value is `<redacted>`
- **E2e check**: after mount ready, verify startup files are deleted, the
  process-state file is under `/run/drive9-csi` with mode `0600`, CSI durable
  state contains no credential, and `systemctl show`, argv, and logs do not
  contain the API key

**Secret rotation**: if the K8s Secret changes, an existing mount continues with
its old credential until either normal `NodeUnstageVolume` followed by
`NodeStageVolume`, or a future explicit `kubectl drive9 mount rebuild`, creates
a new process from the current Secret.

##### Crash recovery boundary

The transient service explicitly uses `Restart=no`. This revision has no
continuous CSI watcher that restarts a FUSE process after it crashes. Mount
recovery runs when the CSI driver starts; a crash while CSI remains running is
not automatically recovered by V1. systemd must not restart Drive9 directly: it
cannot clean a disconnected staging mount, reconstruct Kubernetes-derived
configuration safely, or repair publish targets. A CSI-owned watcher is a
follow-up after subtree publish recovery is merged, as described below.

##### Readiness and failure attribution

**Readiness detection**:

1. `systemd-run --service-type=exec` returns once systemd has exec'd
   `drive9-csi-launcher`. This confirms that `ExecStart` was created, but does
   not yet prove that the launcher exec'd Drive9 or that FUSE is ready.
2. Poll until both conditions are true: `isMountPoint(stagingTarget)` and a
   valid Drive9 process-state JSON exists for the canonical staging target.
   Drive9 writes that JSON only after the mount and control socket are ready;
   polling both avoids racing between mount visibility and process-state write.
3. Validate the PID/start-time pair, exact argv target, exact executable path,
   mount point, control socket, and systemd cgroup; then atomically promote CSI
   mount state from `starting` to `active` and return success.
4. If the combined readiness check times out → check service status via
   `systemctl is-active drive9-mount-<vol-hash>.service`:
   - Service inactive/failed: `drive9 mount` crashed before mounting. Read logs
     via `journalctl --unit=drive9-mount-<vol-hash>.service --no-pager -n 50`
     for error attribution. Clean up .env/.args files, return error.
   - Service `not-found`/unknown because the transient unit was already garbage
     collected: if this CSI invocation observed `systemd-run` return success for
     the same attempt and readiness is still false, classify it as an exited
     startup attempt and follow the same failure cleanup. It is not evidence
     that the attempt was never created. After a CSI restart, use the durable
     no-service/no-mount reconciliation rows below; do not invent a persisted
     launch result.
   - Service active but no mount: `drive9 mount` is running but not mounting.
     Stop service, clean up .env/.args files, return error with log snippet.

`--collect` permits systemd to unload a failed or inactive transient unit, so
unit status and exit metadata may disappear before CSI observes them. Journal
retrieval is therefore best-effort and must not be required for cleanup or
retry. A D-Bus/query error is different from a definitive `not-found`: preserve
the transaction and report the status capability error unless PID, mount, and
ownership observations independently establish a safe action. Outside a recorded
`starting` attempt, `not-found` retains its ordinary meaning of "no service
exists"; the failure attribution above is context-specific.

##### Crash-safe mount lifecycle transactions

CSI cannot atomically commit its durable state, a systemd unit, and a kernel
mount. It therefore uses the CSI mount state as a write-ahead transaction with
three durable phases:

```text
absent --NodeStageVolume--> starting --ready--> active
                                                |
                                                +--NodeUnstageVolume--> stopping --clean--> absent
                                                |
                                                +--runtime lost-------> starting --ready--> active
starting --NodeUnstageVolume-------------------> stopping
```

Before writing startup files or invoking `systemd-run`, CSI atomically writes a
root-only durable state under `/var/lib/drive9-csi` containing at least:

```json
{
  "schemaVersion": 2,
  "phase": "starting",
  "reason": "stage|recovery",
  "attemptID": "<random-128-bit-id>",
  "volumeID": "<full-volume-id>",
  "remoteRoot": "<remote-root>",
  "stagingTarget": "<canonical-staging-target>",
  "systemdUnit": "drive9-mount-<vol-hash>.service",
  "binaryPath": "/var/lib/drive9-csi/bin/drive9-<sha256>",
  "fallbackBinaryPath": "<previous-active-path-or-empty>",
  "mountArgs": ["mount", "--foreground", "..."],
  "fallbackMountArgs": ["<previous-active-non-secret-argv>"],
  "envPath": "/run/drive9-csi/<vol-hash>-<attempt-id>.env",
  "argsPath": "/run/drive9-csi/<vol-hash>-<attempt-id>.args",
  "createdAt": "<rfc3339>",
  "startupDeadline": "<rfc3339>"
}
```

The `starting` state contains artifact paths but no credential values. For
normal staging, `binaryPath` is the resolved desired binary and
`fallbackBinaryPath` is empty. For recovery of a lost active process,
`binaryPath` is the desired candidate and `fallbackBinaryPath` is the previously
active content-addressed path. `mountArgs` contains the candidate's exact argv
excluding `argv[0]`; `fallbackMountArgs` preserves the previous active argv so a
newer CSI does not pass newly introduced flags to an older fallback binary.
Neither array may contain credentials. The state is committed through a
same-directory temporary file, file fsync, atomic rename, and directory fsync.
No external side effect is allowed unless this write succeeds.

`createdAt` and `startupDeadline` are computed once for each candidate attempt
before that write, with `startupDeadline = createdAt + maxStartupTimeout`. V1
uses a 90-second `maxStartupTimeout`, matching the current mount-readiness
bound. If this becomes configurable later, the resolved value is sampled only
when the attempt is created; an in-flight attempt still uses its persisted
deadline. They are immutable for that `attemptID`: a CSI restart, RPC retry, or
reread must not reset the deadline or recompute it from the new CSI binary's
timeout configuration. Reconciliation first promotes an already-ready, fully
verified attempt without waiting. Otherwise it computes
`remainingTimeout = max(0, startupDeadline - now)`; zero means run the timeout
classification and cleanup immediately. A desired-to-fallback switch creates a
new candidate attempt, with a new ID and its own bounded deadline, only after
the desired attempt is fully absent. Missing, malformed, or internally
inconsistent deadline fields in schema version 2 are invalid durable state and
are preserved for inspection rather than guessed or silently reset.

After mount readiness and the full ownership check succeed, CSI atomically
replaces the same file with `phase=active` and adds `PID`, `PIDStartTime`,
`controlSocketPath`, and the verified start time. Only then may
`NodeStageVolume` return success. If the mount is healthy but the active-state
write fails, CSI preserves `phase=starting` and returns an error without killing
the healthy service; a retry can verify and promote the same attempt. Promotion
records the successful candidate and argv as the new active `binaryPath` and
`mountArgs`, and clears both fallback fields.

On CSI startup, and before a Node RPC mutates a volume with `phase=starting`,
CSI reconciles that attempt under the normal per-volume lock:

Reconciliation first performs all non-blocking observations. A fully verified
ready attempt is promoted even if its deadline has just passed. The persisted
deadline gates every action that would launch the recorded candidate or wait for
additional readiness.

| Starting state observation                                     | Action                                                                                                                                    |
| -------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| `reason=stage`, no service and no mount                        | Remove attempt-scoped startup files, then delete state; kubelet may retry `NodeStageVolume`                                               |
| Recovery; no service/mount; time remains                       | Resume within remaining time                                                                                                              |
| Recovery; no service/mount; expired                            | Fail candidate and apply fallback rule                                                                                                    |
| Service still starting and `remainingTimeout > 0`              | Continue readiness polling for at most the persisted remaining timeout                                                                    |
| Deadline expired and service not ready                         | Fail and clean the current attempt immediately                                                                                            |
| Service, PID, mount, and control socket all verify             | Promote the same attempt to `active`                                                                                                      |
| `reason=stage`, service failed and no mount exists             | Remove startup files and failed unit, then delete state                                                                                   |
| `reason=recovery`, desired service failed and no mount exists  | Remove startup files and failed unit, verify the desired attempt is fully absent, then atomically switch to the recorded fallback attempt |
| `reason=recovery`, fallback service failed and no mount exists | Remove startup files and failed unit, preserve recovery state, and report Degraded                                                        |
| Verified dead service left a disconnected mount                | Clean the mount and artifacts, then retry from a new attempt                                                                              |
| Any live ownership mismatch                                    | Preserve state, perform no destructive action, and report an ownership error                                                              |

For `reason=recovery`, a desired candidate failure does not immediately launch
the fallback. CSI must first verify that the desired service, PID, mount,
socket, and attempt-scoped files are absent. It then atomically records a new
attempt whose `binaryPath` is the previous `fallbackBinaryPath`, moves
`fallbackMountArgs` into `mountArgs`, clears both fallback fields, and launches
that exact attempt. If both candidates fail, state is preserved and the mount is
reported Degraded. A CSI crash before the candidate switch may retry the desired
candidate, but it can never run desired and fallback processes concurrently.

Before `NodeUnstageVolume` performs its first destructive side effect, it
atomically replaces the current state with `phase=stopping`. The stopping state
retains the full volume and staging identity, `systemdUnit`, active
`binaryPath`, PID identity and control-socket path when known, plus a new
`stopAttemptID` and `stoppingAt`. When stopping an in-flight `starting` attempt,
it also retains the exact attempt-scoped startup paths needed for cleanup. It
contains no credential. This write is the durable intent that distinguishes an
intentional unstage from a crash or node reboot.

| Stopping state observation                   | Action                                                                       |
| -------------------------------------------- | ---------------------------------------------------------------------------- |
| Service, verified PID, or mount still exists | Continue the fixed cleanup sequence in section 7                             |
| Service, PID, and mount are all absent       | Remove verified stale runtime artifacts, then delete state                   |
| Cleanup is incomplete                        | Preserve `phase=stopping` and return/report the cleanup error                |
| Any live ownership mismatch                  | Preserve state, perform no destructive action, and report an ownership error |

`NodeUnstageVolume` retries resume the same stopping transaction.
`NodeStageVolume` encountering `phase=stopping` must finish that cleanup before
creating a new `starting` attempt. If unstage encounters `phase=starting`, it
first reconciles the exact startup attempt and then transitions it to
`stopping`, whether or not external resources remain, without launching another
process.

Starting- and stopping-state reconciliation may run in parallel across volumes,
but every `NodeStageVolume`, `NodeUnstageVolume`, `NodePublishVolume`, and
administrative operation for a given volume must first acquire the same
per-volume lock. A state file is deleted only when a cancelled `starting`
attempt or a `stopping` transaction has no remaining external resources. A
successful startup is atomically promoted to `active` rather than deleted.

Cleanup via `NodeUnstageVolume` first drains through the recorded active
`binaryPath`, then stops the systemd service and performs normal/lazy kernel
unmount through the validated launcher.

#### 3. Systemd unit naming and idempotency

**Canonical volume ID encoding**: `sha256(volumeID)[:16]` — the first 16 hex
characters (8 bytes) of the SHA-256 hash. This produces a fixed-length, bounded,
systemd-safe identifier (`[0-9a-f]`, always 16 chars) regardless of volume ID
length. The full original volume ID is stored in mount state JSON for
audit/debug and reverse lookup.

This encoding is used for CSI-owned per-volume artifacts:

- Systemd unit: `drive9-mount-<vol-hash>.service` (37 chars total, well
  under 255)
- Env file: `/run/drive9-csi/<vol-hash>-<attempt-id>.env`
- Args file: `/run/drive9-csi/<vol-hash>-<attempt-id>.args`

The current driver does not accept arbitrary volume IDs. `NodeStageVolume`
requires the ID to equal `volumeIDForRemoteRoot(remoteRoot)` (`drive9-<32hex>`)
or `volumeIDForWorkspaceRoot(name, remoteRoot)` (`drive9-root-<32hex>`). Both
are short and already match the characters preserved by `safeFileName()`.
Existing CSI mount-state/cache/local/perf paths therefore remain unambiguous for
every valid Drive9 volume ID; this design does not claim support for synthetic
200-character IDs.

Drive9-owned process-state and socket paths use hashes of the canonical staging
target as described in section 2. They are deliberately not named with
`<vol-hash>` because the current Drive9 CLI does not provide path override
flags.

**Collision resistance**: 8 bytes (64 bits) of SHA-256 gives a collision
probability of 1/2^64 for a fixed pair. With 1000 volumes on one node, the
birthday-bound probability is approximately 2.7e-14, or 1 in 37 trillion. If a
collision were to occur (same hash for two different volume IDs), the second
`NodeStageVolume` would find an existing service unit with a mismatched
PID/staging-target and fail with an explicit error, never silently stomping the
first mount.

Mount state JSON records `systemdUnit` and the original `volumeID` for reverse
lookup, validation, and audit.

**Idempotency** (`NodeStageVolume` retry):

- After validating request capability and volume/staging identity, acquire the
  per-volume lock and evaluate CSI durable state plus local runtime ownership
  before resolving any Secret or making any Kubernetes/Drive9 API call
- If `phase=active` has a verified service, PID/start time, exact argv target,
  exact executable path, mount point, process-state, and control socket → return
  success using local state only. Temporary Secret/PV/API unavailability must
  not turn this healthy idempotent call into an error
- If state is `starting` → reconcile that exact attempt before creating any new
  startup file or unit
- If state is `stopping` → finish the stopping transaction before creating a new
  startup file or unit
- If the unit name exists but its recorded full volume ID or staging target
  differs → return an explicit hash-collision/ownership error; never stop the
  other unit
- If the recorded PID is stale but the unit's current `MainPID` independently
  passes the full ownership gate → stop that verified owned service, then enter
  desired-first recovery; an unverifiable live PID is an ownership error
- If service doesn't exist but state file exists → state is stale, clean up
  through the phase-specific transaction and enter desired-first recovery when
  the phase is `active`

Credential resolution occurs only after local evaluation proves that a new
process must be created or an unhealthy process must be recovered. A healthy
idempotent return never rotates credentials; Secret rotation continues to take
effect only on a later stage/recovery/rebuild transaction.

**Active recovery split-state handling** (on CSI driver restart):

The matrix below applies only to `phase=active`, after `starting` and `stopping`
transactions have been reconciled. It is evaluated only after the recorded full
volume ID, staging target, and unit name match. For each live PID discovered
from CSI state, Drive9 process state, or the systemd unit's `MainPID`, CSI
independently verifies PID start time where recorded, exact argv target, exact
executable `binaryPath`, and cgroup/unit ownership. Any live ownership mismatch
or unverifiable live PID is a collision/ownership error and is never a cleanup
instruction. In the table, "PID matches" means the recorded PID identity agrees
with the verified current mount process; it is not permission to skip
verification when the answer is "no".

| Service exists | PID matches | Mount exists | Control socket | Action                                                                                                                                                                                                               |
| -------------- | ----------- | ------------ | -------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| yes            | yes         | yes          | yes            | Skip (healthy)                                                                                                                                                                                                       |
| yes            | yes         | yes          | no             | Stop service, unmount, then desired-first recovery                                                                                                                                                                   |
| yes            | yes         | no           | -              | Stop service, then desired-first recovery                                                                                                                                                                            |
| yes            | no          | -            | -              | Stop verified owned service, clean runtime artifacts, then desired-first recovery                                                                                                                                    |
| no             | yes         | yes          | yes            | Drain through `drive9 mount drain`, send SIGTERM to the verified PID, kernel-unmount if needed, then use desired-first recovery with a new service. Adopting an orphan PID into a new systemd service is unreliable. |
| no             | yes         | no           | -              | Kill verified PID, clean runtime artifacts, then desired-first recovery                                                                                                                                              |
| no             | no          | yes          | -              | Kernel-unmount the verified disconnected mount, clean runtime artifacts, then desired-first recovery                                                                                                                 |
| no             | no          | no           | -              | Desired-first recovery; this is the normal node-reboot observation for durable `phase=active` state                                                                                                                  |

Desired-first recovery snapshots the resolved desired content-addressed path at
transaction start and records the previous active path as fallback. It obtains
current credentials from the referenced Kubernetes Secret. It never changes a
healthy active process merely because desired and active differ. An invalid
desired candidate is cleaned and may fall back under the compatibility rule
above. Missing PV or Secret data, or failure of every safe candidate, preserves
the durable state and reports the mount Degraded rather than silently deleting
the staged-volume intent.

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

This avoids `hostPID: true`, which would expose all host processes through the
container's normal `/proc`. The CSI driver accesses `/host-proc/1/ns/mnt` and
`/host-proc/1/root` for host namespace/root entry, plus the verified mount-PID
paths listed in the security posture section.

#### 5. Versioned binary installation (init container)

Install the `drive9` binary to a versioned path on the host:

The CSI image build must consume a pinned Drive9 release artifact for each
architecture. The metadata/tagging job and image build jobs must use the same
resolved release version and expected SHA-256 values (or the same uploaded
artifacts); build jobs must not independently fetch a mutable "latest" URL and
assume it still matches the tag. The host-side content hash below identifies the
installed file but is not a substitute for build-time checksum pinning.

```yaml
initContainers:
  - name: install-drive9
    image: ghcr.io/drive9-ai/drive9-csi:<tag>
    command:
      - /usr/local/bin/drive9-csi
      - install-host-binaries
      - --host-state-dir=/host-state
      - --drive9-source=/usr/local/bin/drive9
      - --launcher-source=/usr/local/bin/drive9-csi-launcher
    volumeMounts:
      - name: state-dir
        mountPath: /host-state
```

`install-host-binaries` is a filesystem-only subcommand.
`cmd/drive9-csi/main.go` must dispatch it before parsing server flags or
creating an in-cluster Kubernetes client, so the init container does not depend
on service-account credentials or a CSI endpoint.

The subcommand owns the complete installation contract:

1. Create `<host-state-dir>/bin` with mode `0755`.
2. Validate Drive9 and the launcher as regular executable static Linux ELF
   binaries for the current target architecture with no `PT_INTERP` segment.
3. Compute SHA-256 from the Drive9 source content and install it as
   `drive9-<sha256>` using a same-directory temporary file, file fsync, atomic
   rename, and directory fsync. An existing destination must be a regular file
   with the same digest; symlinks and unexpected file types are rejected.
4. Install `drive9-csi-launcher` through the same atomic replacement procedure.
5. Only after both files validate, atomically replace the `drive9` symlink with
   a relative link to `drive9-<sha256>`. Any failure leaves the previous desired
   symlink intact.
6. Print only the installed digest/path; never print file contents or
   credentials.

The logic is implemented and unit-tested in Go rather than duplicated as inline
shell in the DaemonSet manifest.

Binary layout on host (`/var/lib/drive9-csi/bin/`):

- `drive9-<sha256>` — content-addressed binary (never overwritten by the
  installer)
- `drive9` — symlink to the node-local desired version (atomically replaced on
  each CSI Pod start)
- `drive9-csi-launcher` — env/arg loader, calls execve. Compatibility-stable
  (its contract is: read NUL-separated .env + .args, exec argv[0]). Installed
  atomically (temp + rename) on every init container run, independent of drive9
  version. Not versioned because its interface is stable; if the interface ever
  changes, it will be versioned alongside drive9.
- Binary, symlink, and launcher replacement use temp paths plus rename to
  prevent partial installation
- Versioned filename is derived from the binary content SHA-256; it does not
  depend on parsing the multi-line output of `drive9 version`

CSI resolves the `drive9` symlink before launch and records the resolved
versioned `binaryPath` for auditability and GC. A running process retains a
kernel reference to its executable inode, so replacing the symlink does not
change the executable used by that process. A CSI rollout therefore changes the
desired binary for new mounts without rebuilding existing mounts.

**Binary GC**: Runs **after** the full recovery scan completes (not interleaved
with mount recovery). The keep set is the union of the current desired `drive9`
symlink target, resolved `binaryPath` values in `starting`, `active`, and
`stopping` CSI state, every non-empty `fallbackBinaryPath` in an in-flight
recovery, and `/host-proc/<pid>/exe` targets for every verified live
`drive9-mount-*.service` or Drive9 process-state artifact. If any candidate unit
or process-state artifact cannot be safely correlated, skip binary GC for that
startup rather than assuming the inventory is complete. GC considers only
regular files whose basename exactly matches `drive9-[0-9a-f]{64}`; it never
matches the `drive9` symlink, `drive9-csi-launcher`, or an inert legacy helper
file. Remove a matching file only when it is outside the keep set. This
preserves binaries used by live, stopping, or partially recovered mounts and
preserves the current binary when no volume is mounted.

#### 6. Remove shutdown unmount on SIGTERM

Remove `shutdownNodeMounts()` from the SIGTERM/exit path. The CSI driver process
exits cleanly by stopping its gRPC server and closing its endpoint socket
without touching mount processes. Kubelet registration is managed by the
separate `node-driver-registrar` sidecar, not by the driver process.

SIGTERM is sent on: rolling update, `kubectl delete pod`, Pod eviction, preStop
hook timeout. In all cases, mount processes should survive.

#### 7. Cleanup sequence (NodeUnstageVolume)

First validate the recorded full volume ID and staging target. For every live
PID or service, also apply the unit, PID-start-time, argv, executable-path, and
cgroup ownership gate. On any live ownership mismatch, return an error without
performing destructive cleanup. After confirming that no active publish target
remains, atomically persist `phase=stopping`, `stopAttemptID`, and `stoppingAt`
before the first drain, stop, unmount, signal, or state-deletion side effect. If
this write fails, return an error and leave the active mount untouched.

After the stopping-state write succeeds, use the fixed ordering below; each step
runs regardless of the previous step's result: `drive9`, `systemctl`, and the
kernel-unmount launcher are invoked through the same host-mount-namespace plus
host-root `nsenter` prefix used during launch.

```text
1. <active-binaryPath> mount drain --timeout 30s <staging-target>
   This reads Drive9 process state and requests a drain through the control
   socket. Run it with the same `TMPDIR` and `XDG_RUNTIME_DIR` used by the
   mount service so it resolves the correct process-state file. Drain failure
   is recorded but does not skip the remaining cleanup.
2. Verify service: systemctl is-active drive9-mount-<vol-hash>.service
   If still active: systemctl stop drive9-mount-<vol-hash>.service
   systemctl stop sends SIGTERM; Drive9's foreground signal handler performs
   its graceful shutdown and unmount path. The transient unit has an explicit
   TimeoutStopSec, and the CSI command context waits longer than that value.
   If the service does not exit before TimeoutStopSec, systemd sends SIGKILL;
   it does not send a second SIGTERM. This runs regardless of step 1.
3. Verify: isMountPoint(stagingTarget) == false
   If still mounted: invoke `drive9-csi-launcher host-unmount` for a normal
   kernel unmount (`unix.Unmount`).
4. Verify again: isMountPoint(stagingTarget) == false
   If busy: lazy unmount (MNT_DETACH). This detaches the mount and defers final
   cleanup while references remain; it does not guarantee that existing FUSE
   file descriptors continue without EIO or ENOTCONN after daemon termination
5. Verify: PID dead (pidMatchesState == false)
   If still alive: SIGKILL + wait 5s
6. Delete stale Drive9 process-state/socket files, then delete CSI mount state
   (only if all above verifications pass)
```

**Terminal state**: mount gone, PID gone, service gone, CSI mount state gone,
and stale Drive9 process-state/socket artifacts gone.

**CSI state deletion rule**: CSI mount state is deleted ONLY after all of the
following are confirmed:

- `isMountPoint(stagingTarget)` returns false
- PID is dead (`pidMatchesState` returns false)
- Service is inactive or doesn't exist
- Attempt-scoped startup files and verified stale process-state/socket artifacts
  are absent

If any of these conditions is NOT met after all cleanup attempts, CSI mount
state is preserved as **`phase=stopping`** and `NodeUnstageVolume` returns an
error. CSI startup and `NodeUnstageVolume` retries continue this cleanup and
never feed a stopping state into desired-first recovery. This ensures the
orphaned resource remains manageable. Never delete state to "clean up" an
unresolvable orphan — that makes it permanently unmanageable.

#### 8. Recovery

`recoverNodeMounts()` enhanced with systemd service awareness:

- Reconcile `phase=stopping` first by continuing cleanup; never re-mount it
- Reconcile `phase=starting`, including an in-flight desired/fallback candidate
- For `phase=active`, check `pidMatchesState(state)` AND service active → skip
  when healthy; otherwise run desired-first recovery through the split-state
  matrix in section 3
- After successful re-mount → `repairPublishTargets` (existing code, unchanged)

#### 9. Follow-up: explicit mount rebuild

Applying the node-local desired Drive9 binary to a **healthy** existing mount is
an explicit administrative action, not a side effect of CSI rollout and not a
`NodeUnstageVolume` operation. Desired-first recovery is separate: it applies
only after the active process has already been lost and replacement is required.

The planned entry point is a thin `kubectl-drive9` plugin:

```text
kubectl drive9 mount rebuild <PVC/PV selector> [--node <node> | --all-nodes]
    -> resolve each target to (nodeName, volumeID)
    -> locate that node's drive9-csi-node Pod
    -> invoke a drive9-csi admin client through kubectl exec
    -> admin client calls the running CSI Node over a root-only local Unix socket
    -> CSI Node acquires the normal per-volume lock and performs the operation
```

The plugin owns discovery, plan display, confirmation, and progress reporting.
It never reads or transports the Drive9 API key. The running CSI Node owns all
state validation, Secret resolution, systemd operations, and publish repair. An
independently exec'd helper must not modify mounts directly because it does not
share the CSI process's per-volume lock.

The command has **rebuild** rather than **upgrade** semantics. The target binary
is the content-addressed path obtained by resolving that node's desired
`/var/lib/drive9-csi/bin/drive9` symlink at transaction start. The plugin does
not download or choose an arbitrary Drive9 version. If active and desired paths
match, the operation is an idempotent no-op unless explicitly forced. Rolling
back the CSI Node image updates the desired symlink; a later explicit rebuild
then applies that rollback to existing mounts.

Future execution may use FD handoff when both Drive9 processes support it.
Otherwise it drains and stops the old service, recreates the staging mount with
the desired binary, and repairs publish targets. The latter path is disruptive
for the old FUSE connection. Before subtree PR #24 is merged, affected workload
Pods under the legacy root-publish layout may require recreation. After subtree
recovery is merged, running Pods can receive the replacement child mount, but
already-open file and directory descriptors remain attached to the old FUSE
connection.

The future transaction adds `phase=rebuilding`, a request ID, and old/desired
binary paths. Binary GC keeps both paths until the transaction reaches a
terminal state. Loss of the kubectl connection does not cancel a committed
rebuild; the CSI state remains the recovery source of truth.

#### 10. Follow-up: continuous FUSE crash recovery

After subtree PR #24 is merged, CSI Node may add a watcher that subscribes to
systemd unit state changes, with periodic reconciliation as a fallback. On a
verified unit failure or dead PID it acquires the per-volume lock, reconstructs
the request from PV/Secret state, clears the disconnected staging mount, and
runs the same **desired-first recovery** transaction used during CSI startup. A
successful desired candidate becomes active; a failed desired candidate is fully
cleaned before the recorded previous active binary is attempted. It then repairs
subtree publish targets. A healthy process is never rebuilt merely to resolve an
active/desired mismatch.

Recovery uses per-volume exponential backoff and stops after a bounded number of
failures, leaving the mount `Degraded` for inspection or explicit rebuild.
Detecting a process that is alive but internally hung requires a future
lightweight control-socket `ping/status` operation; filesystem probes such as
`stat` or `readdir` are not used because a hung FUSE request may block them.

This follow-up restores new pathname-based access in running Pods after subtree
repair. It cannot repair descriptors already opened against a FUSE connection
that died. Preserving those descriptors requires a future stable FD holder and
Drive9 FD/state handoff.

#### 11. Permanent CSI uninstall

Loss of the CSI Node heartbeat never automatically stops host FUSE services: the
service cannot distinguish a rolling update or prolonged outage from an
intentional permanent uninstall. A timeout-based reaper would reintroduce the
business interruption this design removes.

Permanent uninstall is therefore an explicit operator workflow performed while
CSI Node is still present:

```text
kubectl drive9 uninstall --plan
    -> inventory mounts and workload consumers on every node
kubectl drive9 mount cleanup --all-nodes --require-no-consumers
    -> verify ownership and zero active publish consumers
    -> drain, stop, unmount, and remove state
delete CSI controller and node resources only after the inventory is empty
```

Cleanup refuses a mount with active publish consumers unless a separate,
explicit force path is designed later. If CSI has already been removed, the safe
recovery is to reinstall a compatible CSI Node temporarily so it can adopt and
clean the host state; raw `systemctl`/`umount` commands remain break-glass
operations, not the supported uninstall workflow.

#### 12. Priority class for node-pressure resilience

```yaml
spec:
  template:
    spec:
      priorityClassName: system-node-critical
```

This high priority reduces the chance that kubelet selects the CSI DaemonSet Pod
during node-pressure eviction and helps it preempt lower-priority Pods when
being scheduled. It does not define `kubectl drain` ordering.
`kubectl drain --ignore-daemonsets` leaves DaemonSet-managed Pods running while
it evicts consumer Pods.

### Mount propagation

With nsenter, the FUSE mount originates in the host mount namespace:

- Staging target: `/var/lib/kubelet/plugins/kubernetes.io/csi/...` — host path
- CSI `NodePublishVolume` issues the staging-target → publish-target bind mount
  inside the CSI container; `Bidirectional` propagation makes that mount appear
  in the host mount table
- The container runtime bind-mounts the host publish target into the business
  Pod, whose mount normally uses the default `None` (`rprivate`) propagation
  unless the workload explicitly requests another mode

The CSI Pod still needs `mountPropagation: Bidirectional` on the kubelet-dir
volume mount because the current `NodePublishVolume`, `NodeUnpublishVolume`, and
publish-target repair paths execute bind mount and unmount syscalls inside the
CSI container and must propagate those changes back to the host.
`HostToContainer` alone would be sufficient only for observing host mount
changes, not for these container-to-host operations.

This section describes the V1 legacy root-publish layout. The follow-up subtree
change in PR #24 retains the FUSE mount at the same staging target but changes
the publish layer to a stable shared root plus a replaceable child mount, and requires the
workload volume mount to use `HostToContainer`. That follow-up is what allows a
new child mount to propagate into an already-running workload Pod after a
disruptive FUSE rebuild.

### Resource isolation

Mount processes in host systemd services are not constrained by CSI Pod cgroup
limits. Each service still has a separate host systemd cgroup for observation,
but V1 intentionally sets no `MemoryMax`, `CPUQuota`, `CPUWeight`, `IOWeight`,
or OOM-score override.

V1: Drive9 controls in-memory read cache with `--cache-size`. `--cache-dir`
selects the disk cache location; disk read and write-back cache limits use their
corresponding size flags.

Future per-mount limits may be exposed through allowlisted
`VolumeAttributesClass` fields and translated into specific systemd properties.
Users must not pass arbitrary systemd property strings. New desired resource
settings take effect when the mount is next created or explicitly rebuilt; they
do not make a CSI rollout rebuild an existing mount.

### Cross-repo Drive9 CLI requirements

The current `drive9 mount` implementation in `mem9-ai/drive9` provides the
following contract:

| Capability                                        | Current status                                                                                                             | Required for this design                                                                                                                                                                                                                                          |
| ------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--foreground` flag                               | Exists                                                                                                                     | Keep mount process in foreground (no double-fork)                                                                                                                                                                                                                 |
| Mount target syntax                               | Exists as final positional argument; optional remote source precedes it                                                    | Reuse the complete current CSI mount argv; do not use nonexistent `--staging-target`                                                                                                                                                                              |
| Process-state JSON                                | Exists at `${TMPDIR}/drive9-mount-<mount-hash>.pid`; parent directory follows `TMPDIR`, but there is no explicit path flag | Set `TMPDIR=/run/drive9-csi`, derive the filename from the canonical staging target, and accept its root-only API key/token fields as runtime credential state rather than CSI durable state                                                                      |
| Control socket                                    | Exists at `XDG_RUNTIME_DIR`, or under `TMPDIR/drive9-<uid>` as fallback                                                    | Set `XDG_RUNTIME_DIR` to the host run directory and read the exact socket path from process state                                                                                                                                                                 |
| Control socket drain                              | Exists through `drive9 mount drain`                                                                                        | Drain pending writes before service shutdown                                                                                                                                                                                                                      |
| Graceful foreground shutdown                      | Exists on SIGTERM/SIGINT                                                                                                   | `systemctl stop` drives normal process shutdown                                                                                                                                                                                                                   |
| `--direct-mount-strict`                           | Exists                                                                                                                     | Required for every new mount; direct `mount(2)` failure must not fall back to a helper                                                                                                                                                                            |
| `drive9 version`                                  | Exists and prints multi-line build information; no `--short-sha` flag                                                      | Installer uses binary SHA-256 instead of parsing CLI output                                                                                                                                                                                                       |
| Version query through control socket              | **Not implemented**                                                                                                        | Not required for V1; audit through CSI `binaryPath` and `/host-proc/<pid>/exe`                                                                                                                                                                                    |
| Persistent cache/writeback rollback compatibility | Required release contract and release-admission gate                                                                       | Every pair of Drive9 CLI versions carried by adjacent release-admitted CSI images must pass N/N-1 bidirectional on-disk cache/writeback compatibility tests; manual workflow dispatch may publish a traceable validation image without claiming release admission |
| FD/state handoff                                  | Not implemented; current reexec work is a clean-state prototype and does not cover active CSI workloads                    | Not required for V1; future `mount rebuild` may use it to preserve open descriptors, otherwise use the explicit stop-and-remount fallback                                                                                                                         |

The current control protocol is drain-only and has no operation discriminator or
protocol envelope. A future version/status/shutdown/handoff extension must
introduce an explicitly versioned operation protocol; it must not send a new
request shape to an old drain-only server and assume that unknown fields are
rejected.

The previous active argv is stored without credentials and reused for fallback,
so a newer CSI does not pass newly introduced flags to an older Drive9 binary.
Rollback compatibility is a hard build/release invariant, not a runtime
negotiation performed by CSI. Release validation must cover N-1 state opened by
N and then reopened by N-1 after an injected desired-start failure. The newer
CSI image must not be release-admitted if that gate fails. A manual dispatch may
publish an immutable validation tag while the gate is unverified. For artifacts
admitted by this gate, CSI may attempt the previous active binary after fully
cleaning a post-`execve` desired failure. If both candidates fail, it preserves
recovery state and reports Degraded.

### Changes required

| File                                             | Change                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/driver/mount_linux.go`                 | `startDrive9Mount`: persist `phase=starting` and immutable `startupDeadline` before side effects; create attempt-scoped startup files under `/run/drive9-csi`; nsenter + systemd-run transient service with `--collect`, `Restart=no`, and explicit `TimeoutStopSec`; classify collected `not-found` units in the recorded-attempt context; SHA-256 unit naming; PID discovery from Drive9 process-state JSON; complete mount argv via NUL-separated files + drive9-csi-launcher; promote verified state to `active`; desired-first recovery with a recorded previous-active fallback |
| `cmd/drive9-csi-launcher/main.go`                | New ~50-line binary: reads NUL-separated .env and .args files, immediately unlinks both files, then calls syscall.Exec                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `cmd/drive9-csi/main.go`, installer helper       | Dispatch `install-host-binaries` before CSI server flag parsing and Kubernetes client creation; implement/test ELF validation, content hashing, atomic Drive9/launcher installation, and desired-symlink replacement in Go                                                                                                                                                                                                                                                                                                                                                            |
| `Dockerfile`, `Makefile`                         | Build the launcher with `CGO_ENABLED=0` for the target architecture; validate the launcher and Drive9 ELF machine and absence of `PT_INTERP`; copy the launcher into the CSI image at `/usr/local/bin/drive9-csi-launcher`; include it in local builds                                                                                                                                                                                                                                                                                                                                |
| `.github/workflows/publish-image.yml`            | Resolve one pinned Drive9 release plus per-architecture SHA-256 values/artifacts and pass that same immutable input to tagging and every image build job                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `mem9-ai/drive9` release workflow and FUSE tests | Add a mandatory N/N-1 bidirectional cache/writeback compatibility gate: create pending state with N-1, open it with N, inject desired-start failure, reopen with N-1, and verify data, metadata, queue state, and final upload results before allowing the adjacent CSI image to publish                                                                                                                                                                                                                                                                                              |
| `internal/driver/mount_linux.go`                 | Cleanup: persist `phase=stopping` before side effects; drain through the active Drive9 binary, stop systemd service, use normal/lazy launcher kernel unmount; delete state only after verified terminal cleanup                                                                                                                                                                                                                                                                                                                                                                       |
| `internal/driver/mount_linux.go`                 | `pidStartTime`, `pidMatchesState`, `pidAlive`: read from `/host-proc/<pid>/...` instead of `/proc/<pid>/...`; make canonical `/host-proc/<pid>/exe == binaryPath` part of the ownership gate                                                                                                                                                                                                                                                                                                                                                                                          |
| `internal/driver/driver.go`                      | Node-only startup preflight with named capability failures and operation-specific degraded behavior; evaluate healthy local idempotency before Secret/API resolution; remove `shutdownNodeMounts()` from the CSI Node SIGTERM handler while leaving the controller path free of host-systemd prerequisites                                                                                                                                                                                                                                                                            |
| `internal/driver/node_recovery.go`               | Reconcile `stopping` without remount; reconcile `starting` desired/fallback attempts; active split-state matrix handling; systemd service liveness check; run binary GC only after complete inventory                                                                                                                                                                                                                                                                                                                                                                                 |
| `deploy/kubernetes/node.yaml`                    | Init container (content-addressed binary and launcher), host-proc volume, `/run/drive9-csi` hostPath, node-pressure priorityClassName                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| Mount state JSON                                 | Durable atomic writes; add `schemaVersion`, `phase`, start/stop attempt IDs, immutable per-attempt `createdAt`/`startupDeadline`, startup artifact paths, resolved `binaryPath`, non-secret active/fallback argv, `fallbackBinaryPath`, `systemdUnit`, `controlSocketPath`, and stopping timestamp; never store API key/token                                                                                                                                                                                                                                                         |

### Verification plan (e2e)

1. **Preflight and operation-specific degradation**: CSI driver starts, passes
   all preflight checks, and logs success. Then inject individual launch-path
   failures after a healthy mount exists: new mount/recovery must fail before
   side effects, while a locally verifiable healthy `NodeStageVolume` remains
   idempotent, `NodePublishVolume` does not require systemd launch capability,
   and unpublish/unstage enter their normal safe cleanup paths
2. **Mount + cgroup**: `NodeStageVolume` creates mount via systemd service;
   verify `/host-proc/<pid>/cgroup` shows `system.slice/drive9-mount-*.service`,
   NOT `kubepods/` (all `/proc/<pid>` checks in e2e run against
   `/host-proc/<pid>` since mount process is in host PID namespace); verify
   process-state and control socket are under host `/run/drive9-csi`; verify
   `/host-proc/<pid>/exe` resolves exactly to the active state's `binaryPath`,
   and inject an executable mismatch to verify it fails closed without stopping
   the foreign process
3. **Rolling update**: `kubectl rollout restart ds/drive9-csi-node` — business
   Pod maintains open fd loop (read/write/fsync), verify mount PID/startTime/
   mount id and active `binaryPath` unchanged even when the new CSI image
   installs a different desired Drive9 binary
4. **Pod delete**: `kubectl delete pod drive9-csi-node-xxx` — same verification
   as rolling update
5. **New Pod mount**: create new PVC + Pod after upgrade, confirm new binary
   version by checking: (a) mount state `binaryPath` points to new versioned
   path, (b) `/host-proc/<pid>/exe` of the new mount process resolves to the new
   binary. V1 does not require a control-socket version RPC
6. **No implicit rebuild**: leave an old mount active after CSI rollout and
   verify desired/active mismatch is preserved without stopping its service
7. **NodeUnstageVolume + stopping failpoints**: delete all consumer Pods and
   verify CSI persists `phase=stopping` before drain/stop/unmount. Terminate CSI
   after the stopping write, after drain, after service stop, after unmount, and
   before state deletion. On every restart verify recovery continues cleanup,
   never starts desired or fallback, and deletes state only after mount, PID,
   service, socket, and attempt files are absent
8. **Starting transaction failpoints**: terminate CSI after each of: durable
   `starting` write, startup-file creation, `systemd-run` return, and mount
   readiness before active-state commit. On restart verify the exact attempt is
   either promoted or fully cleaned, with no duplicate service. Restart before
   and after the persisted `startupDeadline`; verify retries never reset it, an
   expired non-ready attempt does not begin another full wait, and an already
   ready fully verified attempt is promoted without waiting
9. **Starting-state RPC retry**: retry `NodeStageVolume` while a recoverable
   `starting` state exists and verify it adopts/reconciles that attempt before
   creating any new startup files. Delete or deny access to the referenced
   Secret while a mount is healthy and verify the idempotent active path still
   succeeds without a Kubernetes or Drive9 API call; then lose the process and
   verify recovery preserves state and reports the credential error
10. **Mount crash boundary + desired-first recovery**: start with active binary
    A, install desired binary B, then `kill -9` the mount PID and verify systemd
    does not restart it automatically. Restart the CSI Pod and verify recovery
    removes the disconnected host mount, starts B, and records B as active.
    Inject a B startup failure after B has opened A's persistent cache/writeback
    state; verify B is fully absent before A is attempted, and verify A recovers
    pending data, metadata, queue state, and final upload results. If A succeeds
    it remains active. This N/N-1 bidirectional case is a mandatory
    image-publishing gate. An already-running business Pod under the V1 legacy
    root-publish layout is expected to retain the broken old reference and must
    not be reported as transparently healed before subtree PR #24 is merged
11. **Idempotency**: call `NodeStageVolume` twice for same volume → second call
    returns success without creating duplicate mount
12. **Binary GC**: after all old mounts are cleaned, verify unreferenced old
    version files are removed while the current symlink target and
    `drive9-csi-launcher` remain. Inject an uncorrelated candidate mount unit or
    process-state artifact and verify that startup skips GC; verify binaries in
    `starting`, `active`, and `stopping` state, plus both candidates of an
    in-flight desired/fallback recovery, are retained
13. **Node drain**: run `kubectl drain --ignore-daemonsets`; verify consumer
    Pods are evicted and their volumes are unpublished/unstaged while the CSI
    DaemonSet Pod remains on the node
14. **Runtime credential state**: after mount readiness verify startup files are
    absent; process-state is under `/run/drive9-csi` with mode `0600`; CSI
    durable state contains no API key/token; and systemd properties, argv, and
    CSI/Drive9 logs do not contain the real key. The Drive9 process-state may
    contain it as accepted runtime state and test output must never print it
15. **Secret cleanup on failure**: trigger a handled mount failure and verify
    startup files are removed; terminate CSI before launcher start and verify
    `starting` reconciliation removes the attempt-scoped files. Make the
    launcher exit immediately so `--collect` can garbage-collect the transient
    unit before observation; verify `not-found` is attributed to the recorded
    failed attempt, cleanup succeeds without unit exit metadata, and journal
    retrieval is best-effort
16. **Launcher byte preservation**: invoke the launcher with a probe executable
    and synthetic values containing embedded `$`, backticks, spaces,
    single/double quotes, newlines, and semicolons. Verify the probe receives
    the exact values without shell interpretation
17. **Volume ID contract**: verify valid managed-directory and workspace-root
    IDs produce bounded unit names. Submit an arbitrary ID longer than 200
    characters and verify existing volume-context identity validation rejects it
    before creating startup files or a systemd unit
18. **NUL byte rejection**: attempt mount with a K8s Secret value containing a
    NUL byte. Verify `NodeStageVolume` returns `INVALID_ARGUMENT`, no .env/.args
    files are created, and no service unit is started
19. **Surrounding whitespace rejection**: attempt mount with a credential
    containing leading or trailing ASCII whitespace. Verify `NodeStageVolume`
    returns `INVALID_ARGUMENT` before writing startup files
20. **Runtime-directory lifecycle**: restart only the CSI Pod and verify live
    process-state/socket files remain; reboot a test node and verify stale
    runtime files disappear while durable `phase=active` state drives
    desired-first remount. Repeat with `phase=stopping` and verify reboot
    cleanup never remounts

### Follow-up verification gates

These gates apply to the phased follow-ups, not the V1 implementation:

1. **Subtree recovery**: after PR #24 is merged, kill the FUSE process and
   verify a running Pod sees the repaired child mount while a pre-existing open
   descriptor remains tied to the old connection
2. **Explicit rebuild**: with multiple Pods sharing one node/volume, verify
   `kubectl drive9 mount rebuild` changes active to node-local desired binary
   without waiting for `NodeUnstageVolume`
3. **Crash watcher**: verify a unit failure triggers desired-first recovery,
   falls back only after complete desired-attempt cleanup, and enters `Degraded`
   after the configured backoff threshold when no safe candidate succeeds
4. **Permanent uninstall**: verify cleanup refuses active consumers, removes all
   verified inactive mounts, and leaves no systemd service or host state before
   CSI resources are deleted
