---
title: CSI Perf Support Bundle Upload Design
---

## Goal

Drive9 CSI should provide a safe support workflow for collecting `drive9 mount --perf-dir` output and uploading it to a Drive9-owned support space.

The upload should use the `drive9` CLI already present in the CSI node image, but it must not use the user's workload Drive9 credentials or upload into the user's business space.

## Current Context

The node plugin image already contains `/usr/local/bin/drive9` because `NodeStageVolume` execs it for FUSE mounts.

The CLI has an upload primitive through `drive9 fs cp`:

```text
drive9 fs cp [-r|--recursive] [--resume] [--append] [--tag key=value]... [--description <text>] <src> <dst>
```

This makes it possible to upload a local support bundle from inside the node plugin container without adding a new uploader binary.

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

The user runs an explicit upload command inside the CSI node plugin container:

```sh
DRIVE9_SERVER=https://api.drive9.ai \
DRIVE9_API_KEY=<support-upload-token> \
drive9 fs cp /var/lib/drive9-csi/perf/<case-id>.tgz \
  :/support-inbox/<case-id>/<node-name>/<volume-id>.tgz \
  --tag case=<case-id> \
  --tag source=k8s-csi \
  --description "Drive9 CSI perf bundle"
```

## Data Flow

```text
drive9 mount --perf-dir
  -> writes perf files under <stateDir>/perf/<safe-volume-id>
  -> user creates a local bundle
  -> user injects support upload token only for the upload command
  -> drive9 fs cp uploads to Drive9 support space
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

The token should be passed only at upload time, either as temporary environment variables or as a short-lived Kubernetes Secret used by a support runbook.

## Token Scope

The preferred token type is a scoped Drive9 filesystem token limited to a single support prefix:

```text
:/support-inbox/<case-id>/<cluster-id>/
```

The scope should be as narrow as the CLI permits:

1. Short expiration.
2. Write access to the case prefix.
3. No delete access.
4. No read or list access unless `drive9 fs cp` requires it for the selected upload mode.

If resumable uploads require read/list/stat permissions, the support runbook should either disable resume or document the additional scope requirement.

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

## Support Runbook Shape

A future helper script can wrap the manual steps:

```sh
hack/collect-perf-bundle.sh \
  --namespace drive9-csi \
  --node <node-name> \
  --volume-id <volume-id> \
  --case-id <case-id> \
  --upload-prefix :/support-inbox/<case-id>/<cluster-id>/ \
  --server https://api.drive9.ai
```

The script should:

1. Locate the `drive9-csi-node` pod on the requested node.
2. Create a tarball from the perf directory.
3. Run `drive9 fs cp` inside the container.
4. Attach useful tags such as case ID, CSI version, node name, volume ID, and timestamp.
5. Print the uploaded path.

The script should not persist the support upload token. It should read the token from an environment variable or prompt the operator to provide it.

## Security And Privacy

Perf bundles may contain sensitive operational metadata such as paths, timing data, mount behavior, and possibly request identifiers.

The upload action must remain explicit and user-approved:

1. No default auto-upload.
2. No baked-in support token.
3. No reuse of user workload credentials.
4. No upload to user business space.
5. No silent background retry after the user command exits.

Documentation should tell users what path is being bundled and where it will be uploaded.

## Open Questions

1. What exact Drive9 token API should support use to mint the scoped upload token?
2. Does `drive9 fs cp` require read/list/stat permission for non-resumable uploads?
3. What metadata fields should be mandatory tags on uploaded bundles?
4. What retention policy applies to support-space uploaded bundles?

These questions do not block implementation of `perfEnabled` and fixed perf paths. They only affect the support upload runbook and token issuance process.

## Tests

If a helper script is added, test coverage should verify:

1. It selects the node plugin pod for the requested node.
2. It refuses to run without an explicit support upload token.
3. It builds the expected tarball path.
4. It calls `drive9 fs cp` with the support destination prefix.
5. It does not write the token to files or command logs.

If only documentation is added, no code tests are required.

If `perfEnabled` is implemented in CSI, add focused tests for:

1. `CreateVolume` defaults `perfEnabled` to `false`.
2. `CreateVolume` stores `perfEnabled=true` when requested.
3. Invalid `perfEnabled` values fail with `InvalidArgument`.
4. `NodeStageVolume` defaults missing `perfEnabled` to `false` for legacy PVs.
5. `drive9MountArgs` includes `--perf-dir <stateDir>/perf/<safe-volume-id>` only when enabled.
6. Sidecar manifest includes `DRIVE9_PERF_ENABLED=false` and fixed `/perf` behavior.
