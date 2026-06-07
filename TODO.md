# TODO

- [x] Confirm repository state and implementation scope.
- [x] Implement minimal Drive9 CSI-lite driver.
- [x] Add Kubernetes manifests for CSI controller/node deployment.
- [x] Add sidecar demo for customers that cannot install CSI yet.
- [x] Add tests for safety-critical path handling and access-mode validation.
- [x] Run formatting and tests.
- [ ] Commit and push the demo to `drive9-ai/csi`.

## Scope

This repository targets `github.com/mem9-ai/drive9` semantics. Do not use assumptions from `github.com/drive9-ai/drive9`.

Production-minimal means:

- RWO only.
- Secrets only for server/API key.
- `CreateVolume` creates a Drive9 remote directory and marker file.
- `DeleteVolume` deletes only a directory with both a matching metadata index entry and a matching root marker.
- `NodeStageVolume` mounts Drive9 FUSE on the node.
- `NodePublishVolume` bind-mounts the staged path into the pod.
- No snapshots, expansion, RWX, or cross-node cache-consistency claims.
