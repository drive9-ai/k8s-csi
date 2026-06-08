# Task: Publish Full CSI Image To 1252 ECR

## Objective
使用 `AWS_PROFILE=1252` 为当前 workspace 构建完整 Drive9 CSI linux/amd64 镜像，推送到 AWS account `401696231252` 的 ap-southeast-1 ECR，并记录可用于 Kubernetes 部署的 immutable 镜像地址。

## Planned Steps

### Phase 1: ECR Readiness
- [x] Confirm AWS profile identity and target ECR registry.
- [x] Confirm or create the `drive9-csi` ECR repository.
- [x] Authenticate Docker to the target ECR registry.

### Phase 2: Build and Push
- [x] Build the full linux/amd64 image from the repository Dockerfile.
- [x] Push the image to ECR using an immutable smoke-test tag.
- [x] Record the pushed digest and image reference.

### Phase 3: Follow-up
- [x] Update the dat9-dev test notes with the ECR image reference.
- [x] State whether this image is ready for full node DaemonSet/Pod mount testing.

## Result

- AWS account: `401696231252`
- ECR repository: `401696231252.dkr.ecr.ap-southeast-1.amazonaws.com/drive9-csi`
- Pushed tag: `dat9-dev-smoke-2bf5e78-20260607152249`
- Immutable image: `401696231252.dkr.ecr.ap-southeast-1.amazonaws.com/drive9-csi@sha256:b26a1f120a9f4f4151eb87f1073a2644d6d577b3229b9e0aec89aa5eb47866d3`
- Full dat9-dev e2e passed with controller Deployment, node DaemonSet, PVC dynamic provisioning, Pod write/read, Pod recreation readback, PVC deletion, PV deletion, and Drive9 remote-root deletion.
- Fixes discovered during this e2e:
  - `Dockerfile` now uses Go 1.25 because the pinned Drive9 source requires `go >= 1.25.1`.
  - `deploy/kubernetes/rbac.yaml` grants `nodes` get/list/watch because `csi-provisioner` needs node reads for `WaitForFirstConsumer`.

---
Created: 2026-06-07
Status: COMPLETED

# Task: Dat9 Dev Kubernetes CSI Smoke Test

## Objective
使用 `KUBECONFIG=~/.kube/dat9-dev.kc` 指向的 Kubernetes 集群验证 Drive9 CSI 安装、PVC 创建、Pod 挂载读写、清理删除等路径，并记录可复现的测试步骤、命令、结果和任何问题。

## Planned Steps

### Phase 1: Cluster and Image Readiness
- [x] Confirm kubeconfig access, current cluster identity, server version, nodes, and storage prerequisites.
- [x] Decide which driver image/tag is actually deployed for the smoke test and avoid accidentally testing stale code.
- [x] Check or create the needed Drive9 Secret in a dedicated test namespace.

### Phase 2: Deploy and Exercise CSI
- [x] Apply controller-side CSI manifests to the cluster.
- [x] Wait for controller Deployment and node DaemonSet readiness.
- [x] Create a test PVC and verify dynamic provisioning.
- [x] Create a test Pod that writes/reads data through the mounted volume.
- [x] Validate data remains visible across Pod restart/recreate.

### Phase 3: Cleanup and Evidence
- [x] Delete test PVC and verify cleanup behavior for Delete policy.
- [x] Collect relevant resource status, events, and logs if anything fails.
- [x] Record exact steps and results in a test report.

## Result

- First controller-only run passed CreateVolume/DeleteVolume but could not cover node mount behavior.
- Follow-up ECR full-image run passed the full dat9-dev e2e:
  - `Deployment/drive9-csi-controller`: `2/2 Running`, `0` restarts.
  - `DaemonSet/drive9-csi-node`: both linux/amd64 nodes `2/2 Running`, `0` restarts.
  - PVC `drive9-csi-smoke/drive9-ecr-e2e-pvc`: `Bound`.
  - Writer Pod wrote and read `drive9-ecr-e2e-20260607` from `/data/hello.txt`.
  - Reader Pod recreated on the same PVC and read the same content.
  - Deleting the PVC deleted PV `pvc-bd3afb49-f41e-4663-9a52-e8d31198f30c`.
  - Drive9 remote root `/k8s/csi-ecr-e2e-20260607/pvc-bd3afb49-f41e-4663-9a52-e8d31198f30c-dd4a9f23a6c1` returned `fs stat: not found` after deletion.
  - All test Kubernetes resources and local temporary key/kubeconfig files were cleaned up.

---
Created: 2026-06-07
Status: COMPLETED

# Task: Fifth Production Detail Review Pass

## Objective
继续以客户生产可用为标准复审当前完整改动，重点检查 CSI name/idempotency 语义、手工 PV/异常 VolumeContext、Secret/marker/index 一致性、Kubernetes 安装流程、节点清理恢复、sidecar fallback，以及测试是否覆盖真实风险；确认的问题继续修复并验证。

## Planned Steps

### Phase 1: Fresh Re-read
- [x] Re-read current TODO/notes plus source, tests, manifests, README, Makefile after fourth-pass changes.
- [x] Build a fifth-pass risk checklist focused on remaining production edge cases.

### Phase 2: Spec and Failure Review
- [x] Re-check CSI Create/Delete/Stage/Publish idempotency and unsupported feature behavior.
- [x] Re-check Drive9 marker/index ownership checks and remote path safety.
- [x] Re-check Kubernetes/sidecar install assets for placeholder credentials, namespace mistakes, and command drift.
- [x] Add focused tests for confirmed gaps.

### Phase 3: Verification
- [x] Patch confirmed issues.
- [x] Run full verification commands.
- [x] Update TODO and summarize fifth-pass findings.

---
Created: 2026-06-07
Status: COMPLETED

# Task: Fourth Production Detail Review Pass

## Objective
再次以客户生产可用为标准复审当前完整工作区，重点找上一轮仍可能遗漏的低级问题：CSI idempotency、Kubernetes sidecar 参数、Drive9 HTTP 错误分类、FUSE 子进程失败路径、清单安装安全性、测试有效性和文档一致性；确认的问题直接修复并补测试。

## Planned Steps

### Phase 1: Full Context Re-scan
- [x] Re-read repo notes, README, Makefile, Dockerfile, manifests, source files, and tests in the current worktree.
- [x] Build a fresh checklist of production risks independent of the previous pass.

### Phase 2: Deep Edge-case Review
- [x] Review controller create/delete idempotency and Drive9 HTTP behavior.
- [x] Review node stage/publish/unpublish/unstage lifecycle, state files, process cleanup, and restart recovery.
- [x] Review Kubernetes manifests, sidecar shell, RBAC, examples, and README install flow.
- [x] Review tests for meaningful coverage and add missing tests for confirmed gaps.

### Phase 3: Fix and Verify
- [x] Patch confirmed issues with minimal production-safe changes.
- [x] Run gofmt plus full verification commands.
- [x] Update TODO and summarize fourth-pass findings.

---
Created: 2026-06-07
Status: COMPLETED

# Task: Third Production Detail Review Pass

## Objective
再次从客户生产部署角度复审当前改动，重点检查 CSI 规范细节、真实安装步骤、异常恢复、节点重启、sidecar/清单兼容性、权限最小化和测试缺口，确认没有低级遗漏；发现问题继续修复并验证。

## Planned Steps

### Phase 1: Deployment and Spec Re-check
- [x] Re-read all manifests, Dockerfile, README install steps, and current Go code after prior fixes.
- [x] Check CSI unsupported features are explicitly rejected or documented.
- [x] Check Kubernetes sidecar compatibility, secrets, RBAC, hostPath, mount propagation, and install examples.

### Phase 2: Failure and Recovery Review
- [x] Review create/delete idempotency, partial failure recovery, node restart recovery, mount process lifecycle, and state-file mismatch handling.
- [x] Review tests for these conditions and add focused tests for confirmed gaps.

### Phase 3: Verification
- [x] Patch any confirmed issues.
- [x] Run full verification commands.
- [x] Update TODO and summarize third-pass findings.

---
Created: 2026-06-07
Status: COMPLETED

# Task: Second Production Review Pass

## Objective
按客户生产可用标准重新审查 Drive9 CSI lite 实现和上一轮修改，重点确认没有 CSI 语义、Kubernetes 清单、安全边界、幂等性、测试覆盖或文档流程上的低级遗漏；发现问题就直接修复并验证。

## Planned Steps

### Phase 1: Re-read Context
- [x] Re-read repo notes, README, task plan, and all source/manifests with the current diff applied.
- [x] Build a second-pass risk checklist for CSI controller, node lifecycle, Drive9 HTTP client, FUSE mount process, and deployment assets.

### Phase 2: Deep Review
- [x] Check current diff for regressions introduced by the first pass.
- [x] Check original code paths not touched in the first pass for production defects.
- [x] Check test cases against the risk checklist and add missing tests for confirmed gaps.

### Phase 3: Fix and Verify
- [x] Patch confirmed issues with narrowly scoped changes.
- [x] Run gofmt, unit tests, vet, kustomize rendering, and repo test target.
- [x] Update TODO and provide final findings.

---
Created: 2026-06-07
Status: COMPLETED

# Task: Production Readiness Review for Drive9 Kubernetes CLI / CSI Driver

## Objective
认真阅读并审查仓库实现，面向客户生产环境使用检查正确性、安全性、Kubernetes 语义、错误处理、测试覆盖与部署清单；如发现必须修复的问题，直接修改代码并补充测试。

## Planned Steps

### Phase 1: Context and Architecture
- [x] Check existing repo notes and previous TODO context.
- [x] Read README, task plan, module metadata, CLI entrypoint, driver implementation, and deployment manifests.
- [x] Map the current behavior across controller, node publish/stage, Drive9 client, path safety, and mount helpers.

### Phase 2: Risk Review
- [x] Review production-risk areas: path traversal, deletion safety, idempotency, K8s CSI contract behavior, secrets handling, mount lifecycle, platform gates, and manifest privileges.
- [x] Review tests for meaningful coverage and identify missing cases.

### Phase 3: Fixes and Tests
- [x] Implement narrowly scoped code fixes for confirmed issues.
- [x] Add or improve tests for the fixed behavior and critical edge cases.
- [x] Run formatting, unit tests, and available repo verification commands.

### Phase 4: Final Report
- [x] Update this TODO with completed status.
- [x] Summarize findings, changed files, tests run, and any residual production risks.

## Dependencies
- Tools: `rg`, Go toolchain, local shell commands.
- Files: `README.md`, `task_plan.md`, `notes.md`, `cmd/`, `internal/driver/`, `deploy/`.
- Permissions: workspace read/write only unless verification requires network or privileged mounts.

## Risks
- Some mount behavior may require Linux privileges and cannot be fully exercised on this local environment.
- Production-readiness gaps in external Drive9 CLI behavior may require assumptions if no API docs are present in repo.

## Expected Output
- Code and test changes if confirmed issues exist.
- A concise production-readiness review summary.

---
Created: 2026-06-07
Status: COMPLETED

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
