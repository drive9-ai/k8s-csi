---
name: csi-append-log-design
title: CSI Append-Log Mount Parameter Design
description: |
  将 Drive9 --append-log 接入现有 CSI 卷级挂载参数链路。
  固定参数语义、实现边界和验收条件，供后续实施使用。
status: implemented
updated: 2026-09-06
---

## 1. 范围基线

目标：允许用户在创建卷时配置 `appendLogPatterns`，并由 CSI 在挂载与恢复时
传递给 Drive9 的重复 `--append-log=<pattern>` 参数。

用户已确认本设计并授权按 implementation plan 实施 A1–A7。
已实现 A1–A7，所有本地检查项通过。
详细实施与验证记录保存在本地 implementation plan；本地验收不代表发布或生产准入。

| 编号 | 本次范围及验收条件 |
| --- | --- |
| A1 | 创建时支持 VAC 参数，保留 StorageClass 参数入口；VAC 按 key 整值覆盖，包括显式空值清空 |
| A2 | 复用 pattern 规范化、校验和大小限制；append-log 单独配置时允许非 overlay profile |
| A3 | workspace-root 和 managed-directory 两种卷都保存规范值；缺省为空且不启用 |
| A4 | staging 与恢复使用确定性 argv，保留重复参数和原有挂载生命周期语义 |
| A5 | fallback sidecar 暴露 Drive9 原生环境变量，默认空；原生 CSI 仍通过卷参数配置 |
| A6 | 镜像构建和节点预检检查 CLI 是否支持 append-log，复用现有失败处理 |
| A7 | 增补行为测试、manifest 检查及独立 VAC 示例，更新用户文档 |

明确不包含：Drive9 CLI/FUSE/服务端改动、append-log API 或存储实现、自动识别 WAL、
新 profile/durability 默认值、动态 VAC 更新、强制重挂载、节点环境变量透传、
新持久化 schema 或迁移、兼容性框架、锁、后台任务、恢复状态机和邻接问题修复。
真实集群 E2E、性能评测、发布镜像、部署、commit、push 和 PR 属于后续操作。

预计生产 Net LoC 为 **50–90 LoC，Small**，包含生产代码及运行配置的新增与修改，
不计测试、检查工具和文档。与已确认范围一致。

## 2. 依据与现状

核对基线：CSI `c6a50eea631b1e0b283ce7652647424f05bd99b3`；
Drive9 `cff6d294b452b54a964de2217c137af303602be9`。
以下路径和行号指向这两个版本；Drive9 路径相对于其源码仓库。

| 依据 | 当前行为及本次用途 |
| --- | --- |
| Drive9 `cmd/drive9/cli/mount.go:201` | 注册可重复 `--append-log`，原生环境变量为 `DRIVE9_MOUNT_APPEND_LOG_PATTERNS` |
| Drive9 `cmd/drive9/cli/mount.go:271`、`:344`、`:459` | 环境变量与 flag 相加；环境变量转为 argv 供 supervisor 保留；仅支持 Drive9 FUSE |
| Drive9 `pkg/fuse/local_policy.go:40`、`pkg/fuse/append_log.go:478` | matcher 不改变路由；实际优化资格及服务端能力由 Drive9 判断 |
| CSI `internal/driver/mutable_parameters.go:32` | VAC 整值覆盖 StorageClass；已有参数白名单与校验入口 |
| CSI `internal/driver/mount_policy.go:25` | 两类 pattern 的规范化、profile 检查和三种大小限制 |
| CSI `internal/driver/driver.go:361`、`:426`、`:515`、`:683` | 创建前校验、两种卷的 VolumeContext 写入、staging 校验 |
| CSI `internal/driver/node_recovery.go:268` | 从卷属性重建共用 mount request |
| CSI `internal/driver/mount_args.go:31` | 集中生成挂载 argv，pattern 使用等号形式 |
| CSI `internal/driver/node_preflight.go:367`、`Dockerfile:65` | 主机与镜像中已有 CLI help 能力检查 |

此前的本地笔记 `claude-notes/issues-35-36-per-volume-mount-options-proposal.md`
记录了同类接入。已与上述源码核对；其中关于预检失败导致 Node 全局启动失败的旧描述
不作为依据，当前代码允许降级启动并按操作检查能力
（`internal/driver/node_preflight.go:141`、`internal/driver/node_preflight_test.go:287`）。

## 3. 用户契约

| 项目 | 决定 |
| --- | --- |
| 参数名 | `appendLogPatterns` |
| 首选来源 | `VolumeAttributesClass.parameters`，仅在卷创建时生效 |
| 兼容入口 | `StorageClass.parameters`；手写 PV 的 `volumeAttributes` 使用同一解析规则 |
| 值类型 | 一个字符串，每行一个 pattern |
| 默认值 | 空列表；不保存该 key，不输出 append-log flag |
| VAC 优先级 | 覆盖完整的 StorageClass 同名值；空字符串或全空白可清空 |
| 保存形式 | 规范化后以单个换行符连接，保存到 `VolumeContext` / PV `volumeAttributes` |
| CLI 映射 | 每个 pattern 对应一个 `--append-log=<pattern>` argv 元素 |
| 匹配坐标 | 挂载文件系统内的路径，由 Drive9 解释；CSI 不拼接 remoteRoot 或主机挂载点 |

新增独立示例 `deploy/examples/kubernetes/volumeattributesclass-append-log.example.yaml`：

```yaml
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: drive9-append-log
driverName: csi.drive9.ai
parameters:
  profile: none
  appendLogPatterns: |-
    data/app.db-wal
    logs/events.log
```

新 PVC 使用 `spec.volumeAttributesClassName: drive9-append-log`，其他 Secret、
StorageClass 和卷身份配置沿用已有示例。此处显式使用 `none` 展示非 overlay 支持，
不改变默认 VAC，也不新增默认 profile 或 durability。

对应的新增 argv 为：

```text
--append-log=data/app.db-wal
--append-log=logs/events.log
```

pattern 声明本身不保证发生增量追加，也不保证某种性能提升。Drive9 负责判断文件路由、
服务端能力、写入形态和优化资格；CSI 不探测服务端能力或实现追加/回退逻辑。
该选项不限于 `-wal` 后缀。

### Profile 和路由组合

| 配置 | CSI 结果 |
| --- | --- |
| 仅 append-log，profile 缺省、`coding-agent`、`portable` 或自定义 | 按现有 profile 传递方式接受 |
| 仅 append-log，profile 为 `none` 或 `interactive` | 接受；append-log 不要求 overlay |
| `none` / `interactive` 加非空 local-only 或 remote-only | 保留原有 `InvalidArgument`，无论是否配置 append-log |
| append-log 与 local-only 匹配同一路径 | 不判冲突、不自动改路由；Drive9 的本地文件不参与远端追加优化 |
| append-log 与 remote-only 匹配同一路径 | 不判冲突；路由由 remote-only 规则决定，优化资格由 Drive9 决定 |

## 4. 校验和大小限制

扩展现有 `mountPathPolicy`，增加 `AppendLogPatterns []string`；复用
`normalizeMountPolicyParameter` 和 `validateMountPolicyPattern`。

1. 按换行拆分，去掉每行首尾空白，忽略空行。
2. 仅在同一列表内对规范化后的字符串精确去重，保留首次出现顺序。
3. 延续现有校验：拒绝无效 UTF-8、NUL、反斜杠、现有规则禁止的控制字符，
   以及独立的 `.` / `..` 路径段；不新增 glob 编译器或重写 pattern。
4. 对 `--append-log=` 编码后的参数执行现有凭据扫描；保留现有允许的
   credential-shaped 路径文本，不新增对原始文本的敏感词过滤。
5. 错误返回 `InvalidArgument`，包含参数名、原始行号及原因，不回显原始 pattern。
6. 创建流程在读取 PVC/Secret 和写远端元数据之前拒绝非法或过大的参数；
   staging、PV 恢复和 `ControllerModifyVolume` 使用同一规则。

三类列表共同使用已有预算，不为 append-log 单独分配第二份额度：

| 检查 | 限制 |
| --- | --- |
| 三个原始字符串的字节数之和 | 不超过 64 KiB，在规范化前检查 |
| 规范化后三类 argv 的 `len(arg) + 1` 之和 | 不超过 64 KiB，包含 flag 前缀及 NUL 预算 |
| 三类 argv 的 JSON 编码大小 | 不超过 `maxMountStateLength / 4`，当前为 256 KiB |

`validateMountPathPolicyContract` 的空列表快速返回必须覆盖三类列表；overlay 检查
只取决于 local-only / remote-only 是否非空。只有 append-log 时也必须执行大小检查。
不修改 mount-state 已有的整体 1 MiB 写入限制。

## 5. 参数传递与生命周期

```text
StorageClass parameters + VAC mutable_parameters
  -> 整值覆盖、校验、规范化
  -> CreateVolume.VolumeContext -> PV volumeAttributes
  -> NodeStageVolume / node recovery
  -> drive9MountRequest.Policy.AppendLogPatterns
  -> 确定性 argv -> 现有 host launcher / Drive9 supervisor
```

1. `supportedMutableMountParameters` 注册 `appendLogPatterns`。
2. `mountPathPolicy.addToVolumeContext` 保存非空规范值；两种卷的现有调用直接覆盖。
3. mount request 已携带整个 `mountPathPolicy`，staging 与恢复复用现有解析入口。
4. argv 顺序固定为 local-only、remote-only、append-log，列表内保持规范化顺序；
   所有 pattern 参数位于两个位置参数之前。`--append-log=--foreground` 必须仍是一个元素。
5. 复用现有 `MountArgs` / `FallbackMountArgs` 保存和恢复 argv。健康挂载继续按现有规则
   接管，不因为新增字段或不同 argv 强制重挂；starting 恢复保留该次尝试已保存的参数，
   active 重启通过已有流程从卷属性构造目标 argv，fallback 保留对应旧尝试的 argv。
6. 无该属性的既有 PV 解析为空；不新增 schema、迁移、版本协商或降级流程。
7. `ControllerModifyVolume` 对合法参数仍返回 `Unimplemented`，非法值返回
   `InvalidArgument`；配置更新不是动态生效入口。

## 6. Sidecar 和 CLI 检查

在 `deploy/sidecar/deployment.yaml` 的 mounter 容器中增加：

```yaml
- name: DRIVE9_MOUNT_APPEND_LOG_PATTERNS
  value: ""
```

Drive9 自行解析每行 pattern，并将环境变量快照转为 supervisor argv。sidecar 不做
shell 分词、glob 展开或额外 flag 拼接。原生 CSI 不增加此环境变量的传递入口。

| 位置 | 增量检查及失败行为 |
| --- | --- |
| `Dockerfile` | 现有 runtime help 检查增加 `-append-log ` 和 `DRIVE9_MOUNT_APPEND_LOG_PATTERNS`；缺失则构建失败 |
| `node_preflight.go` | 现有必需 flag 列表增加 `append-log`；缺失时将 `drive9-execution` 标为不可用并指出 flag |
| `hack/check-manifests.go` | 检查上述 Dockerfile 条目、sidecar 空环境变量和独立 VAC 示例；继续禁止默认 VAC 自动启用 policy |

CLI 要求适用于该 CSI 版本的镜像和节点，即使当前卷没有配置 append-log，也沿用完整
CLI 能力集合的检查方式。节点保留现有按操作检查能力的行为，不新增全局退出条件。

源代码基线证明了支持情况，未核实公开下载端点当前发布的二进制版本。后续镜像构建
须使用具备全部既有能力及 append-log 的已发布 CLI，由实际 help 检查确认；不把源码
commit 自动当成已发布或生产准入证明。本设计不修改镜像发布或生产准入流程。

## 7. 取舍和实施边界

| 方案 | 结论 |
| --- | --- |
| 扩展已有 `mountPathPolicy` | 采用；增加一个字段和对应处理，复用预算、保存和恢复路径 |
| 独立 append-log 配置对象及整套解析/持久化入口 | 不采用；重复现有链路，还需额外组合大小预算 |
| 原生 CSI 通过节点环境变量配置 | 不采用；配置无法由单卷属性固定，新增优先级来源 |

计划修改的生产路径只有 `mount_policy.go`、`mutable_parameters.go`、`mount_args.go`、
`node_preflight.go`、`Dockerfile` 和 sidecar manifest。`driver.go`、`node_recovery.go`
及状态模块为重点验证路径，预期不需要新增生产逻辑。

## 8. 验收与后续

| 覆盖 | 必须证明的结果 |
| --- | --- |
| A1–A3 | VAC/StorageClass 覆盖、显式清空、两种卷保存、旧 PV 缺省、非法值在外部访问前拒绝 |
| A2 | 空白/CRLF/重复处理、错误脱敏、profile 组合、仅 append-log 及混合列表的三种大小限制 |
| A4 | 精确 argv 和 flag-like pattern；staging 属性重建、boot recovery 和保存的 argv 保留参数；健康挂载不强制重启 |
| A5–A7 | sidecar 默认空、示例可被 manifest 检查识别、缺失 CLI flag 的预检失败、默认 VAC 未启用 |

实现后运行 `make check`，覆盖格式、单测、race、vet、Linux 测试编译、构建产物及
非 Go 配置检查。Go 单测不读取 Dockerfile/YAML/shell，也不运行构建命令。
本地测试证明 CSI 参数契约；不宣称验证了真实 FUSE、服务端追加或性能收益。

实施步骤和验收日志保存在 `.sisyphus/plans/csi-append-log.md`，按现有忽略规则仅留本地。
最终实现仍限定为 A1–A7；上述服务端、schema、动态更新和真实集群工作保持范围外，
原生产估算为 **50–90 LoC**，实际为 **33 LoC，Small**，复用了全部既有调用点。
`make check` 首次因磁盘不足中断；用户清理后补跑 Linux 编译和构建检查，所有检查项
均已通过。未运行真实镜像构建、集群 E2E、性能验证或任何发布操作。
