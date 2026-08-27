---
name: k8s-csi
description: |
  Drive9 CSI Lite driver — minimal K8s CSI integration for drive9 FUSE mounts.
  Single Go module (go 1.26), two Linux binaries, no codegen.
---

<!-- markdownlint-disable MD013 -->

# Drive9 CSI Lite — Agent Reference

## Module & Package

- **Module**: `github.com/drive9-ai/csi` (single `go.mod`, no workspace)
- **Binary**: `cmd/drive9-csi/main.go` → `bin/drive9-csi`
- **Launcher**: `cmd/drive9-csi-launcher/main.go` → `bin/drive9-csi-launcher`
- **Driver code**: `internal/driver/` is a single package with no sub-packages
- **Entrypoint** dispatches installer and verifier commands
  before creating an in-cluster K8s client and calling `driver.Run()`
- **No codegen, no protobuf**, no `//go:generate`, no mockgen, no
  controller-gen. CSI spec is consumed as Go module
  (`container-storage-interface/spec v1.11.0`).

## Build

```sh
make test     # gofmt check + go test ./...
make build    # CGO_ENABLED=0, default linux/amd64 → bin/drive9-csi
make image IMAGE=ghcr.io/drive9-ai/drive9-csi:local  # single-arch Docker build
make image-multi IMAGE=ghcr.io/drive9-ai/drive9-csi:<tag>  # multi-arch buildx + push
```

Override platform: `GOOS`, `GOARCH` env vars. Default
`GOPROXY=https://proxy.golang.org,direct`.

## Dockerfile

Multi-stage (`Dockerfile`):

1. `golang:1.26-bookworm` — compiles `drive9-csi` (CGO_ENABLED=0)
2. `debian:bookworm-slim` — downloads `drive9` CLI from
   `https://drive9.ai/releases/`
3. Target-platform `debian:bookworm-slim` — runtime: `tini`, `util-linux`,
   `tar`, and `ca-certificates`; entrypoint via `tini`

Both build stages validate target arch and static ELF contracts. The target
runtime stage executes
`drive9 mount --supervise-foreground --direct-mount-strict --help`; an
incompatible CLI fails the image build. The help must expose
`gvisor-compat`, `local-only`, `remote-only`, and the three corresponding
`DRIVE9_MOUNT_*` environment names used by the fallback sidecar; the minimum
compatible version is the first published Drive9 release containing those
contracts, not the older `dac2d62` supervisor minimum. The runtime image does
not contain `fuse3` and does not configure `/etc/fuse.conf`.

## Drive9 CLI Release Contract

The CSI image embeds a published Drive9 CLI. `https://drive9.ai/install.sh`
documents the public download layout, but the image build does not execute that
installer.

| Release resource                                | Contract                                                                                   |
| ----------------------------------------------- | ------------------------------------------------------------------------------------------ |
| `https://drive9.ai/releases/version`            | Exactly seven lowercase hexadecimal characters identifying the Drive9 source commit prefix |
| `https://drive9.ai/releases/checksums.txt`      | Exactly one SHA256 entry for each required artifact                                        |
| `https://drive9.ai/releases/drive9-linux-amd64` | Current published Linux amd64 CLI binary                                                   |
| `https://drive9.ai/releases/drive9-linux-arm64` | Current published Linux arm64 CLI binary                                                   |

The binary URLs are mutable and contain no version. Local image builds either
resolve a complete release from `version` plus `checksums.txt`, or pin
`DRIVE9_CLI_VERSION` together with the target platform checksum. CI reads the
version before and after the checksums to reject mixed releases, then records
the resolved CLI version and source commit in the image tag and labels. The
workflow publishes validation images, not release-admitted production images.

## Docker Image Tags

**No `:latest` tag.** Validation image tags follow the pattern:
`drive9-<drive9-cli-version>-csi-<sha7>`. Example: `drive9-aff1023-csi-ef5fab2`.
The CI publishes only these traceable tags. A tag or digest proves artifact
identity, not release admission. The N/N-1 cache/writeback compatibility gate
in the mount-survival design must pass before a fallback-capable image is
promoted to production.

## Tests

```sh
make test             # formatting + production Go unit tests
make build-check      # Linux artifact and ELF validation
make manifest-check   # Kubernetes deployment contract validation
make script-check     # drive9-csi-upload-perf shell behavior
make e2e-check        # non-mutating E2E structure validation
make check            # all local checks above, including race and vet
```

Go unit tests live beside production Go packages under `cmd/` and
`internal/driver/`. They cover shipped launcher/installer behavior, CSI RPCs,
mount lifecycle, recovery, state, host process ownership, systemd, and Secret
resolution.

Repository configuration and non-Go components are deliberately outside the Go
unit suite:

| Target           | Covers                                                                                 |
| ---------------- | -------------------------------------------------------------------------------------- |
| `build-check`    | Cross-compiled CSI/launcher binaries, ELF architecture, static linkage, build metadata |
| `manifest-check` | Deploy YAML, RBAC, Kustomize, Dockerfile and example contracts                         |
| `script-check`   | Performance upload shell helper behavior and secret hygiene                            |
| `e2e-check`      | E2E script safety and prepare/case ownership boundaries                                |

Tests use:

- `testing` stdlib — no test framework/suite, no `TestMain`
- Tests are in-package (`package driver`, not `package driver_test`) — they
  access unexported symbols directly
- `fakeDrive9` — `httptest.Server`-based HTTP handler simulating the Drive9 API
  (defined in `internal/driver/driver_test.go`; supports dirs, files, marker
  JSON, and transient error injection via `failDeleteOnce`)
- `k8sfake.NewSimpleClientset` — fake K8s API for Secret/PVC/PV operations
- `createPVForVolume` helper — simulates what `csi-provisioner` does after
  `CreateVolume`
- Error code assertions use `status.Code(err) != codes.InvalidArgument` — never
  raw `err == nil` checks for gRPC calls
- `t.Fatalf`/`t.Fatal` only (no `require`/`assert`)
- No `testdata/` directories, no test fixtures, no `.env` files
- Go tests must not read workflow, Dockerfile, Makefile, deploy YAML, or shell
  source files, and must not invoke `go build` for artifact acceptance.

CI does **not** run tests — `.github/workflows/publish-image.yml` only builds
and publishes images.

`test-linux-compile` validates Linux-only Driver test compilation from macOS.

## E2E Test (Real K8s)

```sh
export DRIVE9_CSI_E2E_CONTEXT=dev-dat9-eks-ap-southeast-1
export DRIVE9_CSI_E2E_DRIVER_NAMESPACE=drive9-csi
export DRIVE9_CSI_E2E_SECRET_NAME=drive9-csi-secret-flags-test
export DRIVE9_CSI_E2E_CONFIRM=1
e2e/prepare.sh --image-tag drive9-a53e497-csi-d91bfe3

e2e/basic-lifecycle.sh
e2e/mount-survival.sh
e2e/multi-node-rwx.sh
```

Run `prepare.sh` to create or update the persistent Driver environment, then run
any case repeatedly against it. Pass the completed publishing workflow's bare
trace tag literally through `--image-tag`. Cases reuse the pre-provisioned
namespace and Secret and create and clean only their own StorageClass, VAC, PVC,
and Pod resources. `multi-node-rwx.sh` requires two eligible nodes and separate
command approval. Every command requires explicit context and Driver namespace
values. Read `e2e/AGENTS.md` before modifying or running E2E.

## Architecture

The binary implements three CSI gRPC services. `--service-mode=controller`
registers Identity plus Controller; `--service-mode=node` registers Identity
plus Node:

| Service    | RPC handlers                                                                                                              |
| ---------- | ------------------------------------------------------------------------------------------------------------------------ |
| Identity   | `GetPluginInfo`, `GetPluginCapabilities`, `Probe`                                                                        |
| Controller | `CreateVolume`, `DeleteVolume`, `ControllerGetCapabilities`, `ValidateVolumeCapabilities`, `ControllerModifyVolume`      |
| Node       | `NodeStageVolume`, `NodeUnstageVolume`, `NodePublishVolume`, `NodeUnpublishVolume`, `NodeGetInfo`, `NodeGetCapabilities` |

### Volume Modes

Two provisioning modes are selected by StorageClass identity parameters and PVC
annotations. Mount behavior parameters may come from a VolumeAttributesClass at
volume creation time:

**Workspace-root** (default, no `remoteRootPrefix`):

- Mounts a Drive9 workspace root selected by API key
- `CreateVolume` checks the path exists but writes no CSI metadata
- `DeleteVolume` is a no-op for Drive9 data
- Volume ID prefix: `drive9-root-`

**Managed directory** (`remoteRootPrefix` parameter present):

- Creates a unique subdirectory under the prefix (e.g.,
  `/k8s/pvc/<name>-<sha12>`)
- Writes CSI metadata: marker file (`.drive9-csi-volume.json`), index at
  `/k8s/.drive9-csi/volumes/`, name-index at `/k8s/.drive9-csi/volumes/by-name/`
- `DeleteVolume` removes only CSI metadata — never deletes user data

### Access Modes

- `SINGLE_NODE_WRITER`, `SINGLE_NODE_SINGLE_WRITER`, and
  `SINGLE_NODE_MULTI_WRITER` are supported for single-node volumes.
- `MULTI_NODE_MULTI_WRITER` backs Kubernetes `ReadWriteMany` volumes.
- RWX adds no `profile` or `durability` default or restriction. Explicit mount
  parameters are validated and forwarded to `drive9 mount`.
- A request cannot mix single-node and multi-node writer capabilities. Active
  publish targets for one volume must use the same multi-writer mode.

### VolumeAttributesClass

The external provisioner sends VolumeAttributesClass parameters through
`CreateVolumeRequest.mutable_parameters`. Supported create-time keys are
`profile`, `durability`, `gvisorCompat`, `localOnlyPatterns`,
`remoteOnlyPatterns`, the three TTLs, `perfEnabled`, and the five tuning
parameters. They override matching legacy StorageClass parameters by complete
value. Identity parameters such as `remoteRoot` and `remoteRootPrefix` are not
mutable.

`gvisorCompat` defaults to `false` and is always emitted as an explicit Drive9
flag. The policy values contain one pattern per line; the driver trims blanks,
deduplicates exact values in order, persists the canonical newline-delimited
form, and emits repeated equals-form flags. Native CSI does not forward the
matching Drive9 environment variables.

The driver advertises `MODIFY_VOLUME` so external-provisioner can provision a
PVC that references a VAC, but `ControllerModifyVolume` returns `Unimplemented`
for a valid update request after validating its keys and values.
Changing an existing PVC's VAC is unsupported; recreate the volume to apply new
mount parameters.

### Credential Flow

Credentials **never** stored in StorageClass parameters, PV attributes, or pod
env.

```text
CreateVolume: PVC annotation drive9.ai/secret-name → fetch K8s Secret → fixate ref in volumeAttributes
NodeStageVolume: volumeAttributes secretName/secretNamespace → fetch K8s Secret → set env vars → exec drive9 mount
DeleteVolume: look up PV by volumeHandle → read secretName/secretNamespace from volumeAttributes → fetch Secret
```

### Platform

**Linux only.** `mount_linux.go` implements FUSE mount observation. New mounts
always use `--supervise-foreground --direct-mount-strict --allow-other`;
Drive9's in-binary supervisor owns a replaceable FUSE worker, which calls
`mount(2)` and must not fall back to a FUSE helper. The Go binaries use
`CGO_ENABLED=0`. Root,
`SYS_ADMIN`, `/dev/fuse`, host `/proc`, host systemd, and writable host state and
runtime directories remain required; `fuse3`, `fusermount3`, and
`/etc/fuse.conf` do not. Mount processes run in host-managed transient systemd
services and survive CSI node Pod replacement.

### Unsupported Features

Rejected with explicit gRPC errors:

- Block volumes, fs_type, mount flags
- Reader-only and writer access modes outside the four supported modes above
- Volume content sources (cloning/snapshots)
- Volume expansion
- Dynamic VolumeAttributesClass updates after volume creation

## Deploy

```sh
make image IMAGE=ghcr.io/drive9-ai/drive9-csi:local
kubectl apply -k deploy/overlays/local  # local/preloaded validation image
```

`deploy/kubernetes` and `deploy/sidecar` are fail-closed bases containing
`registry.invalid/drive9-csi:unpublished`; do not apply either as a runnable
installation without an environment-specific image override. Production must
use a release-admitted immutable image selected by the release process.

Deploy resources:

| Path                          | Purpose                                                                                         |
| ----------------------------- | ----------------------------------------------------------------------------------------------- |
| `deploy/kubernetes/`          | Fail-closed CSI base: namespace, CSIDriver, controller, node, RBAC, two StorageClasses, VAC |
| `deploy/overlays/local/`      | Local image override for the CSI base                                                     |
| `deploy/sidecar/`             | Fail-closed fallback sidecar base (privileged, hostPath mount propagation)                |
| `deploy/examples/kubernetes/` | Separate SC, VAC, Secret, PVC and Pod examples plus a self-contained RWX bundle            |
| `deploy/examples/sidecar/`    | Example Secret for sidecar                                                               |

Key deploy details:

- `csi-provisioner:v6.3.0` sidecar with `--extra-create-metadata` (required for
  PVC name/namespace injection) and `VolumeAttributesClass=true`
- `node-driver-registrar:v2.13.0` sidecar
- Node DaemonSet needs `privileged: true`, `SYS_ADMIN`, `BIDIRECTIONAL` mount
  propagation
- Controller RBAC needs `secrets: [get]` (ClusterRole) for credential resolution
- Node RBAC also needs `secrets: [get]`
- StorageClasses `drive9-rwo` and `drive9-rwx` both use empty parameters,
  `Retain`, and `WaitForFirstConsumer`
- Optional default VAC `drive9-coding-agent` provides `profile=coding-agent`,
  `gvisorCompat=false`, 30-second TTLs, and `perfEnabled=false`; PVCs opt into
  it explicitly
- Examples are intentionally separate from kustomization so
  `kubectl apply -k deploy/kubernetes` doesn't create placeholder credentials
- The fallback sidecar uses a fail-closed image placeholder and Drive9's
  in-binary supervisor with a 30-second stop timeout

## Security Constraints

- `validateNoAPIKeyInAttributes` checks that `apiKey`/`server` never leak into
  volume attributes
- `validateSafeDeleteRoot` prevents deleting `/` or CSI metadata paths
- `DeleteVolume` requires both a matching index entry AND a matching marker file
- `NodeStageVolume` validates volume ID matches: workspace-root IDs must match
  both `volumeName` and `remoteRoot`; managed-directory IDs must match
  `remoteRoot`
- `NodePublishVolume` validates the staged mount belongs to the correct volume
  before bind-mounting
- Per-volume mutex (`volumeMu sync.Map`) serializes Node RPCs for the same
  volume

## State Files

Stored under `$DRIVE9_CSI_STATE_DIR` (default `/var/lib/drive9-csi`):

- `{volumeID}.json` — strict schema-v2 mount transaction state with
  `starting`/`active`/`stopping` phase, mount attempt, systemd unit,
  content-addressed binary, argv, process identity, and staging target. Pre-v2
  mount state is rejected and preserved for operator inspection.
- `published-{sha256(target)}.json` — publish state with required status field
  (`pending`/`published`/`unpublishing`); missing or unknown status fails
  closed. Legacy files may omit only `accessMode`, which defaults to
  `SINGLE_NODE_WRITER`.

## CI

`.github/workflows/publish-image.yml` is manually triggered. It resolves the
latest complete Drive9 CLI release, builds amd64 and arm64 validation images,
publishes a multi-architecture manifest to GHCR, and outputs the tag and digest.
It does not run tests, run E2E, deploy to a cluster, or grant release admission.

## Notes & Planning Files

- `notes.md` — Drive9 API assumptions
- `task_plan.md` — original implementation plan and safety invariants
- `TODO.md` — completed task history (production review passes, E2E tests)
- `dat9-dev-csi-smoke-test-20260607.md` — internal test report (may not be
  relevant to all agents)
- `docs/design/` — dated design records; use each file's frontmatter `status`
  and supersession note before treating it as current
