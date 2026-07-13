---
title: CSI Mount TTL Parameters Design
status: implemented
---

## Status

Implemented by `9ff7d41`. This document records the original StorageClass-based
contract. VolumeAttributesClass is now the preferred source for mount behavior;
legacy StorageClass parameters remain supported as a compatibility fallback.

## Goal

Drive9 CSI should let users tune the `drive9 mount --mode=fuse` metadata TTL flags through `StorageClass` parameters:

| StorageClass parameter | drive9 mount flag | Default |
| --- | --- | --- |
| `attrTTL` | `--attr-ttl` | `30s` |
| `entryTTL` | `--entry-ttl` | `30s` |
| `dirTTL` | `--dir-ttl` | `30s` |

If a user omits any value, CSI must still pass that flag to `drive9 mount` with `30s`. This makes CSI behavior stable and independent from `drive9` CLI or profile defaults.

## Original Behavior

The driver currently reads `profile` from `CreateVolumeRequest.Parameters`, stores it in `VolumeContext`, then `NodeStageVolume` passes it to `startDrive9Mount`.

```text
StorageClass parameters
  -> CreateVolume Parameters
  -> PV VolumeContext
  -> NodeStageVolume
  -> drive9 mount flags
```

Current `drive9 mount` flags are:

```text
drive9 mount \
  --mode=fuse \
  --allow-other \
  --cache-dir <state-dir>/cache/<volume-id> \
  [--profile <profile>] \
  [--local-root <state-dir>/local/<volume-id>] \
  :<remoteRoot> \
  <stagingTarget>
```

The original driver behavior did not support `attrTTL`, `entryTTL`, or `dirTTL`.

## User Contract

Users configure TTLs on the `StorageClass`:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: drive9-rwo
provisioner: csi.drive9.ai
parameters:
  profile: coding-agent
  attrTTL: 30s
  entryTTL: 30s
  dirTTL: 30s
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: false
```

All three parameters use Go duration syntax, for example `500ms`, `1s`, `30s`, or `2m`.

## Data Flow

`CreateVolume` should normalize and persist the effective TTLs into `VolumeContext`:

```text
CreateVolumeRequest.Parameters
  attrTTL, entryTTL, dirTTL
    -> parse as positive time.Duration
    -> default missing values to 30s
    -> store normalized strings in VolumeContext
```

`NodeStageVolume` should read the values from `VolumeContext`. If the values are missing, as with old PVs or hand-written PVs, it should apply the same `30s` default.

`startDrive9Mount` should always append:

```text
--attr-ttl <attrTTL>
--entry-ttl <entryTTL>
--dir-ttl <dirTTL>
```

## Validation

Validation should be strict:

| Input | Result |
| --- | --- |
| Missing | Use `30s` |
| Valid positive duration | Use normalized `time.Duration.String()` |
| Invalid duration | Return `InvalidArgument` |
| Zero or negative duration | Return `InvalidArgument` |

Rejecting zero and negative values avoids surprising fallback behavior in `drive9`, where non-positive TTLs can be treated as unset.

## Implementation Points

1. Add constants for parameter keys and default:

```go
const (
	paramAttrTTL  = "attrTTL"
	paramEntryTTL = "entryTTL"
	paramDirTTL   = "dirTTL"

	defaultMountTTL = 30 * time.Second
)
```

2. Add a helper that parses and normalizes TTL parameters:

```go
func effectiveMountTTLs(values map[string]string) (mountTTLs, error)
```

3. Call the helper from both `CreateVolume` and `NodeStageVolume`.

4. Extend `drive9MountRequest` with `AttrTTL`, `EntryTTL`, and `DirTTL`.

5. Append the three `drive9 mount` flags in `startDrive9Mount`.

6. Keep Kubernetes `mountOptions` and CSI `MountFlags` unsupported. These values are Drive9 CLI flags, not kernel mount options.

## Compatibility

Existing StorageClasses keep working. Existing PVs also keep working because `NodeStageVolume` applies defaults when TTL values are absent from `VolumeContext`.

The default example `StorageClass` should include the three parameters explicitly so users can discover and tune them.

## Sidecar Fallback

The sidecar fallback is not CSI and does not use `StorageClass`, PVC, or PV metadata. It should still expose the same effective behavior through environment variables:

| Environment variable | drive9 mount flag | Default |
| --- | --- | --- |
| `DRIVE9_ATTR_TTL` | `--attr-ttl` | `30s` |
| `DRIVE9_ENTRY_TTL` | `--entry-ttl` | `30s` |
| `DRIVE9_DIR_TTL` | `--dir-ttl` | `30s` |

The sidecar shell should default missing values to `30s`:

```sh
attr_ttl="${DRIVE9_ATTR_TTL:-30s}"
entry_ttl="${DRIVE9_ENTRY_TTL:-30s}"
dir_ttl="${DRIVE9_DIR_TTL:-30s}"
```

It should reject empty values:

```sh
if [ -z "${attr_ttl}" ] || [ -z "${entry_ttl}" ] || [ -z "${dir_ttl}" ]; then
  echo "DRIVE9_*_TTL must not be empty" >&2
  exit 1
fi
```

Then the sidecar command should pass:

```text
--attr-ttl <attr_ttl>
--entry-ttl <entry_ttl>
--dir-ttl <dir_ttl>
```

The manifest should include the three environment variables with `30s` values so users can discover and override them. Duration syntax validation can remain with `drive9 mount`; shell-side parsing of Go durations would add complexity without improving the contract.

## Tests

Add focused tests for:

1. `CreateVolume` stores default TTLs when omitted.
2. `CreateVolume` stores user-provided TTLs when present.
3. Invalid TTL values fail with `InvalidArgument`.
4. `NodeStageVolume` applies defaults for legacy `VolumeContext` without TTLs.
5. `startDrive9Mount` builds `drive9 mount` args with `--attr-ttl`, `--entry-ttl`, and `--dir-ttl`.
6. Manifest or README coverage shows the default `StorageClass` parameters.
7. Sidecar manifest coverage shows the three TTL env vars, default `30s` values, and matching `drive9 mount` flags.
