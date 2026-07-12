# 管理端存储对账：扫盘核对与配额回写

## Goal

在管理端提供**手动存储对账**能力：按 bucket 扫 `storage.data_root`，核对真实对象文件与 sidecar，对比绑定密钥的 `used_bytes`，并支持 dry-run / apply 两步操作。

**不**引入对象清单数据库表；磁盘仍是对象是否存在的唯一真相源。

本任务**仅规划、不实现**；后续准备开发时再 `task.py start`。

## Background / Problem

- 对象字节与 metadata 不入库；List/Get/Head 以磁盘文件为准。
- 密钥 `credentials.used_bytes` 仅在 S3 PUT / DELETE（及 multipart complete）路径增减。
- 运维或用户若直接在 `data_root` 下 `rm` / `cp` / `mv` 原生文件：
  - 对象从 S3 视角会立刻消失或出现；
  - `used_bytes` 可能虚高/虚低；
  - 可能残留孤儿 `.s3meta`（有 sidecar、无对象文件）。
- 当前管理端无扫盘核对、无配额回写、无孤儿 sidecar 清理入口。

## Confirmed Facts（代码现状）

- 对象路径：`data_root/<bucket>/<key>`；sidecar：`<object-path><metadata_suffix>`（默认 `.s3meta`）。
- DB 模型仅有 `credentials` / `request_stats` / `buckets` / `hook_configs`，无 objects 表（`pkg/db/models.go`；README：「对象字节、对象 metadata 和 tags 不存入数据库」）。
- `FileBackend.DeleteObject` 走 S3 删除时会 `os.Remove` 对象并 `DeleteSidecar`；手工删盘不会触发该路径，也不会改配额。
- 密钥 `credentials.bucket`：空 = 可访问全部桶；非空 = 绑定单桶（与 `07-12-credential-bucket-select` 对齐）。
- 管理 API 在 `server.admin_addr`，会话鉴权；公开 ops（healthz/readyz/metrics）不得承载对账写操作。
- 现有 `ListObjects` / walk 会跳过 sidecar、`.multipart`、db 类后缀文件。

## Decisions

| 决策 | 选择 | 说明 |
|------|------|------|
| 对象是否入库 | **否** | 保持磁盘为唯一对象真相；对账不写 objects 表 |
| 对账入口 | **管理端会话 API + UI 按钮** | 禁止挂到公开 ops / S3 匿名面 |
| 操作模型 | **dry-run 默认 + 显式 apply** | apply 需明确标志，避免误写 |
| 扫描粒度 | **第一期仅单桶** | 全量循环可二期；降低 IO/误操作面 |
| 统计口径 | **真实对象文件**（与 ListObjects 同过滤规则） | 不计 sidecar、临时 multipart、隐藏/非对象文件 |
| Apply 动作 | **(1) 删孤儿 sidecar (2) 回写绑定该桶密钥的 used_bytes** | 不删对象文件；不改 ACL；不自动补全 sidecar |
| 配额回写范围 | **仅 `credentials.bucket = 该桶` 的密钥** | 空绑定（全部桶）密钥单桶 apply 不改写 |
| 多密钥绑同一桶 | **每个绑定密钥 used_bytes 都设为 scanned_bytes** | 与「一桶多钥」现状一致；文档说明语义 |
| 外部新拷入文件 | **List 已可见；本任务不强制写 sidecar** | 「补元数据」另立任务 |
| 自动定时对账 | **不做** | 仅手动触发 |

## Requirements

### R1. Dry-run 扫盘报告

- 管理端可对指定 `bucket` 发起对账 dry-run。
- 报告至少包含：
  - `bucket`
  - `object_count` / `scanned_bytes`
  - `orphan_sidecar_count`（及可选相对路径样例，截断）
  - 绑定该桶的密钥列表：`id` / `access_key` / `name` / 当前 `used_bytes` / 与 `scanned_bytes` 的 `diff_bytes`
  - 将执行的动作摘要（清多少 sidecar、回写哪些密钥）
- dry-run **不得**修改磁盘或 DB。

### R2. Apply 执行

- 显式 `apply=true`（或独立 apply 端点 + 请求体确认）执行：
  1. 删除该桶下孤儿 sidecar；
  2. 将所有 `credentials.bucket = 该桶` 的密钥 `used_bytes` 设为本次 `scanned_bytes`。
- 返回实际结果：删除数、更新密钥数、最终 used_bytes。
- 更新 `used_bytes` 后必须使对应 `CredentialStore` 缓存失效（与现有密钥变更一致）。
- apply **不得**删除任何对象本体文件；**不得**修改桶 ACL / 密钥 status / secret。
- apply 时服务端**重新扫盘**，不信任客户端回传的计数值。

### R3. 安全与权限

- 仅管理员会话可调用；未登录 401。
- 不在 S3 监听端口暴露对账 API。
- 返回的路径样例使用桶内相对 key，限制条数与长度；不泄露无关系统路径。
- 非法桶名 / 不存在桶：明确 400/404，不扫盘。

### R4. 管理 UI

- 桶管理页入口：选择桶 → 核对（dry-run）→ 展示报告 → 确认后 apply。
- 文案明确：会清理孤儿 sidecar、会改绑定密钥用量；**不会恢复已删文件**。

### R5. 测试与文档

- 单测/API 测覆盖 dry-run 只读、apply 清 sidecar + 回写绑定密钥、空绑定密钥不被改、401、非法/缺失桶。
- README 或管理说明写明：对象不入库、对账用途、apply 影响面。

## Acceptance Criteria

- [x] 存在管理端受会话保护的对账接口（dry-run / apply 语义清晰）。
- [x] 对含 N 个对象文件、M 个孤儿 sidecar 的桶，dry-run 报告 `object_count=N`、`orphan_sidecar_count=M`、`scanned_bytes` 等于对象文件字节之和（过滤规则与 List 一致）。
- [x] dry-run 前后：磁盘 sidecar 数量、`credentials.used_bytes` 均不变。
- [x] apply 后：孤儿 sidecar 被删除；对象文件仍在；绑定该桶的密钥 `used_bytes == scanned_bytes`。
- [x] apply 后：未绑定该桶 / `bucket=""` 的密钥 used_bytes 不变。
- [x] apply 后相关密钥缓存失效，后续配额检查读到新 used_bytes。
- [x] 无会话调用返回 401；对账 API 不出现在公开 ops 面。
- [x] 管理 UI 可完成 dry-run → 确认 apply 流程，并有风险提示文案。
- [x] 文档写明对象不入库、对账用途与 apply 影响面。
- [x] 相关单测通过。

## Out of Scope

- 创建 objects / 对象清单表，或把每个 key 写入数据库。
- 版本控制、回收站、软删除、对象锁。
- 定时/后台自动全盘对账。
- 为外部拷入文件自动生成完整 `.s3meta` / 导入流水线。
- 第一期「全部桶一次 apply」与空绑定密钥全盘重算（可后续任务）。
- 修改 S3 DELETE/PUT 的实时配额语义（本任务只补手工对账）。
- 分布式多节点共享盘一致性（单机 `data_root` 假设）。

## Non-goals / Product Notes

- 对账不能恢复用户手工删除的文件；只能发现不一致并修正配额/垃圾 sidecar。
- `used_bytes` 回写是运维校正，不是对象级审计日志。

## Open Questions（design 内已给默认，实现前可改）

1. API 形态：单一 `POST .../reconcile` + `apply` 标志（推荐） vs dry-run/apply 双端点。
2. 孤儿 sidecar 样例列表上限：建议 50。
3. UI 挂在桶管理页 vs 独立菜单：建议桶管理页内按当前桶操作。

## Planning status

- 状态：`planning`（用户要求：创建任务但不执行）
- 实现前必须：`design.md` + `implement.md` 已齐 → 用户确认 → `task.py start`
- **禁止**在未 start 前改业务代码
