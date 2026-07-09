---
title: VAC Mount Parameters Implementation Plan
---

## Scope

Implement first-phase VolumeAttributesClass support for Drive9 CSI mount behavior parameters.

## Steps

1. Accept and validate CSI `MutableParameters` for mount behavior keys.
2. Merge mutable parameters over StorageClass parameters for mount behavior only.
3. Advertise `MODIFY_VOLUME` and return an explicit unsupported error from `ControllerModifyVolume`.
4. Add GA `storage.k8s.io/v1` VolumeAttributesClass manifests and update examples.
5. Upgrade `csi-provisioner` to `v6.3.0` with the VAC feature gate.
6. Add focused unit and manifest tests.
7. Run formatting and `make test`.
