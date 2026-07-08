---
name: k8s-csi
description: |
  Drive9 CSI Lite driver — minimal K8s CSI integration for drive9 FUSE mounts.
  Single Go module (go 1.26), single binary, no codegen.
---

# Drive9 CSI Lite — Agent Reference

## Module & Package

- **Module**: `github.com/drive9-ai/csi` (single `go.mod`, no workspace)
- **Binary**: `cmd/drive9-csi/main.go` → `bin/drive9-csi`
- **Production code**: all in `internal/driver/` (single `package driver`; 10 files, no sub-packages)
- **Entrypoint** creates an in-cluster K8s client from `rest.InClusterConfig()`, then calls `driver.Run()`.
- **No codegen, no protobuf**, no `//go:generate`, no mockgen, no controller-gen. CSI spec is consumed as Go module (`container-storage-interface/spec v1.11.0`).

## Build

```sh
make test     # gofmt -w cmd internal && go test ./...
make build    # CGO_ENABLED=0, default linux/amd64 → bin/drive9-csi
make image IMAGE=ghcr.io/drive9-ai/drive9-csi:local  # single-arch Docker build
make image-multi IMAGE=ghcr.io/drive9-ai/drive9-csi:<tag>  # multi-arch buildx + push
```

Override platform: `GOOS`, `GOARCH` env vars. Default `GOPROXY=http://proxy.golang.org,direct`.

## Dockerfile

Multi-stage (`Dockerfile`, 56 lines):
1. `golang:1.26-bookworm` — compiles `drive9-csi` (CGO_ENABLED=0)
2. `debian:bookworm-slim` — downloads `drive9` CLI from `https://drive9.ai/releases/`
3. `debian:bookworm-slim` — runtime: `fuse3`, `tini`, `ca-certificates`; entrypoint via `tini`

Both build stages validate target arch via ELF machine byte check. The runtime image must contain `/usr/local/bin/drive9` — the node plugin execs it for FUSE mounts.

## Docker Image Tags

**No `:latest` tag.** Image tags follow the pattern: `drive9-<drive9-cli-version>-csi-<sha7>`. Example: `drive9-aff1023-csi-ef5fab2`. The CI publishes only these traceable tags. Use an immutable tag or digest for production.

## Tests

```sh
make test           # gofmt + go test ./...
go test ./...       # run without formatting
go test -run TestCreateVolume ./internal/driver/   # run a single test function
```

Test file locations (4 files, all in `internal/driver/`):

| File | Covers |
|---|---|
| `driver_test.go` | Create/Delete/Stage/Publish/Unstage/Unpublish, path validation, capability checks |
| `k8s_secret_test.go` | Secret resolution from PVC annotations |
| `kubernetes_manifest_test.go` | Deploy YAML vs driver parameter consistency (reads YAMLs from `../../deploy/`) |
| `mount_linux_test.go` | `stopRecordedMount`, `waitForMount` (build tag: `linux`) |

Tests use:
- `testing` stdlib — no test framework/suite, no `TestMain`
- Tests are in-package (`package driver`, not `package driver_test`) — they access unexported symbols directly
- `fakeDrive9` — `httptest.Server`-based HTTP handler simulating the Drive9 API (defined in `driver_test.go:1324`, supports dirs, files, marker JSON, and transient error injection via `failDeleteOnce`)
- `k8sfake.NewSimpleClientset` — fake K8s API for Secret/PVC/PV operations
- `createPVForVolume` helper — simulates what `csi-provisioner` does after `CreateVolume`
- Error code assertions use `status.Code(err) != codes.InvalidArgument` — never raw `err == nil` checks for gRPC calls
- `t.Fatalf`/`t.Fatal` only (no `require`/`assert`)
- No `testdata/` directories, no test fixtures, no `.env` files

CI does **not** run tests — `.github/workflows/publish-image.yml` only builds and publishes images.

Test quirks:
- `kubernetes_manifest_test.go` reads deploy YAMLs via relative paths (`../../deploy/...`) — test working directory matters
- `mount_linux_test.go` has build tag `linux` — won't compile on macOS without explicit cross-compile

## E2E Test (Real K8s)

```sh
export DRIVE9_SERVER=https://api.drive9.ai
export DRIVE9_API_KEY=drive9_api_key_redacted
export DRIVE9_CSI_IMAGE=ghcr.io/drive9-ai/drive9-csi:drive9-aff1023-csi-ef5fab2
export DRIVE9_CSI_E2E_CONFIRM=1
hack/e2e-k8s.sh
```

`DRIVE9_CSI_E2E_CONFIRM=1` is mandatory — the script mutates the current K8s context. Requires a clean cluster (fails if `drive9-csi` resources already exist). Do not use `:latest` for the image.

## Architecture

The driver implements three CSI gRPC services in one binary:

| Service | Implemented operations |
|---|---|
| Identity | `GetPluginInfo`, `GetPluginCapabilities`, `Probe` |
| Controller | `CreateVolume`, `DeleteVolume`, `ControllerGetCapabilities`, `ValidateVolumeCapabilities` |
| Node | `NodeStageVolume`, `NodeUnstageVolume`, `NodePublishVolume`, `NodeUnpublishVolume`, `NodeGetInfo`, `NodeGetCapabilities` |

### Volume Modes

Two volume types, determined by StorageClass parameters:

**Workspace-root** (default, no `remoteRootPrefix`):
- Mounts a Drive9 workspace root selected by API key
- `CreateVolume` checks the path exists but writes no CSI metadata
- `DeleteVolume` is a no-op for Drive9 data
- Volume ID prefix: `drive9-root-`

**Managed directory** (`remoteRootPrefix` parameter present):
- Creates a unique subdirectory under the prefix (e.g., `/k8s/pvc/<name>-<sha12>`)
- Writes CSI metadata: marker file (`.drive9-csi-volume.json`), index at `/k8s/.drive9-csi/volumes/`, name-index at `/k8s/.drive9-csi/volumes/by-name/`
- `DeleteVolume` removes only CSI metadata — never deletes user data

### Credential Flow

Credentials **never** stored in StorageClass parameters, PV attributes, or pod env.
```
CreateVolume: PVC annotation drive9.ai/secret-name → fetch K8s Secret → fixate ref in volumeAttributes
NodeStageVolume: volumeAttributes secretName/secretNamespace → fetch K8s Secret → set env vars → exec drive9 mount
DeleteVolume: look up PV by volumeHandle → read secretName/secretNamespace from volumeAttributes → fetch Secret
```

### Platform

**Linux only.** `mount_linux.go` (build tag `linux`) implements FUSE mount orchestration. `mount_unsupported.go` (build tag `!linux`) returns errors. `CGO_ENABLED=0` — no C dependencies in the Go binary. The runtime image needs `fuse3` apt package and `user_allow_other` in `/etc/fuse.conf`.

### Unsupported Features

Rejected with explicit gRPC errors:
- Block volumes, fs_type, mount flags
- RWX/MultiNode access modes
- Volume content sources (cloning/snapshots)
- Volume expansion
- Mutable parameters

## Deploy

```sh
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl apply -k deploy/kubernetes      # install CSI driver
kubectl apply -k deploy/sidecar         # sidecar fallback (non-CSI)
```

Deploy resources:

| Path | Purpose |
|---|---|
| `deploy/kubernetes/` | Production CSI: namespace, CSIDriver, controller Deployment, node DaemonSet, RBAC, StorageClass |
| `deploy/sidecar/` | Fallback sidecar Deployment (privileged, hostPath mount propagation) |
| `deploy/examples/kubernetes/` | Example Secret, PVC, Pod (apply separately, not via kustomize) |
| `deploy/examples/sidecar/` | Example Secret for sidecar |

Key deploy details:
- `csi-provisioner:v5.2.0` sidecar with `--extra-create-metadata` (required for PVC name/namespace injection)
- `node-driver-registrar:v2.13.0` sidecar
- Node DaemonSet needs `privileged: true`, `SYS_ADMIN`, `BIDIRECTIONAL` mount propagation
- Controller RBAC needs `secrets: [get]` (ClusterRole) for credential resolution
- Node RBAC also needs `secrets: [get]`
- Default StorageClass: `drive9-rwo`, `Retain`, `WaitForFirstConsumer`, `profile=coding-agent`
- Examples are intentionally separate from kustomization so `kubectl apply -k deploy/kubernetes` doesn't create placeholder credentials

## Security Constraints

- `validateNoAPIKeyInAttributes` checks that `apiKey`/`server` never leak into volume attributes
- `validateSafeDeleteRoot` prevents deleting `/` or CSI metadata paths
- `DeleteVolume` requires both a matching index entry AND a matching marker file
- `NodeStageVolume` validates volume ID matches: workspace-root IDs must match both `volumeName` and `remoteRoot`; managed-directory IDs must match `remoteRoot`
- `NodePublishVolume` validates the staged mount belongs to the correct volume before bind-mounting
- Per-volume mutex (`volumeMu sync.Map`) serializes Node RPCs for the same volume

## State Files

Stored under `$DRIVE9_CSI_STATE_DIR` (default `/var/lib/drive9-csi`):
- `{volumeID}.json` — mount state (PID, PIDStartTime, staging target)
- `published-{sha256(target)}.json` — publish state with status field (`pending`/`published`), supports legacy state files without `status`/`accessMode` fields

## CI

`.github/workflows/publish-image.yml` triggers on push to `main` (paths: `Dockerfile`, `Makefile`, `cmd/`, `go.mod`, `go.sum`, `internal/`). Multi-arch build (amd64 + arm64) → GHCR. Requires manual step to make package public after first publish.

## Notes & Planning Files

- `notes.md` — Drive9 API assumptions
- `task_plan.md` — original implementation plan and safety invariants
- `TODO.md` — completed task history (production review passes, E2E tests)
- `dat9-dev-csi-smoke-test-20260607.md` — internal test report (may not be relevant to all agents)
