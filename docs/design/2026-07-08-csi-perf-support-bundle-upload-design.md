---
title: CSI Perf Support Bundle Upload Design
status: implemented
---

## Status

Implemented by `9ff7d41` and `07e19f3`. VolumeAttributesClass is now the
preferred source for `perfEnabled`; legacy StorageClass parameters remain a
compatibility fallback. The later mount-survival design supersedes descriptions
below that imply the node plugin directly owns the long-running mount process.

## Goal

Drive9 CSI should provide a safe support workflow for collecting `drive9 mount --perf-dir` output and uploading it to a Drive9-owned support space.

The upload should use the `drive9` CLI already present in the CSI node image, but it must not use the user's workload Drive9 credentials or upload into the user's business space.

## Design-time Context

At design time, the node plugin image already contained `/usr/local/bin/drive9`
and `NodeStageVolume` launched it for FUSE mounts. The current host-systemd
lifecycle installs and launches a content-addressed host copy instead.

The CLI has an upload primitive through `drive9 fs cp`:

```text
drive9 fs cp [-r|--recursive] [--resume] [--append] [--tag key=value]... [--description <text>] <src> <dst>
```

This makes it possible to upload a local support bundle from inside the node plugin container without adding a new uploader service.

## User Contract

CSI exposes a `StorageClass` parameter:

| StorageClass parameter | Meaning | Default |
| --- | --- | --- |
| `perfEnabled` | Enable `drive9 mount --perf-dir` for the volume | `false` |

When `perfEnabled` is omitted or set to `false`, CSI does not pass `--perf-dir`.

When `perfEnabled` is set to `true`, CSI passes a driver-generated perf directory:

```text
--perf-dir <stateDir>/perf/<safe-volume-id>
```

CSI does not accept a user-provided perf path. Users can enable or disable perf collection, but the driver owns the path shape.

Drive9 support provides the user with upload credentials:

| Field | Meaning |
| --- | --- |
| `DRIVE9_SERVER` | Drive9 API endpoint for the support space |
| `DRIVE9_API_KEY` | Short-lived support upload token |
| Upload prefix | Case-specific destination, for example `:/support-inbox/<case-id>/<cluster-id>/` |

The token should resolve to a Drive9-owned support space. The destination path is relative to that support context, not the user's business workspace.

The preferred user-facing flow is a helper command inside the CSI node plugin container:

```sh
drive9-csi-upload-perf --case-id <case-id>
```

The helper should prompt for the support upload token without echoing it. It should discover the node identity and the perf volume directory automatically when possible.

## Data Flow

```text
drive9 mount --perf-dir
  -> writes perf files under <stateDir>/perf/<safe-volume-id>
  -> user runs drive9-csi-upload-perf
  -> helper creates a local bundle
  -> helper reads the support upload token only for the upload command
  -> helper runs drive9 fs cp to upload to Drive9 support space
  -> helper runs drive9 fs stat to verify the uploaded bundle
```

The recommended local layout is:

```text
/var/lib/drive9-csi/perf/<volume-id>/
/var/lib/drive9-csi/perf/<case-id>.tgz
```

The bundle command can be:

```sh
tar czf /var/lib/drive9-csi/perf/<case-id>.tgz \
  -C /var/lib/drive9-csi/perf <volume-id>
```

This command is an implementation detail of the helper and a fallback for manual support debugging. It should not be the primary customer workflow.

## Credential Model

The support upload token is separate from the volume mount credential.

| Credential | Owner | Use |
| --- | --- | --- |
| Workload Drive9 API key | User | Mount user's Drive9 business space |
| Support upload token | Drive9 support | Upload bundle to Drive9 support space |

CSI must not store the support upload token in:

1. `StorageClass` parameters.
2. PV `VolumeContext`.
3. PVC annotations.
4. Driver state files.
5. Default deployment manifests.

The token should be passed only at upload time. The preferred customer workflow is an interactive hidden prompt. If the helper is not attached to a TTY, it must not attempt an interactive prompt; users must pass `--token-stdin`.

A non-interactive mode may read the token from stdin:

```sh
printf '%s' '<support-upload-token>' | drive9-csi-upload-perf --case-id <case-id> --token-stdin
```

The helper must not require users to pass the token as a command-line argument because arguments can be exposed through shell history and process listings.

## Token Scope

The preferred token type is a scoped Drive9 filesystem token limited to a single support prefix:

```text
:/support-inbox/<case-id>/<cluster-id>/
```

The support upload token must allow:

1. Short expiration.
2. Write/create access under the case prefix.
3. `stat` access for the uploaded object so the helper can verify the upload.
4. No delete access.
5. No broad read or list access unless the `drive9 fs cp` implementation requires it.

The scope should be as narrow as the CLI permits. If `drive9 fs cp` requires additional read/list permissions, support token issuance should grant only the minimum required permission under the case prefix.

The helper must treat failed `drive9 fs stat` as upload verification failure and return a non-zero exit status.

## Input Validation

The helper must validate user-controlled path components before using them in local or remote paths.

`--case-id` rules:

1. Required.
2. Must be non-empty after trimming whitespace.
3. Maximum length: 128 bytes.
4. Allowed characters: ASCII letters, ASCII digits, `.`, `_`, and `-`.
5. Must not contain `/`, `\`, `..`, shell metacharacters, or whitespace.

`--volume-id` rules:

1. Optional when exactly one perf volume directory exists.
2. Required when multiple perf volume directories exist.
3. Must be a basename, not a path.
4. Must be non-empty after trimming whitespace.
5. Must not contain `/`, `\`, `..`, shell metacharacters, or whitespace.
6. Must refer to an existing directory directly under `/var/lib/drive9-csi/perf/`.

The helper should build paths by joining validated components, not by accepting arbitrary user-provided paths.

Perf directory discovery should only consider immediate child directories under `/var/lib/drive9-csi/perf/`. It should ignore files such as existing `*.tgz` bundles.

When multiple perf volume directories exist, the helper should print the candidate basenames and exit with instructions to rerun with `--volume-id`. It should not guess based on modification time or current mount processes.

## CSI Behavior

First implementation should not auto-upload perf data.

CSI should only make perf output predictable and easy to collect:

1. Parse `perfEnabled` from `StorageClass` parameters in `CreateVolume`.
2. Store the effective value in PV `VolumeContext`.
3. Default missing values to `false` in `NodeStageVolume` for legacy PVs.
4. Use a deterministic perf directory under the CSI state directory.
5. Include a collision-safe volume identifier in the path.
6. Keep upload credentials outside CSI configuration.
7. Provide documentation or a helper script for bundle creation and upload.

The generated CSI perf path is:

```text
<stateDir>/perf/<safe-volume-id>
```

`NodeStageVolume` should pass:

```text
--perf-dir <stateDir>/perf/<safe-volume-id>
```

only when `perfEnabled` is `true`.

Automatic upload can be considered later only if there is a clear support requirement and a separate design covers privacy, retries, cleanup, and secret lifecycle.

## Sidecar Fallback

The sidecar fallback should expose the same high-level behavior with an environment variable:

| Environment variable | Meaning | Default |
| --- | --- | --- |
| `DRIVE9_PERF_ENABLED` | Enable `drive9 mount --perf-dir` in the sidecar mounter | `false` |

When enabled, the sidecar uses a fixed container path:

```text
--perf-dir /perf
```

The sidecar should not expose `DRIVE9_PERF_DIR` or any other arbitrary path override.

Users can control the backing storage through Kubernetes volume mounts:

```yaml
volumeMounts:
  - name: drive9-perf
    mountPath: /perf

volumes:
  - name: drive9-perf
    hostPath:
      path: /var/lib/drive9-sidecar/demo/perf
      type: DirectoryOrCreate
```

This keeps the `drive9 mount` flag contract consistent while still letting users choose whether `/perf` is backed by `hostPath`, `emptyDir`, or another Kubernetes volume type.

The sidecar shell should create `/perf` before mounting when perf is enabled:

```sh
if [ "${DRIVE9_PERF_ENABLED:-false}" = "true" ]; then
  mkdir -p /perf || exit 1
  perf_args="--perf-dir /perf"
fi
```

The default sidecar manifest should set:

```yaml
- name: DRIVE9_PERF_ENABLED
  value: "false"
```

## Cleanup

CSI should not automatically delete perf directories.

Perf output is diagnostic data. It should survive `NodeUnstageVolume` so users can collect and upload it after reproducing an issue.

Cleanup is explicit and manual:

```sh
rm -rf /var/lib/drive9-csi/perf/<volume-id>
rm -f /var/lib/drive9-csi/perf/<case-id>.tgz
```

For sidecar fallback, users clean the volume mounted at `/perf`.

## One-Command Upload Helper

The node plugin image should include a helper command:

```sh
drive9-csi-upload-perf --case-id <case-id>
```

The helper runs inside the `drive9-csi` container. It should use the existing `drive9` CLI and `tar`; it should not add a new long-running process or controller.

The first implementation should be a shell script copied into the runtime image as:

```text
/usr/local/bin/drive9-csi-upload-perf
```

The repository source path can be:

```text
hack/drive9-csi-upload-perf.sh
```

The Dockerfile should copy it into the runtime image and make it executable. The script should be POSIX `sh` compatible unless a specific bash-only need appears during implementation.

Required behavior:

1. Require `--case-id`.
2. Discover perf directories under `/var/lib/drive9-csi/perf/`.
3. If exactly one volume directory exists, select it automatically.
4. If multiple volume directories exist, print the candidates and require `--volume-id`.
5. Create `/var/lib/drive9-csi/perf/<case-id>.tgz` from the selected volume directory.
6. Prompt for the support upload token without echoing input, or read it from stdin when `--token-stdin` is set.
7. Upload to the support space with `drive9 fs cp`.
8. Verify the uploaded bundle with `drive9 fs stat`.
9. Print the uploaded path and local bundle path.

Optional flags:

| Flag | Meaning | Default |
| --- | --- | --- |
| `--case-id` | Support case identifier | Required |
| `--volume-id` | Perf volume ID to upload when multiple directories exist | Auto-select when exactly one exists |
| `--server` | Drive9 support API endpoint | `https://api.drive9.ai` |
| `--token-stdin` | Read support upload token from stdin | `false` |
| `--keep-bundle` | Keep local tarball after successful upload | `true` |

The destination path should be:

```text
:/support-inbox/<case-id>/<node-name>/<volume-id>.tgz
```

The helper should attach these tags:

1. `case=<case-id>`
2. `source=k8s-csi`
3. `node=<node-name>`
4. `volume=<volume-id>`

The helper should not persist the support upload token. It must not write the token into files, logs, shell history, or generated bundle metadata.

## Node Identity

The upload path needs a stable node identifier. The helper should not require customers to type it.

The DaemonSet should inject node and pod identity into the `drive9-csi` container through the Kubernetes Downward API:

```yaml
env:
  - name: DRIVE9_CSI_NODE_NAME
    valueFrom:
      fieldRef:
        fieldPath: spec.nodeName
  - name: DRIVE9_CSI_POD_NAME
    valueFrom:
      fieldRef:
        fieldPath: metadata.name
```

The helper should resolve node identity in this order:

1. `DRIVE9_CSI_NODE_NAME`
2. `NODE_NAME`
3. `hostname`
4. `unknown-node`

`DRIVE9_CSI_POD_NAME` is useful for logs and diagnostics but should not be the primary destination path component because support usually groups uploads by Kubernetes node.

## Manual Fallback

If the helper is unavailable, support can still guide a user through the manual flow:

```sh
tar czf /var/lib/drive9-csi/perf/<case-id>.tgz \
  -C /var/lib/drive9-csi/perf <volume-id>
```

```sh
DRIVE9_SERVER=https://api.drive9.ai \
DRIVE9_API_KEY=<support-upload-token> \
drive9 fs cp /var/lib/drive9-csi/perf/<case-id>.tgz \
  :/support-inbox/<case-id>/<node-name>/<volume-id>.tgz \
  --tag case=<case-id> \
  --tag source=k8s-csi \
  --description "Drive9 CSI perf bundle"
```

```sh
DRIVE9_SERVER=https://api.drive9.ai \
DRIVE9_API_KEY=<support-upload-token> \
drive9 fs stat :/support-inbox/<case-id>/<node-name>/<volume-id>.tgz
```

The manual fallback is intentionally more verbose and should not be the documented happy path for customers.

## Security And Privacy

Perf bundles may contain sensitive operational metadata such as paths, timing data, mount behavior, and possibly request identifiers.

The upload action must remain explicit and user-approved:

1. No default auto-upload.
2. No baked-in support token.
3. No reuse of user workload credentials.
4. No upload to user business space.
5. No silent background retry after the user command exits.

Documentation should tell users what path is being bundled and where it will be uploaded.

The README should document the helper as the customer happy path and keep the manual `tar` plus `drive9 fs cp` sequence as a troubleshooting fallback.

## Open Questions

1. What exact Drive9 token API should support use to mint the scoped upload token?
2. Does `drive9 fs cp` require read/list permission beyond write and object `stat` for non-resumable uploads?
3. What retention policy applies to support-space uploaded bundles?

These questions do not block implementation of `perfEnabled`, fixed perf paths, or the one-command helper. They only affect support token issuance policy and support-space retention policy.

## Tests

If a helper script is added, test coverage should verify:

1. It refuses to run without `--case-id`.
2. It auto-selects the only perf volume directory.
3. It requires `--volume-id` when multiple perf volume directories exist.
4. It refuses to run without an explicit support upload token.
5. It builds the expected tarball path.
6. It calls `drive9 fs cp` with the support destination path.
7. It calls `drive9 fs stat` after upload.
8. It resolves node identity from `DRIVE9_CSI_NODE_NAME`.
9. It does not write the token to files or command logs.

If only documentation is added, no code tests are required.

If `perfEnabled` is implemented in CSI, add focused tests for:

1. `CreateVolume` defaults `perfEnabled` to `false`.
2. `CreateVolume` stores `perfEnabled=true` when requested.
3. Invalid `perfEnabled` values fail with `InvalidArgument`.
4. `NodeStageVolume` defaults missing `perfEnabled` to `false` for legacy PVs.
5. `drive9MountArgs` includes `--perf-dir <stateDir>/perf/<safe-volume-id>` only when enabled.
6. Sidecar manifest includes `DRIVE9_PERF_ENABLED=false` and fixed `/perf` behavior.
