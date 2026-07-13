---
title: CSI Direct Mount Strict and Host FUSE Helper Removal Design
status: implemented
date: 2026-07-12
---

## Status

Implemented by `92b52a8`. Drive9 direct mounting is mandatory and the CSI
dependency on `fusermount3` is removed completely. There is no runtime switch or
helper fallback. Validation artifacts remain subject to the release-admission
gate in the mount-survival design; implementation does not itself authorize
production deployment.

This design narrowly supersedes the host FUSE-helper requirements in
`docs/design/csi-rolling-upgrade-mount-survival-v2.md`. All mount-survival,
ownership, state-machine, recovery, secret, and release-admission invariants in
that document remain authoritative unless this document explicitly replaces a
helper-specific step.

## Pre-change Problem

The pre-change CSI node path installed an image-provided `fusermount3` binary
into the host filesystem and made it part of node startup readiness:

```text
CSI image
  -> init container validates image /usr/bin/fusermount3
  -> atomically installs /var/lib/drive9-csi/bin/fusermount3
  -> node preflight executes fusermount3 --version in the host root
  -> Drive9 mount/umount can discover the helper through PATH
```

This adds a host ABI dependency, installer steps, preflight capability state,
manifest arguments, image packages, failure modes, and tests. It is also
unnecessary when the packaged Drive9 CLI mounts through the Linux mount syscall
and refuses to fall back to a FUSE helper.

Drive9 CLI `a53e497` provides `--direct-mount-strict`. On Linux FUSE mounts the
flag requests direct `mount(2)` and returns an error instead of invoking
`fusermount3` or `fusermount` as a fallback.

## Verified Evidence

The following validation ran on 2026-07-12:

1. Local `drive9 mount --help` for CLI `a53e497` exposed `--direct-mount-strict`
   with Linux-only, no-fallback semantics.
2. Manual workflow run
   `https://github.com/drive9-ai/k8s-csi/actions/runs/29177941414` published:
   `ghcr.io/drive9-ai/drive9-csi@sha256:356701c8026106d57f78e77016c999d39c3531ff6b1bb719222ae7bcac7eb4f2`.
3. An isolated privileged Pod on the arm64 Amazon Linux 2023 dev cluster ran
   that exact image with only `/dev/fuse` and `emptyDir` volumes. Its Drive9
   process had a PATH in which neither `fusermount3` nor `fusermount` was
   discoverable.
4. Direct strict mount readiness and temporary file write, read, sync, and
   deletion succeeded.
5. Process termination alone left a FUSE mount requiring kernel cleanup.
   External unmount alone did not terminate the foreground Drive9 process.
   Process stop followed by ordinary kernel unmount completed successfully.
6. The target EKS hosts do not provide a native `/usr/bin/fusermount3` or
   `/bin/fusermount`; only the current CSI init container installs the custom
   helper.

The isolated probe did not exercise the complete CSI argv, including
`--allow-other`, or the host systemd launch path. Those are required targeted
runtime gates before this change is considered validated for a CSI rollout.

## Decision

All Drive9 FUSE mounts created by this repository use strict direct mounting:

```text
drive9 mount
  --foreground
  --mode=fuse
  --direct-mount-strict
  --allow-other
  ...
```

The CSI driver does not expose a feature flag, environment variable,
StorageClass parameter, or VolumeAttributesClass parameter for this behavior. An
image is either built against a compatible Drive9 CLI and uses strict direct
mounting everywhere, or the image is invalid.

The lifecycle remains:

```text
CSI Node Pod
  -> host mount namespace and host root
  -> host systemd transient mount service
  -> drive9-csi-launcher
  -> drive9 mount --foreground --direct-mount-strict
  -> /dev/fuse and mount(2)
```

Intentional teardown remains owned by `NodeUnstageVolume` and recovery:

```text
drain
  -> stop verified systemd service/process
  -> verify mount state
  -> launcher unix.Unmount
  -> lazy unix.Unmount when required
  -> verify terminal state
  -> delete durable state
```

No new CSI or sidecar cleanup path invokes `drive9 umount`, `fusermount3`, or
`fusermount`. A live process created by an older image retains its original
binary, argv, and environment until it is safely stopped; that legacy
coexistence is not part of the strict release-admission claim.

## Goals

1. Add `--direct-mount-strict` exactly once to every new CSI and sidecar FUSE
   mount.
2. Remove host `fusermount3` installation, validation, preflight, capability,
   PATH, and cleanup dependencies.
3. Remove the `fuse3` runtime package and `/etc/fuse.conf` mutation from the CSI
   image when the exact `--allow-other` runtime gate passes.
4. Preserve mount survival across CSI Pod replacement.
5. Preserve fail-closed process ownership, crash-safe mount state, safe cleanup,
   secret hygiene, and desired-first recovery.
6. Make legacy non-strict state behavior explicit instead of silently relying on
   a helper that may or may not remain on a node.
7. Keep publishing build-only and runtime validation local and manual.

## Non-Goals

1. Adopting the separate `hostPID: true` process-control draft.
2. Removing the host mount namespace, host root, host `/proc`, or systemd
   transient mount service.
3. Running Drive9 as a child of the CSI container.
4. Changing CSI Controller, volume identity, credential, staging, publish, or
   access-mode semantics.
5. Rebuilding healthy mounts merely because their argv lacks the strict flag.
6. Adding FD handoff, continuous crash watching, subtree recovery, or a
   permanent-uninstall controller.
7. Deleting an old host `fusermount3` file during normal rollout.
8. Adding a heavy E2E suite or running cluster validation from
   `publish-image.yml`.

## Preserved Invariants

1. A mount lives in the host mount namespace and a host systemd service cgroup,
   outside the CSI Pod cgroup.
2. CSI process shutdown does not unmount volumes.
3. `starting`, `active`, and `stopping` state is persisted atomically before the
   corresponding side effects.
4. Process ownership requires exact staging-target argv, expected systemd
   unit/cgroup, PID start time, and canonical executable path.
5. Healthy idempotent RPCs use verified local state before Secret or API access.
6. Degraded capabilities remain operation-specific.
7. Credentials cross the systemd boundary only through bounded NUL-separated
   startup files and are never persisted in CSI durable state or argv.
8. Desired and fallback candidates never run concurrently.
9. Content-addressed Drive9 installation and complete-inventory-before-GC remain
   unchanged.
10. A state record is deleted only after mount, PID, service, startup files,
    process state, and control socket reach the verified terminal condition.
11. Manual validation images do not imply release admission, and the existing
    N/N-1 cache/writeback compatibility gate remains required.

## Detailed Design

### Fixed Mount Argument Contract

`internal/driver/mount_args.go` adds `--direct-mount-strict` to the common
argument prefix immediately after `--mode=fuse`. The centralized argument
builder is the source of truth for new `NodeStageVolume` mounts and desired
recovery attempts.

The rules are:

1. The flag is always present and appears exactly once.
2. It is not derived from user-controlled volume parameters.
3. It remains in the non-secret `mountArgs` persisted in mount state.
4. Readiness and ownership checks continue comparing exact argv.
5. Recovery never appends the flag to a stored candidate argv after the fact.

`deploy/sidecar/deployment.yaml` uses the same image and invokes Drive9
directly. Its fixed mount command also adds `--direct-mount-strict`; otherwise
removing `fuse3` from the shared image would break the sidecar path.

### Image Contract

The runtime image removes:

1. The Debian `fuse3` package.
2. The `/etc/fuse.conf` `user_allow_other` mutation.
3. Any assumption that a helper exists at `/usr/bin/fusermount3`.

It retains `ca-certificates`, `tar`, `tini`, and `util-linux`. The Drive9 and
CSI binaries remain static Linux binaries for the target architecture.

The packaged Drive9 CLI is a build-time compatibility dependency. On each
published architecture, the image build runs the no-side-effect command
`drive9 mount --direct-mount-strict --help` and requires exit code zero. A CLI
that does not recognize the flag fails during argument parsing. This is an
artifact contract check, not a CLI-version lock and not cluster validation.
`publish-image.yml` remains manual-only and continues to build, push, and output
tag/digest without deployment or E2E behavior.

The executable probe runs in the final image or a dedicated
`FROM --platform=$TARGETPLATFORM` stage. It must not run a downloaded arm64
binary in a `BUILDPLATFORM=amd64` downloader stage. The native per-architecture
workflow jobs execute the real probe. macOS `make check` validates the
Dockerfile contract statically; it does not claim to execute a Linux target
binary locally.

### Host Binary Installer

`install-host-binaries` installs only:

1. Content-addressed `drive9-<sha256>`.
2. The atomic `drive9` desired symlink.
3. `drive9-csi-launcher`.

The installer removes:

1. `--fusermount-source`.
2. `FusermountSource` and `FusermountPath` result fields.
3. Helper ELF validation and reads.
4. Helper temporary-write, fsync, rename, and fault-injection steps.
5. `fusermount_path` command output.

There is no compatibility shim that accepts and ignores `--fusermount-source`. A
DaemonSet template and CSI image are a paired artifact; mixing a new image with
an old init-container argument fails closed. Rollback must restore the previous
manifest and image together.

The new installer does not delete an existing
`/var/lib/drive9-csi/bin/fusermount3`. A node may still have a healthy mount
created by an older image, and normal rollout must not mutate artifacts outside
the new install contract. The stale file is ignored and is not put on the Drive9
runtime PATH. Explicit legacy-artifact removal is a separate maintenance
operation after complete inventory proves that no legacy mount requires it.

### Node Preflight and Capabilities

Node preflight deletes:

1. `hostFusermountPath`.
2. `nodeCapabilityFUSEHelper` and its failure reason.
3. `checkHostFUSEHelper`.
4. The helper requirement in `checkInstalledHostBinaries`.

It retains checks for:

1. Host `/proc`, mount namespace, root, and PID signaling.
2. Host systemd transient services, `systemctl`, and `journalctl`.
3. A readable and writable `/dev/fuse` character device.
4. Safe writable runtime and durable state directories.
5. Installed Drive9 and launcher binaries.
6. Content-addressed desired Drive9 integrity and host execution.

After validating the desired binary, preflight runs
`<desired-drive9> mount --direct-mount-strict --help` through the existing
short-lived host execution path and requires exit code zero. This duplicates the
image contract at the node boundary and fails before any `NodeStageVolume` side
effect if an incompatible image or desired binary is installed. The result is
part of the existing Drive9 execution capability rather than a user-selectable
mount mode.

Removing the helper capability must not change capability ordering semantics for
the remaining entries or convert preflight into a global RPC rejection switch.

### Runtime Environment

The mount service receives the existing fixed `TMPDIR` and `XDG_RUNTIME_DIR`.
Its PATH becomes the normal host system path:

```text
/usr/sbin:/usr/bin:/sbin:/bin
```

`/var/lib/drive9-csi/bin` is removed from PATH so an ignored legacy helper
cannot be selected accidentally. Drive9 and the launcher are already invoked
through validated absolute content-addressed paths.

### Cleanup

The current `drive9 umount` fallback is removed. It exists only as an OS-helper
selection layer and is redundant with the compiled launcher's direct
`unix.Unmount` operation.

Cleanup keeps the existing ownership gate, stopping-state write, best-effort
ordering, and terminal verification:

1. Drain through the active Drive9 control socket.
2. Stop the verified systemd service and wait through its bounded stop timeout.
3. Check whether the staging target remains mounted.
4. If mounted, invoke `drive9-csi-launcher host-unmount -- <target>`.
5. If busy or still mounted, invoke the same operation with `--lazy`.
6. Verify or kill the fully owned remaining PID as already specified.
7. Delete runtime artifacts and durable state only after terminal verification.

The dev probe showed that signal termination must not be treated as proof of
unmount and unmount must not be treated as proof of process termination. Both
terminal conditions remain independently verified.

### State and Recovery Compatibility

Adding a fixed argv element does not change the mount-state JSON shape, so this
design does not require a schema-version bump. It changes which stored argv is
eligible to launch.

<!-- markdownlint-disable MD013 -->

| Existing state                                                | Behavior after rollout                                                                               |
| ------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| Healthy `active` mount with strict argv                       | Preserve it; normal idempotent path                                                                  |
| Healthy `active` mount without strict argv                    | Preserve it; do not rebuild merely to add the flag                                                   |
| Fully ready, fully owned `starting` mount without strict argv | It may be promoted because no new helper-dependent mount is created                                  |
| Non-ready `starting` attempt without strict argv              | Reconcile and clean it; never relaunch the old argv                                                  |
| `stopping` state without strict argv                          | Continue drain, service stop, and direct kernel cleanup                                              |
| Desired new mount or desired recovery                         | Build new argv with strict enabled                                                                   |
| Fallback candidate whose stored argv contains strict          | Eligible under the existing desired-first and N/N-1 gates                                            |
| Fallback candidate whose stored argv lacks strict             | Ineligible; do not launch it and preserve the desired `starting` state as inspectable Degraded state |

<!-- markdownlint-enable MD013 -->

The eligibility rule prevents a helper-dependent N-1 argv from silently
reintroducing the deleted dependency. CSI must not append the new flag to an old
candidate because the old binary may not support it.

The durable representation does not add a `degraded` phase. After the desired
attempt has been fully cleaned, fallback eligibility is checked before creating
or persisting a fallback attempt. For an ineligible fallback:

1. Keep the existing recovery `starting` record unchanged. It continues to
   contain the strict desired `BinaryPath`/`MountArgs`, the pre-strict
   `FallbackBinaryPath`/`FallbackMountArgs`, and the original immutable
   `StartupDeadline`.
2. Do not generate a fallback attempt ID, write a fallback candidate, create
   startup files, or launch a service.
3. Return the existing degraded reconciliation result with an actionable
   `FAILED_PRECONDITION` explaining that the recorded fallback predates the
   strict mount contract.
4. Before the immutable deadline, reconciliation may resume the same desired
   attempt under the existing rules, including after a process restart. It never
   resets the deadline.
5. After deadline expiry, restart and RPC retries return the same failure
   without launching either candidate or rewriting the state.
6. `NodeUnstageVolume` may transition that preserved `starting` record through
   the existing cancel-start/stopping cleanup and remove it only after terminal
   verification.

The eligibility check is applied both by immediate active recovery and by
startup reconciliation before the existing desired-to-fallback state transition.
An in-process known failure returns immediately, but no unpersisted failure
marker is assumed to survive a crash. Retries remain bounded by the
already-persisted immutable deadline, so this design needs no new field or
schema version.

The first strict-only release therefore has an explicit admission boundary:

1. Validation images may run on clean dev nodes and are not release-admitted.
2. Release admission requires either the existing clean-state rollout boundary
   or an adjacent N-1 image whose mounts already record strict argv.
3. The N/N-1 bidirectional cache/writeback gate still applies to every fallback
   candidate; strict mounting does not weaken that gate.
4. A pre-strict fallback is not claimed to work after helper removal.

If the installed N-1 image can create non-strict mounts, N must not be
advertised as a transparent rolling upgrade. Operators must drain and unstage
those mounts and satisfy the clean-state boundary before rollout. Transparent
N/N-1 rolling admission resumes only when both adjacent admitted images create
strict mounts.

### Rollout and Rollback

Rollout behavior:

1. The init container installs Drive9 and the launcher but neither installs nor
   deletes a helper.
2. Healthy old mounts remain pinned to their existing binary and argv.
3. New mounts and unavoidable desired recovery use strict direct mounting.
4. Old and new mount processes may coexist temporarily on a validation node, but
   a pre-strict coexistence rollout is not release-admitted as transparent N/N-1
   recovery.
5. An old helper file may remain on disk but is neither checked nor discoverable
   through the CSI mount-service PATH.

Rollback behavior:

1. Rollback applies the old DaemonSet template and image together.
2. The old init container reinstalls its expected helper.
3. A healthy strict mount remains pinned and is not rebuilt solely because the
   desired image changed.
4. Recovery continues using each candidate's exact persisted argv; no version
   receives an argument it did not previously run.
5. Cache/writeback rollback remains subject to the existing N/N-1 gate.

### Sidecar Path

The fallback sidecar is not host-systemd-managed, but it shares the runtime
image and `/dev/fuse` contract. It must add the fixed strict flag before the
image can remove `fuse3`.

The current sidecar manifest pins an older immutable CSI image. It is replaced
with the fail-closed `registry.invalid/drive9-csi:unpublished` placeholder.
Operators and targeted validation must explicitly override it with a
strict-capable immutable reference. This avoids a circular requirement to pin an
image containing the new supervisor before that implementation image can be
published. Adding the flag while retaining an older CLI remains invalid.

The current sidecar uses `exec drive9 mount` and relies on process termination
for cleanup. That is unsafe after helper removal because the verified strict
probe showed that process exit does not guarantee unmount, while the sidecar
mount is bidirectionally propagated to the host.

After its existing remote-directory preparation, the sidecar shell uses `exec`
to hand control to a new, narrowly scoped `drive9-csi supervise-sidecar-mount`
subcommand. The subcommand is dispatched before CSI flag parsing or Kubernetes
client creation and becomes the container PID 1. It accepts only the packaged
Drive9 binary, a strict foreground mount argv, and the exact canonical target
`/mnt/drive9`; it is not a generic process or unmount wrapper.

The compiled supervisor:

1. Start `drive9 mount --foreground --direct-mount-strict` as a child rather
   than replacing the supervisor with the Drive9 process.
2. Handle TERM and INT with an internal one-shot cleanup state machine; child
   exit and signal handling cannot run cleanup concurrently.
3. On termination, request a bounded `drive9 mount drain`, send TERM to the
   child, and wait within the Pod termination grace period.
4. If the child remains after the TERM bound, send KILL and perform another
   bounded wait.
5. Call normal `unix.Unmount` for `/mnt/drive9` directly.
6. If the normal unmount returns busy, retry with `MNT_DETACH`.
7. Independently verify the child is gone and the mount is absent before
   returning success.
8. Run the same idempotent cleanup after an unexpected child exit.

Drain, TERM, and wait failures are recorded but do not skip KILL or unmount.
`EINVAL` or `ENOENT` from unmount is idempotent success; permission and other
unexpected errors remain failures. The supervisor exits nonzero when terminal
verification fails. Its process, signal, wait, mount-observation, and unmount
operations are injectable for unit tests, including macOS tests that do not
perform a real Linux mount.

The sidecar retains privileged mode, `SYS_ADMIN`, `/dev/fuse`, bidirectional
mount propagation, credential flow, cache, and tuning parameters. Its
termination grace period must exceed the drain and child-stop bounds.

### Security

Direct mounting still requires the existing root, privileged, `SYS_ADMIN`, and
host mount-namespace trust boundary. This design does not broaden that trust.

It reduces host attack surface by removing an extra host executable, dynamic
loader/library compatibility, helper PATH selection, and helper replacement
logic. Strict failure also prevents a deployment from silently switching to a
different mount mechanism than the one reviewed and tested.

Credentials remain absent from argv, systemd properties, durable mount state,
logs, and image artifacts.

## Failure Handling

<!-- markdownlint-disable MD013 -->

| Failure                                        | Required behavior                                                                   |
| ---------------------------------------------- | ----------------------------------------------------------------------------------- |
| `/dev/fuse` missing or unusable                | Node launch capability unavailable; no mount side effect                            |
| Direct `mount(2)` fails                        | Preserve/reconcile `starting` state and report the launch error; never try a helper |
| Packaged or installed CLI lacks strict support | Image build or node preflight fails before mount side effects                       |
| Legacy non-strict fallback is selected         | Reject it as ineligible and preserve Degraded state                                 |
| Systemd stop leaves a mount                    | Direct launcher kernel unmount, then lazy detach if required                        |
| Kernel unmount fails or PID/service survives   | Preserve `stopping` state and return an actionable error                            |
| Old helper file exists                         | New strict paths ignore it; do not delete it during rollout                         |
| New image is paired with old init args         | Installer rejects unknown `--fusermount-source` and rollout fails closed            |
| Sidecar child exits or receives TERM           | Bounded drain, TERM/KILL escalation, and exact-target normal/lazy syscall unmount   |

<!-- markdownlint-enable MD013 -->

## Affected Surfaces

<!-- markdownlint-disable MD013 -->

| Surface                                            | Required change                                                                                |
| -------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| `internal/driver/mount_args.go`                    | Add fixed strict flag to all desired CSI mount argv                                            |
| `internal/driver/mount_lifecycle.go`               | Enforce strict on newly launched primary argv                                                  |
| `internal/driver/active_recovery.go`               | Check stored fallback eligibility before persisting or launching fallback                      |
| `internal/driver/mount_reconcile_starting.go`      | Apply the same eligibility gate during crash replay and never resume non-ready pre-strict argv |
| `internal/driver/mount_state.go`                   | Continue reading valid V2 non-strict state while testing strict/new versus legacy eligibility  |
| `internal/driver/node_preflight.go`                | Remove helper path, capability, execution, and installed-binary checks                         |
| `internal/driver/mount_stop.go`                    | Remove `drive9 umount`; retain direct normal/lazy kernel unmount                               |
| `cmd/drive9-csi/install_host_binaries.go`          | Remove helper input, validation, installation, output, and fault steps                         |
| `cmd/drive9-csi/main.go`, sidecar supervisor files | Dispatch and implement the bounded exact-target sidecar lifecycle                              |
| `Dockerfile`                                       | Remove `fuse3` and `/etc/fuse.conf` mutation; verify packaged strict capability                |
| `deploy/kubernetes/node.yaml`                      | Remove `--fusermount-source`                                                                   |
| `deploy/sidecar/deployment.yaml`                   | Add strict flag, fail-closed image placeholder, and compiled supervisor invocation             |
| `hack/check-manifests.go`                          | Replace helper expectations with strict/no-helper expectations                                 |
| Unit tests                                         | Update installer, capability, argv, recovery, cleanup, and manifest contracts                  |
| `README.md`, `AGENTS.md`, rolling design           | Document strict direct mount and superseded helper requirements                                |

<!-- markdownlint-enable MD013 -->

## Acceptance and Validation

### Local Implementation Acceptance

Local acceptance covers code quality and unit-level contracts on macOS:

1. `drive9MountArgs` always emits exactly one `--direct-mount-strict` before
   positional arguments.
2. Desired stage and recovery use strict argv.
3. Stored fallback argv is never mutated; a non-strict fallback is ineligible.
4. Installer tests cover atomic Drive9/launcher installation without helper
   inputs, outputs, or fault steps.
5. Preflight tests prove helper absence is irrelevant, the no-side-effect strict
   capability probe fails closed for an incompatible CLI, and `/dev/fuse` plus
   every remaining host capability remain enforced.
6. Cleanup tests prove no `drive9 umount`, `fusermount3`, or `fusermount`
   command is constructed and normal/lazy launcher unmount remains fail-closed.
7. Manifest checks require no `--fusermount-source` and require strict sidecar
   argv, the fail-closed image placeholder, and the compiled supervisor
   contract.
8. Image/build-artifact checks require no `fuse3` package or `fuse.conf`
   mutation and verify the packaged CLI capability.
9. Production-code search contains no `fusermount`, `hostFusermount`, or
   `FUSEHelper` dependency in new mount or cleanup paths.
10. Sidecar supervisor tests cover drain failure, TERM success, TERM timeout
    followed by KILL, unexpected child exit, EBUSY-to-lazy fallback,
    EINVAL/ENOENT idempotency, permission failure, terminal verification, and
    duplicate cleanup requests.
11. `make test`, `make build`, and `make check` pass from the macOS development
    environment.
12. Implementation and documentation remain consistent with this design and the
    preserved mount-survival invariants.

No Drive9 filesystem baseline, Linux VM, `hdiutil`, or broad real-cluster E2E is
part of local implementation acceptance.

### Targeted Dev-Cluster Validation

Runtime validation is a separate local/manual step. Every command must use the
explicit non-production context and Driver namespace.

Use an immutable image that physically lacks `fusermount3` and run these focused
checks:

1. Confirm the packaged Drive9 CLI version and strict flag.
2. Confirm the node has no CSI-installed helper at the expected host path on a
   clean test node.
3. Prepare the CSI environment and verify Node preflight is healthy without a
   helper capability.
4. Create one disposable PVC/Pod and verify the host process argv contains
   `--direct-mount-strict` and `--allow-other`.
5. From a non-root workload process, read, write, sync, and delete a temporary
   file through the CSI volume.
6. Restart or replace the CSI node Pod and verify the mount PID, start time,
   systemd unit, binary path, and workload I/O remain unchanged.
7. Remove consumers and verify `NodeUnstageVolume` drains, stops the service,
   performs direct kernel unmount when needed, and reaches terminal cleanup
   without any helper.
8. Exercise one desired recovery after a disposable mount process loss and
   verify the replacement is strict.
9. Restart and delete one strict sidecar Pod; verify its propagated host mount
   is absent and no disconnected mount remains.
10. Verify test-owned resources and temporary remote files are removed while the
    prepared Driver remains intact.

These checks do not run in `publish-image.yml` and do not expand into a general
cluster conformance suite.

## Alternatives

<!-- markdownlint-disable MD013 -->

| Alternative                                  | Assessment                                                                            |
| -------------------------------------------- | ------------------------------------------------------------------------------------- |
| Optional strict flag with legacy fallback    | Rejected: retains helper installation and doubles behavior/test matrices              |
| Direct mount with helper fallback            | Rejected: failure can silently use an unreviewed host path                            |
| Keep helper only for pre-strict fallback     | Rejected: preserves a nondeterministic host dependency and weakens the fixed contract |
| Retain `drive9 umount` before kernel unmount | Rejected: redundant OS-helper selection layer                                         |
| Remove host systemd or mount namespace too   | Rejected: unrelated to direct mounting and breaks mount survival                      |
| Adopt `hostPID: true` in the same change     | Rejected: independent trust and process-control decision                              |

<!-- markdownlint-enable MD013 -->

## Completion Criteria

The design is implemented only when:

1. Every repository-owned FUSE mount path is strict.
2. CSI no longer installs, validates, or depends on a FUSE helper, and no new
   strict mount or repository-owned cleanup path invokes one.
3. The runtime image no longer contains the helper package.
4. Process stop plus direct kernel unmount reaches the existing verified
   terminal state.
5. Legacy state and release-admission boundaries are enforced as documented.
6. Local acceptance and the focused dev-cluster validation both pass.
