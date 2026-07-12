# 密钥绑定桶下拉选择与删桶约束

## Goal

密钥管理创建/编辑时，绑定桶从手输改为下拉选择（可保留「全部桶」）；删除桶时若仍有密钥绑定该桶则拒绝删除；创建/更新密钥时非空 `bucket` 必须指向已存在桶，从源头避免悬空绑定；历史悬空绑定在编辑时强制改绑；本地 seed 与管理路径规则对齐。

## Confirmed Facts（代码现状）

- 凭证模型 `credentials.bucket`：空字符串 = 可访问全部桶；非空 = 仅可访问该桶（Auth 中间件按路径桶名校验，越权 403 AccessDenied）。
- 管理端密钥表单 `Credentials.vue` 目前是手输 `input`，placeholder 示例 `my-bucket`；列表显示 `credential.bucket || '全部桶'`。
- `normalizeCredentialBucket` 只校验桶名格式（或空），**不校验桶是否存在**。
- 管理端/S3 删桶 `BucketStore.Delete` / `deleteBucket` 仅校验：桶名合法、目录存在、目录为空；非空返回 `bucket not empty`（409）。
- **删桶路径目前不检查**是否有密钥绑定该桶名；绑定密钥可继续存在于 DB，形成悬空绑定。
- 现有测试 `TestCredentialBucketScoping` 会创建绑定到不存在桶 `my-bucket` / `other-bucket` 的密钥——实现后需改为先建桶再绑定。
- `-seed-bucket` 在 `cmd/natives3bridge/main.go` 的 `seedCredential` 中直接写入 `Bucket`，不校验桶存在；README 示例多为空字符串。
- S3 非空删桶错误码为 `BucketNotEmpty`（409）；管理端对应 JSON `"bucket not empty"`。
- 相关实现：`pkg/webadmin/ui/src/views/Credentials.vue`、`pkg/webadmin/api.go`、`pkg/storage/bucketmeta.go`、`pkg/handlers/bucket.go`、`pkg/server/router.go` Auth 桶边界、`Buckets.vue` 删桶确认文案、`cmd/natives3bridge/main.go` seed。

## Decisions

| 决策 | 选择 | 说明 |
|------|------|------|
| 删桶有绑定密钥 | **拒绝删除** | 管理端/S3 删桶前检查 `credentials.bucket`；有绑定则冲突拒绝，提示先解绑/改绑 |
| 绑定桶可选范围 | **全部桶 + 已存在桶** | UI 下拉：空（全部桶）+ `listBuckets`；禁止手输任意名 |
| 创建/更新密钥后端 | **非空 bucket 必须存在** | 否则 400；与 UI 下拉配套，API 不可绕过 |
| 历史悬空绑定 | **编辑时强制改绑** | 列表可显示原桶名+异常提示；编辑下拉额外展示历史值（标记不可用），保存时必须改选「全部桶」或现有桶；不强制批量迁移 |
| seed 桶校验 | **非空 seed-bucket 要求桶已存在** | 与管理 API 一致；空字符串仍表示全部桶；不存在则启动失败并打印明确错误 |
| S3 有绑定删桶错误 | **复用 BucketNotEmpty** | S3 侧 409 + `BucketNotEmpty`；管理端 JSON 用独立文案区分「非空」与「有密钥绑定」 |

## Requirements

### 删桶约束
- 删除桶（管理端 API 与 S3 `DeleteBucket`）在「桶为空」之外，还必须检查是否仍有密钥 `credentials.bucket = 该桶名`。
- 存在绑定则拒绝删除：
  - **S3**：`BucketNotEmpty` + HTTP 409（与非空桶同一错误码，兼容 aws-cli/SDK）。
  - **管理端**：HTTP 409，JSON error 文案独立于「bucket not empty」，便于 UI 区分提示（例如 `bucket has bound credentials`）。
- 桶管理 UI 确认文案与错误映射说明「有密钥绑定时无法删除」。

### 密钥绑定桶下拉
- 创建/编辑密钥表单：绑定桶改为 `<select>`（或等价控件），选项至少包含「全部桶」与当前桶列表。
- 打开表单时加载/刷新桶列表；无桶时仅能选「全部桶」。
- 后端 create/update：非空 `bucket` 必须通过桶存在性校验（`BucketStore`/DB），否则 400。

### 历史悬空密钥
- 列表：绑定桶不在当前桶列表中时，仍显示原桶名，并给出可见异常提示（如「桶已不存在」）。
- 编辑：下拉可临时包含该历史值（标记不可用/已失效）；保存时若仍提交不存在桶则 400；用户须改选「全部桶」或现有桶。
- 不自动批量清空历史绑定，避免静默扩大权限。

### Seed 对齐
- `seedCredential` / `-seed-bucket`：非空时校验目标桶已存在（`buckets` 表或 `BucketStore` 可查询到）；不存在则启动失败，错误信息明确。
- 空 `-seed-bucket` 行为不变（全部桶）。
- README 中若示例使用非空 seed-bucket，需同步说明「须先建桶」。

## Acceptance Criteria

- [x] 有密钥 `bucket = X` 时，管理端 `DELETE /api/admin/buckets/X` 返回 409（独立于 `bucket not empty` 的文案）且桶与密钥均未被删除。
- [x] 有密钥 `bucket = X` 时，S3 `DeleteBucket` 对 `X` 返回 `BucketNotEmpty`（409）且桶与密钥均未被删除。
- [x] 无绑定且桶为空时，删桶行为与现网一致（成功删除）。
- [x] 桶管理 UI 对「有密钥绑定」错误有可读中文提示；确认文案提及该约束。
- [x] 密钥创建/编辑表单绑定桶为下拉：全部桶 + 已存在桶列表。
- [x] 管理端 create/update credential 对不存在的 bucket 返回 400，且不写入。
- [x] 合法存在桶或空（全部桶）的绑定仍可正常创建/更新。
- [x] 历史悬空绑定在列表有异常提示；编辑保存时不能继续提交不存在桶。
- [x] 非空 `-seed-bucket` 指向不存在桶时启动失败；指向已存在桶或空字符串时行为正确。
- [x] 现有/新增相关单元测试覆盖：绑定存在性、删桶有绑定拒绝、空绑定可删；`TestCredentialBucketScoping` 改为先建桶。

## Out of Scope

- 不改变 S3 侧 per-bucket scoping 语义（空=全桶，非空=单桶）。
- 不引入多桶绑定（仍是一密钥最多一个桶）。
- 不改配额/用量模型。
- 不自动解绑或级联删除密钥。
- 不强制「一桶一把密钥」唯一性（多密钥可绑同一桶）。
- 不提供批量「清理悬空绑定」运维工具（可后续单独立项）。
- 不新增管理端「查看某桶绑定了哪些密钥」专用 API（可选优化，非本任务必须）。

## Open Questions

（无阻塞项；以下为 design 内可定的实现细节）

1. 管理端 409 精确 error 字符串：`bucket has bound credentials`（推荐）。
2. 校验桶存在性：优先 `BucketStore`/`buckets` 表，与 ACL/列表同源。

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
