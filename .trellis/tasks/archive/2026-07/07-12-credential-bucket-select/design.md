# Design: 密钥绑定桶下拉选择与删桶约束

## Summary

在现有 per-bucket scoping（`credentials.bucket` 空=全桶 / 非空=单桶）之上，补齐三道一致性约束：

1. **写路径**：create/update/seed 非空 bucket 必须已存在。
2. **删路径**：删桶前若有密钥绑定则拒绝（S3 复用 `BucketNotEmpty`；管理端独立 JSON 文案）。
3. **UI**：密钥绑定改为下拉；桶删除确认/错误文案覆盖绑定约束；历史悬空绑定可编辑强制改绑。

不改 Auth 中间件 scoping 语义，不改配额。

## Boundaries

| 层 | 职责 |
|----|------|
| `pkg/storage` 或共享校验 helper | 可选：`ErrBucketHasCredentials` 错误值；或由 webadmin/handlers 直接查 DB |
| `pkg/webadmin/api.go` | create/update 校验桶存在；deleteBucket 查绑定并 409 |
| `pkg/handlers/bucket.go` | S3 DeleteBucket 在空桶检查后、`bucketStore.Delete` 前查绑定 → `BucketNotEmpty` |
| `cmd/natives3bridge/main.go` | seed 非空 bucket 存在性校验 |
| `Credentials.vue` / `Buckets.vue` | 下拉、悬空提示、删桶文案 |

## Contracts

### Admin API

**POST/PATCH credentials**

- 非空 `bucket`：先 `normalizeCredentialBucket`，再 `bucketExists(name)`。
- 不存在 → `400`，error：`bucket not found`（或更明确 `bucket does not exist`；实现时固定一种并测）。
- 空字符串 → 允许（全部桶）。

**DELETE /api/admin/buckets/{name}**

检查顺序建议：

1. 桶元数据/目录存在（现有 `BucketStore.Delete` 行为）。
2. **新增**：`credentials` 中是否存在 `bucket = name`（精确匹配，不含空字符串全局密钥）。
3. 目录是否为空（现有 ENOTEMPTY）。

若有绑定：

- HTTP `409`
- JSON：`{"error":"bucket has bound credentials"}`（与 `bucket not empty` 区分）

### S3 DeleteBucket

在对象非空检查之后、`bucketStore.Delete` 之前：

```
if hasBoundCredentials(bucket) {
    WriteS3Error(w, "BucketNotEmpty", 409, path)
    return
}
```

Message 仍走现有 `errorMessage("BucketNotEmpty")`（标准兼容）。

### Seed

```
if strings.TrimSpace(seedBucket) != "" {
  validate name
  if !bucketExists { return error("seed-bucket %q does not exist") }
}
```

## Data / Query

绑定检查：

```sql
SELECT COUNT(*) FROM credentials WHERE bucket = ?
```

存在性检查：

```sql
SELECT 1 FROM buckets WHERE name = ? LIMIT 1
```

或 `BucketStore.GetACL`/`List` 封装；优先复用 `BucketStore` 以免绕过缓存语义。`GetACL` 返回 `exists=false` 即可表示不存在。

## UI

### Credentials.vue

- 打开创建/编辑：`adminApi.listBuckets()` 填充 options。
- `<select>`：
  - value `""` → 文案「全部桶」
  - 每个 `bucket.name`
  - 编辑时若 `form.bucket` 不在列表：额外 `<option disabled>` 或带「（已不存在）」后缀，保存时前端可拦截并提示改选。
- 列表列：若 `credential.bucket` 非空且不在 buckets 集合 → 显示桶名 + 异常 badge/小字「桶已不存在」。

### Buckets.vue

- confirm：`确认删除桶 X？仅空桶可删；若仍有密钥绑定该桶，将无法删除。`
- `toBucketError` 增加：
  - `bucket has bound credentials` → 中文：`该桶仍有密钥绑定，请先在密钥管理中解绑或改绑后再删除。`

## Compatibility

- 已部署环境可能存在历史悬空绑定：不自动迁移；列表提示 + 编辑强制改绑 + 写路径 400。
- 依赖「先建桶再绑密钥」的测试与脚本需更新（`TestCredentialBucketScoping`）。
- aws-cli `rb` 在有绑定时看到 `BucketNotEmpty`，与非空桶一致；运维需靠管理端文案区分。

## Trade-offs

| 选择 | 收益 | 代价 |
|------|------|------|
| S3 复用 BucketNotEmpty | 客户端兼容 | 无法从 S3 错误 alone 区分「有对象」vs「有密钥」 |
| 管理端独立文案 | UI 可精确提示 | 双通道错误语义不完全对称 |
| 不级联解绑 | 避免静默扩大权限 | 删桶前多一步人工改绑 |

## Rollback

- 功能可独立回滚：去掉存在性校验与绑定检查即恢复旧行为。
- 无 schema migration（`credentials.bucket` 已存在）。
- 回滚后历史数据无额外迁移负担。

## Out of Design Scope

- 一桶一密钥唯一索引。
- 管理端「按桶列出绑定密钥」列表页。
- 批量清理悬空绑定 CLI。
