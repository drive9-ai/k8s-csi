---
title: Dat9 Dev CSI Smoke Test - 2026-06-07
status: historical
---

## Historical Status

This report is a point-in-time record of the June 2026 implementation. Image
names, deployment commands, unsupported features, and cleanup behavior below
must not be treated as the current contract. Use `README.md`, `AGENTS.md`, and
the current `deploy/` manifests for current behavior.

## Scope

Kubeconfig: `~/.kube/dat9-dev.kc`

Cluster context: `arn:aws:eks:ap-southeast-1:401696231252:cluster/dev-dat9`

Final result: full ECR image e2e passed. The first controller-only run validated dynamic provisioning/deletion; the follow-up ECR run validated controller Deployment, node DaemonSet, PVC provisioning, Pod mount write/read, Pod recreation readback, PVC/PV deletion, and Drive9 remote-root deletion.

## Environment Checks

- `kubectl version --short`
  - Client: `v1.27.0`
  - Server: `v1.34.7-eks-40737a8`
  - Warning: client/server minor skew is outside the supported +/-1 range.
- Nodes:
  - `ip-10-0-3-179.ap-southeast-1.compute.internal`, Ready, linux/amd64
  - `ip-10-0-40-227.ap-southeast-1.compute.internal`, Ready, linux/amd64
- Initial `drive9-csi` namespace and `drive9-rwo` StorageClass did not exist.
- Existing `dat9-server` was running in namespace `dat9` behind service `dat9-server`.

## Image Work

The repository default image `ghcr.io/drive9-ai/drive9-csi:0.1.0` was not usable:

- Local `docker manifest inspect ghcr.io/drive9-ai/drive9-csi:0.1.0` returned `manifest unknown`.
- The cluster also failed to pull that image with GHCR anonymous token `403 Forbidden`.
- ECR auth in local Docker was expired, and local AWS CLI had no credentials.
- Building the full Dockerfile failed because Docker Hub auth/registry endpoints timed out.
- Fetching Drive9 source for a linux/amd64 mounter binary also failed due network/DNS issues.

Before ECR access was available, I built a controller-only scratch image from the current workspace to validate the controller path:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /private/tmp/drive9-csi-smoke-bin/drive9-csi ./cmd/drive9-csi
docker build -f - -t ttl.sh/drive9-csi-controller-smoke-2bf5e78-20260607142412:2h /private/tmp/drive9-csi-smoke-bin
docker push ttl.sh/drive9-csi-controller-smoke-2bf5e78-20260607142412:2h
```

Pushed digest: `sha256:a21bc0b6dda33331d14c7e39665474962578011ecc341784aa404e253a1e0a3b`

This image only validates the CSI controller service. It does not contain `/usr/local/bin/drive9`, so it cannot validate node `NodeStageVolume` or actual Pod filesystem mount.

After `AWS_PROFILE=1252` access was authorized, I built and pushed a full linux/amd64 image to account `401696231252` ECR:

- ECR repository: `401696231252.dkr.ecr.ap-southeast-1.amazonaws.com/drive9-csi`
- Repository settings: immutable tags, scan-on-push enabled, AES256 encryption
- Tag: `dat9-dev-smoke-2bf5e78-20260607152249`
- Immutable image: `401696231252.dkr.ecr.ap-southeast-1.amazonaws.com/drive9-csi@sha256:b26a1f120a9f4f4151eb87f1073a2644d6d577b3229b9e0aec89aa5eb47866d3`
- ECR push time: `2026-06-07T23:36:53+08:00`
- ECR image size: `50767601` bytes
- Local image ID: `sha256:3f09aa1a1dba260a1c98cbf2b4aad5c571cd872407b292b27a63f2b6b93aad22`

Build command:

```sh
AWS_PROFILE=1252 aws ecr get-login-password --region ap-southeast-1 | docker login --username AWS --password-stdin 401696231252.dkr.ecr.ap-southeast-1.amazonaws.com
docker build --platform linux/amd64 --pull=false --build-arg DRIVE9_REF=68ce029f889a1a6ac17b07fb9d6b5849ce39631b -t 401696231252.dkr.ecr.ap-southeast-1.amazonaws.com/drive9-csi:dat9-dev-smoke-2bf5e78-20260607152249 .
docker push 401696231252.dkr.ecr.ap-southeast-1.amazonaws.com/drive9-csi:dat9-dev-smoke-2bf5e78-20260607152249
```

Notes:

- Docker Hub endpoints were intermittently unreachable over the local network. I pulled the base images through AWS Public ECR and retagged them locally as `debian:bookworm-slim` and `golang:1.25-bookworm`, then built with `--pull=false`.
- The Dockerfile was updated from `golang:1.24-bookworm` to `golang:1.25-bookworm` because the pinned Drive9 source requires `go >= 1.25.1`.
- The pushed full image was locally verified to contain both `/usr/local/bin/drive9-csi` and `/usr/local/bin/drive9`.

## Credential Setup

Existing local contexts for the ap-southeast-1 dat9 server failed with `HTTP 500 {"error":"backend unavailable"}` during provisioning. For at least one of those tenants, `dat9-server` logs showed a backend load failure from a TiDB schema contract mismatch.

I then used port-forwarding to the in-cluster server and created a fresh temporary test owner context:

```sh
kubectl --kubeconfig=/Users/qifangfang/.kube/dat9-dev.kc -n dat9 port-forward svc/dat9-server 19009:80
HOME=/private/tmp/drive9-csi-smoke-new-home drive9 create --server http://127.0.0.1:19009 --name csi-smoke-20260607
```

Created test tenant: `26aa93c2-c90d-4d32-9686-e41558cc5f4b`

The Kubernetes Secret used server `http://dat9-server.dat9.svc`. API keys are intentionally not recorded here.

## Controller Deployment

Applied:

```sh
kubectl --kubeconfig=/Users/qifangfang/.kube/dat9-dev.kc create namespace drive9-csi
kubectl --kubeconfig=/Users/qifangfang/.kube/dat9-dev.kc -n drive9-csi create secret generic drive9-csi-secret --from-literal=server=http://dat9-server.dat9.svc --from-file=apiKey=<redacted-file>
kubectl --kubeconfig=/Users/qifangfang/.kube/dat9-dev.kc apply -f deploy/kubernetes/rbac.yaml
kubectl --kubeconfig=/Users/qifangfang/.kube/dat9-dev.kc apply -f deploy/kubernetes/csidriver.yaml
kubectl --kubeconfig=/Users/qifangfang/.kube/dat9-dev.kc apply -f deploy/kubernetes/controller.yaml
kubectl --kubeconfig=/Users/qifangfang/.kube/dat9-dev.kc -n drive9-csi set image deployment/drive9-csi-controller drive9-csi=ttl.sh/drive9-csi-controller-smoke-2bf5e78-20260607142412:2h
```

Controller result:

- Pod `drive9-csi-controller-59f57764dc-h8dwx`
- Status `2/2 Running`
- Restarts `0`
- Node `ip-10-0-3-179.ap-southeast-1.compute.internal`

## CreateVolume Test

Temporary StorageClass:

- Name: `drive9-rwo-delete-smoke`
- Provisioner: `csi.drive9.ai`
- `remoteRootPrefix: /k8s/csi-smoke-20260607`
- `reclaimPolicy: Delete`
- `volumeBindingMode: Immediate`

PVC:

- Namespace/name: `drive9-csi/drive9-csi-smoke-pvc`
- Result: `Bound`
- PV: `pvc-f28f169e-3efa-4d29-9599-54ac5f56c26d`
- VolumeHandle: `drive9-f392792cc2be29861c800b5451602fed`
- Remote root: `/k8s/csi-smoke-20260607/pvc-f28f169e-3efa-4d29-9599-54ac5f56c26d-138d5d44c2bc`

External provisioner logged:

```text
ProvisioningSucceeded ... Successfully provisioned volume pvc-f28f169e-3efa-4d29-9599-54ac5f56c26d
```

Backend verification before delete:

```text
size: 0
isdir: true
revision: 1
mtime: 2026-06-07T14:54:20Z
```

## DeleteVolume Test

Deleted PVC:

```sh
kubectl --kubeconfig=/Users/qifangfang/.kube/dat9-dev.kc -n drive9-csi delete pvc drive9-csi-smoke-pvc --wait=true
kubectl --kubeconfig=/Users/qifangfang/.kube/dat9-dev.kc wait --for=delete pv/pvc-f28f169e-3efa-4d29-9599-54ac5f56c26d --timeout=180s
```

Results:

- PVC deleted.
- PV deleted.
- `drive9 fs stat /k8s/csi-smoke-20260607/pvc-f28f169e-3efa-4d29-9599-54ac5f56c26d-138d5d44c2bc` returned `not found`.

This confirms the CSI controller `DeleteVolume` path removed both the Kubernetes PV and Drive9 remote root for the temporary Delete-policy volume.

## Full ECR Image E2E - 2026-06-08

After the full image was available in 1252 ECR, I reran the test with both controller and node plugin deployed.

Connectivity note: the original EKS endpoint in `~/.kube/dat9-dev.kc` used an uppercase hostname and DNS resolution became intermittent. For the e2e, I used a temporary kubeconfig with server `https://13.228.248.42` and `tls-server-name` set to the lowercase EKS hostname. The temporary kubeconfig was deleted during cleanup.

Fresh Drive9 test context:

- Name: `csi-ecr-e2e-20260607`
- Tenant ID: `485ec6e1-dee2-4780-8cd2-4c3af7bbee8f`
- Server used by Kubernetes Secret: `http://dat9-server.dat9.svc`
- API key: not recorded here; local temporary key material was deleted.

Applied and patched the CSI deployment:

```sh
AWS_PROFILE=1252 kubectl --kubeconfig=/private/tmp/dat9-dev-ip.kc create namespace drive9-csi
AWS_PROFILE=1252 kubectl --kubeconfig=/private/tmp/dat9-dev-ip.kc -n drive9-csi create secret generic drive9-csi-secret --from-literal=server=http://dat9-server.dat9.svc --from-file=apiKey=<redacted-file>
AWS_PROFILE=1252 kubectl --kubeconfig=/private/tmp/dat9-dev-ip.kc apply -k deploy/kubernetes
AWS_PROFILE=1252 kubectl --kubeconfig=/private/tmp/dat9-dev-ip.kc -n drive9-csi set image deployment/drive9-csi-controller drive9-csi=401696231252.dkr.ecr.ap-southeast-1.amazonaws.com/drive9-csi:dat9-dev-smoke-2bf5e78-20260607152249
AWS_PROFILE=1252 kubectl --kubeconfig=/private/tmp/dat9-dev-ip.kc -n drive9-csi set image daemonset/drive9-csi-node drive9-csi=401696231252.dkr.ecr.ap-southeast-1.amazonaws.com/drive9-csi:dat9-dev-smoke-2bf5e78-20260607152249
```

Rollout results:

- `drive9-csi-controller-7db67dbfb8-tl55z`: `2/2 Running`, `0` restarts
- `drive9-csi-node-rvgtg`: `2/2 Running`, `0` restarts on `ip-10-0-3-179.ap-southeast-1.compute.internal`
- `drive9-csi-node-xrcnn`: `2/2 Running`, `0` restarts on `ip-10-0-40-227.ap-southeast-1.compute.internal`

StorageClass:

- Name: `drive9-rwo-delete-smoke-e2e`
- `remoteRootPrefix: /k8s/csi-ecr-e2e-20260607`
- `profile: coding-agent`
- `reclaimPolicy: Delete`
- `volumeBindingMode: WaitForFirstConsumer`

PVC and writer Pod:

- Namespace/PVC: `drive9-csi-smoke/drive9-ecr-e2e-pvc`
- Writer Pod: `drive9-csi-smoke/drive9-ecr-e2e-writer`
- Writer image: `public.ecr.aws/docker/library/busybox:1.36`
- Writer command wrote `drive9-ecr-e2e-20260607` to `/data/hello.txt` and read it back.

During this run, `csi-provisioner` initially failed because `WaitForFirstConsumer` requires reading the selected target node:

```text
failed to get target node: nodes "ip-10-0-3-179.ap-southeast-1.compute.internal" is forbidden
```

Fix applied:

```yaml
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "watch"]
```

After applying the RBAC fix, provisioning succeeded:

- PVC: `Bound`
- PV: `pvc-bd3afb49-f41e-4663-9a52-e8d31198f30c`
- Remote root: `/k8s/csi-ecr-e2e-20260607/pvc-bd3afb49-f41e-4663-9a52-e8d31198f30c-dd4a9f23a6c1`
- Writer logs and `kubectl exec` both returned `drive9-ecr-e2e-20260607`.

Pod recreation/readback:

- Deleted writer Pod.
- Created reader Pod `drive9-csi-smoke/drive9-ecr-e2e-reader` on the same PVC.
- Reader logs and `kubectl exec` both returned `drive9-ecr-e2e-20260607`.

Delete and backend cleanup:

```sh
AWS_PROFILE=1252 kubectl --kubeconfig=/private/tmp/dat9-dev-ip.kc -n drive9-csi-smoke delete pod drive9-ecr-e2e-reader --wait=true
AWS_PROFILE=1252 kubectl --kubeconfig=/private/tmp/dat9-dev-ip.kc -n drive9-csi-smoke delete pvc drive9-ecr-e2e-pvc --wait=true
AWS_PROFILE=1252 kubectl --kubeconfig=/private/tmp/dat9-dev-ip.kc wait --for=delete pv/pvc-bd3afb49-f41e-4663-9a52-e8d31198f30c --timeout=180s
HOME=/private/tmp/drive9-csi-ecr-e2e-home drive9 fs stat /k8s/csi-ecr-e2e-20260607/pvc-bd3afb49-f41e-4663-9a52-e8d31198f30c-dd4a9f23a6c1
```

Results:

- Reader Pod deleted.
- PVC deleted.
- PV deleted.
- `drive9 fs stat` returned `fs stat: not found`, confirming the Drive9 remote root was deleted.

## Cleanup

Deleted during the first controller-only run:

- `StorageClass/drive9-rwo-delete-smoke`
- `Deployment/drive9-csi-controller`
- `CSIDriver/csi.drive9.ai`
- RBAC from `deploy/kubernetes/rbac.yaml`
- Namespace `drive9-csi`
- Local port-forward process
- Local temporary key/config/build files under `/private/tmp`

Deleted during the full ECR e2e run:

- `Namespace/drive9-csi-smoke`
- `StorageClass/drive9-rwo-delete-smoke-e2e`
- `StorageClass/drive9-rwo`
- `Deployment/drive9-csi-controller`
- `DaemonSet/drive9-csi-node`
- `CSIDriver/csi.drive9.ai`
- RBAC from `deploy/kubernetes/rbac.yaml`
- `Namespace/drive9-csi`
- Local port-forward process
- Local temporary kubeconfig, manifest, context, and key files under `/private/tmp`

Final post-cleanup checks:

- `drive9-csi` namespace: not found
- `drive9-csi-smoke` namespace: not found
- `drive9-rwo` StorageClass: not found
- `drive9-rwo-delete-smoke-e2e` StorageClass: not found
- `CSIDriver/csi.drive9.ai`: not found
- Full e2e PV `pvc-bd3afb49-f41e-4663-9a52-e8d31198f30c`: not found
- Existing monitoring PV remained unchanged.

Known leftover:

- The server-side Drive9 test tenant `26aa93c2-c90d-4d32-9686-e41558cc5f4b` was created for this test. I deleted the local temporary context/key material, but I did not find or run a tenant-delete command.
- The server-side Drive9 test tenant `485ec6e1-dee2-4780-8cd2-4c3af7bbee8f` was created for the full ECR e2e. I deleted the local temporary context/key material, but I did not find or run a tenant-delete command.

## Remaining Gaps

Covered by the full ECR e2e:

- Node DaemonSet readiness.
- `NodeStageVolume`.
- Pod mount write/read.
- Data persistence across Pod recreation.
- Delete-policy PVC/PV/backend remote-root cleanup.

Not covered in this smoke test:

- Multi-node forced reschedule of the same RWO volume while the original node is unhealthy.
- Node reboot recovery with an existing FUSE mount process.
- Long-running throughput or cache-coherency behavior.
- Unsupported CSI features such as expansion, snapshots, RWX, and block volumes beyond the repository unit tests and explicit driver errors.
