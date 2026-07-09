---
title: CSI VolumeAttributesClass Mount Parameters Design
---

## Goal

Drive9 CSI should move per-volume `drive9 mount` behavior parameters from `StorageClass.parameters` into Kubernetes `VolumeAttributesClass` parameters.

This lets users choose mount behavior per PVC without creating many nearly identical StorageClasses. The StorageClass should continue to describe the provisioning class. VolumeAttributesClass should describe mutable volume attributes and mount behavior profiles.

First implementation scope:

1. Support VolumeAttributesClass parameters at volume creation time.
2. Keep existing StorageClass parameters as a compatibility fallback.
3. Store the effective mount behavior in PV `VolumeContext`, as the driver already does today.
4. Do not dynamically update already-mounted volumes when a PVC changes `spec.volumeAttributesClassName`.
5. Upgrade the `csi-provisioner` sidecar to `registry.k8s.io/sig-storage/csi-provisioner:v6.3.0`, which supports GA `storage.k8s.io/v1` VolumeAttributesClass.

## Current Behavior

Current CSI manifest puts mount behavior directly in the default StorageClass:

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
  perfEnabled: "false"
```

The current driver rejects CSI `CreateVolumeRequest.MutableParameters`:

```text
internal/driver/driver.go:230
if len(req.GetMutableParameters()) > 0 {
	return nil, status.Error(codes.InvalidArgument, "mutable parameters are not supported")
}
```

It then parses mount behavior only from `CreateVolumeRequest.Parameters`:

```text
internal/driver/driver.go:254
ttls, err := effectiveMountTTLs(params)
```

`CreateVolume` persists the effective values into `VolumeContext`, and `NodeStageVolume` reads from `VolumeContext` before invoking `drive9 mount`.

```text
StorageClass parameters
  -> CreateVolumeRequest.Parameters
  -> CreateVolumeResponse.Volume.VolumeContext
  -> PV spec.csi.volumeAttributes
  -> NodeStageVolumeRequest.VolumeContext
  -> drive9 mount flags
```

This works, but StorageClass is cluster-level policy. It is the wrong place for parameters that need to vary per PVC.

## Target User Contract

Users define reusable mount behavior classes:

```yaml
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: drive9-coding-agent
driverName: csi.drive9.ai
parameters:
  profile: coding-agent
  attrTTL: 30s
  entryTTL: 30s
  dirTTL: 30s
  perfEnabled: "false"
```

Then a PVC selects both the provisioning class and the mount behavior class:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: drive9-workspace
  annotations:
    drive9.ai/secret-name: drive9-workspace-secret
spec:
  storageClassName: drive9-rwo
  volumeAttributesClassName: drive9-coding-agent
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
```

The StorageClass should no longer carry default mount behavior in new manifests:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: drive9-rwo
provisioner: csi.drive9.ai
parameters: {}
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: false
```

`remoteRootPrefix` may remain a StorageClass parameter in example StorageClasses for managed-directory provisioning, because it changes volume identity and path allocation, not just mount behavior.

## Parameter Ownership

| Parameter | New owner | Reason |
| --- | --- | --- |
| `profile` | VolumeAttributesClass | Mount behavior profile. |
| `attrTTL` | VolumeAttributesClass | Drive9 FUSE metadata TTL. |
| `entryTTL` | VolumeAttributesClass | Drive9 FUSE metadata TTL. |
| `dirTTL` | VolumeAttributesClass | Drive9 FUSE metadata TTL. |
| `perfEnabled` | VolumeAttributesClass | Per-volume diagnostic mount behavior. |
| `readdirPrefetch` | VolumeAttributesClass | Per-volume mount tuning. |
| `readdirPrefetchMaxFiles` | VolumeAttributesClass | Per-volume mount tuning. |
| `readdirPrefetchMaxFileBytes` | VolumeAttributesClass | Per-volume mount tuning. |
| `readdirPrefetchMaxBytes` | VolumeAttributesClass | Per-volume mount tuning. |
| `writebackBatchWindow` | VolumeAttributesClass | Per-volume mount tuning. |
| `remoteRootPrefix` | StorageClass | Changes managed volume path allocation. |
| `remoteRoot` | StorageClass or PVC annotation | Changes mounted Drive9 path. Existing PVC annotation remains supported. |
| `drive9.ai/secret-name` | PVC annotation | Per-PVC credential binding. |

Credentials must never move to VolumeAttributesClass. VAC is not secret storage.

`remoteRootPrefix` and `remoteRoot` are intentionally excluded from VolumeAttributesClass because they affect volume identity, not just mount behavior.

`remoteRootPrefix` controls managed-directory allocation. The driver turns it into a concrete Drive9 path and volume ID during `CreateVolume`. Changing it after creation would imply a different remote directory and a different CSI volume identity.

`remoteRoot` controls which Drive9 path is mounted for workspace-root volumes. It is validated against the volume ID for workspace-root safety. Treating it as a mutable VAC parameter would make a PVC-level performance profile capable of redirecting the mounted data path, which is surprising and unsafe.

VAC should only contain knobs that can be interpreted as "how to mount this volume", not "which Drive9 path this volume is".

## Data Flow

For new PVCs with `spec.volumeAttributesClassName`, external-provisioner reads the VolumeAttributesClass and sends its `parameters` as CSI `CreateVolumeRequest.mutable_parameters`.

```text
PVC spec.volumeAttributesClassName
  -> external-provisioner reads VolumeAttributesClass
  -> CreateVolumeRequest.MutableParameters
  -> driver validates supported mount behavior keys
  -> driver merges with StorageClass Parameters
  -> CreateVolumeResponse.Volume.VolumeContext
  -> NodeStageVolumeRequest.VolumeContext
  -> drive9 mount flags
```

Merge rule:

```text
effectiveCreateParameters = StorageClass parameters + MutableParameters override
```

CSI spec requires `mutable_parameters` to take precedence over `parameters`. The driver should follow that rule even when both maps contain the same key.

## Validation

The driver should only accept mount behavior keys in `MutableParameters`:

1. `profile`
2. `attrTTL`
3. `entryTTL`
4. `dirTTL`
5. `perfEnabled`
6. `readdirPrefetch`
7. `readdirPrefetchMaxFiles`
8. `readdirPrefetchMaxFileBytes`
9. `readdirPrefetchMaxBytes`
10. `writebackBatchWindow`

The driver should reject unsupported mutable keys with `InvalidArgument`. In particular, it should reject these keys in `MutableParameters`:

1. `remoteRootPrefix`
2. `remoteRoot`
3. `apiKey`
4. `server`
5. `csi.storage.k8s.io/*`
6. `secretName`
7. `secretNamespace`

Parsing and normalization should reuse the current helpers:

1. `effectiveMountTTLs`
2. `effectiveMountPerf`
3. `effectiveMountTuning`

`profile` should be trimmed before storing in `VolumeContext`.

## Controller Capabilities

external-provisioner requires the CSI driver to report `ControllerServiceCapability_RPC_MODIFY_VOLUME` before it will provision a PVC that references a VolumeAttributesClass.

Therefore the driver must advertise `MODIFY_VOLUME` when this feature is enabled. Without that capability, PVCs with `volumeAttributesClassName` fail before `CreateVolume` reaches the driver.

The initial implementation should advertise `MODIFY_VOLUME` so `CreateVolume` with VAC can work.

It should also implement `ControllerModifyVolume`, but dynamic modification is out of scope for this phase. If the RPC is called, it must not return no-op success. It should return a clear error that dynamic VAC updates are not supported and the volume must be recreated or restaged through a future supported workflow.

This avoids misleading users into thinking an already-mounted FUSE process has picked up new mount flags.

## Dynamic Modification Boundary

Changing `PVC.spec.volumeAttributesClassName` after a PV already exists is not supported in the first implementation.

Reason:

1. `NodeStageVolume` receives `VolumeContext` from the PV.
2. The PV `VolumeContext` was created by the original `CreateVolume` response.
3. `drive9 mount` parameters are process startup flags.
4. An already-running FUSE mount cannot safely change those flags without unmounting and starting a new `drive9 mount` process.

First implementation behavior:

1. New PVCs with VAC use VAC values at creation time.
2. Existing PVCs retain their existing PV `VolumeContext`.
3. The deployment does not include `external-resizer`.
4. If `ControllerModifyVolume` is called anyway, the driver returns a clear unsupported error instead of no-op success.

Future dynamic behavior needs a separate design. It should define where the effective mutable parameters are persisted, how node plugins observe changes, and how remounts are coordinated without breaking active workloads.

## Manifest Changes

Default CSI deploy should add a VolumeAttributesClass:

```yaml
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: drive9-coding-agent
driverName: csi.drive9.ai
parameters:
  profile: coding-agent
  attrTTL: 30s
  entryTTL: 30s
  dirTTL: 30s
  perfEnabled: "false"
```

Default StorageClass should remove mount behavior parameters:

```yaml
parameters: {}
```

PVC examples should include:

```yaml
spec:
  volumeAttributesClassName: drive9-coding-agent
```

Controller RBAC should include read access for VolumeAttributesClass if the external-provisioner sidecar requires it:

```yaml
- apiGroups: ["storage.k8s.io"]
  resources: ["volumeattributesclasses"]
  verbs: ["get", "list", "watch"]
```

The external-provisioner sidecar should explicitly enable the feature gate if required by the shipped image:

```yaml
- --feature-gates=VolumeAttributesClass=true
```

The target implementation should upgrade the sidecar from `registry.k8s.io/sig-storage/csi-provisioner:v5.2.0` to `registry.k8s.io/sig-storage/csi-provisioner:v6.3.0`, which supports GA `storage.k8s.io/v1` VolumeAttributesClass.

The first implementation should not deploy `external-resizer`. That sidecar is only needed for dynamic updates to existing PVCs, which are deferred to a separate design.

## Compatibility

Existing StorageClasses keep working. If `MutableParameters` is empty, the driver reads mount behavior from `Parameters` exactly as it does today.

Existing PVs keep working because `NodeStageVolume` already reads persisted values from `VolumeContext` and applies defaults for missing keys.

The compatibility period should be documented:

1. New examples use VolumeAttributesClass.
2. Existing StorageClass parameters remain supported.
3. A later cleanup can deprecate mount behavior in StorageClass after users have a migration window.

## Implementation Points

1. Replace the blanket `MutableParameters` rejection with validation of supported mutable keys.
2. Add a helper that merges `Parameters` and `MutableParameters` with mutable values taking precedence.
3. Use merged values for `profile`, TTL, perf, and tuning parsing.
4. Continue using original `Parameters` for volume identity values such as `remoteRootPrefix`.
5. Advertise `MODIFY_VOLUME` in `ControllerGetCapabilities`.
6. Implement `ControllerModifyVolume` with a clear unsupported error for dynamic VAC updates.
7. Add a default `VolumeAttributesClass` manifest and include it in the kustomization.
8. Upgrade `csi-provisioner` to `registry.k8s.io/sig-storage/csi-provisioner:v6.3.0`.
9. Update default StorageClass and PVC examples.
10. Update manifest tests for the new resource and parameter placement.

## Tests

Add focused tests for:

1. `CreateVolume` accepts `MutableParameters` with mount behavior keys.
2. `MutableParameters` override conflicting `Parameters`.
3. `CreateVolume` rejects unsupported mutable keys.
4. `CreateVolume` still accepts existing StorageClass-only mount behavior.
5. `CreateVolume` uses original `Parameters` for `remoteRootPrefix`.
6. `ControllerGetCapabilities` includes `MODIFY_VOLUME`.
7. `ControllerModifyVolume` returns a clear unsupported error rather than no-op success.
8. `ControllerModifyVolume` rejects unsupported mutable keys.
9. Deploy manifests include a default VolumeAttributesClass.
10. Default StorageClass no longer carries mount behavior parameters.
11. PVC examples reference the default VolumeAttributesClass.
12. Controller manifest uses a csi-provisioner image version compatible with `storage.k8s.io/v1` VAC.

## References

1. Kubernetes VolumeAttributesClass: <https://kubernetes.io/docs/concepts/storage/volume-attributes-classes/>
2. CSI external-provisioner VolumeAttributesClass behavior: <https://github.com/kubernetes-csi/external-provisioner>
3. Local CSI spec: `github.com/container-storage-interface/spec v1.11.0`
