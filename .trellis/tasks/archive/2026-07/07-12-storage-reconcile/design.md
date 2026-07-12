# Design: 管理端存储对账

## 1. 目标与边界

在**不引入对象表**的前提下，提供管理端手动对账：

```text
磁盘对象文件  ──扫盘──►  object_count / scanned_bytes
磁盘 sidecar  ──对比──►  orphan_sidecars
DB used_bytes ──对比──►  diff；apply 时回写绑定密钥
```

权威源：

| 数据 | 权威 |
|------|------|
| 对象是否存在、大小 | 磁盘 `data_root` |
| 密钥配额账本 | DB `credentials.used_bytes`（可被 apply 校正） |
| 桶 ACL | DB `buckets`（本任务不改） |

## 2. 架构位置

```text
Admin UI (Buckets 页)
    │  session cookie
    ▼
POST /api/admin/buckets/{name}/reconcile   ← webadmin.API（鉴权中间件内）
    │
    ├─ storage: 扫盘统计 + 可选删孤儿 sidecar
    └─ db + CredentialStore.Invalidate: 回写 used_bytes
```

- **不**挂在 `OpsHandler`（healthz/readyz/metrics 保持探针/指标模型）。
- **不**挂在 S3 router。
- `webadmin.NewServer` / `NewAPI` 需能访问：`data_root` 或 `FileBackend`、`metadata_suffix`、`*gorm.DB`、密钥缓存失效接口。

### 依赖注入建议

现状 `NewAPI(gdb, invalidator, buckets *BucketStore)`。对账需要扫盘，可选扩展：

- 向 `NewAPI` 注入 `root` + `metadataSuffix`，或
- 新增 `StorageReconciler` 注入 API：

```go
type StorageReconciler struct {
    Root           string
    MetadataSuffix string
    DB             *gorm.DB
    Invalidator    interface{ Invalidate(string) }
}
```

优先复用 `storage` 包路径解析与过滤规则，避免 webadmin 手写路径穿越。

## 3. API 契约

### 3.1 端点

```http
POST /api/admin/buckets/{name}/reconcile
Content-Type: application/json

{ "apply": false }
```

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `apply` | bool | `false` | `false`=dry-run；`true`=执行清理+回写 |

路径 `{name}` 为桶名；须通过现有桶名规则校验，且与管理端其它桶 API 一致要求桶存在（`BucketStore`/DB）。

### 3.2 响应（dry-run 与 apply 共用形状）

```json
{
  "bucket": "media",
  "apply": false,
  "object_count": 123,
  "scanned_bytes": 10737418240,
  "orphan_sidecar_count": 5,
  "orphan_sidecar_samples": ["images/a.jpg.s3meta"],
  "bound_credentials": [
    {
      "id": 1,
      "access_key": "AK...",
      "name": "app",
      "used_bytes": 12884901888,
      "diff_bytes": 2147483648,
      "updated": false
    }
  ],
  "orphans_deleted": 0,
  "credentials_updated": 0
}
```

- dry-run：`orphans_deleted=0`，`credentials_updated=0`，`updated=false`。
- apply：填入实际删除/更新数；对应 credential `updated=true`，`used_bytes` 为回写后值，`diff_bytes=0`。
- **永不**返回 `secret_key`。
- `orphan_sidecar_samples` 最多 50 条，值为桶内相对路径（含 sidecar 后缀）。

### 3.3 错误

| 条件 | HTTP | body |
|------|------|------|
| 无会话 | 401 | `{"error":"unauthorized"}` |
| 非法桶名 | 400 | `{"error":"invalid bucket name"}` |
| 桶不存在 | 404 | `{"error":"bucket not found"}` |
| JSON 非法 / 未知字段 | 400 | 与现网 admin API 一致 |
| 扫盘/DB 内部错误 | 500 | 不泄露内部细节 |

## 4. 扫盘算法

### 4.1 对象统计

对 `ResolveBucketPath(root, bucket)` 目录 `filepath.WalkDir`：

- 跳过目录名 `.multipart`
- 跳过文件：后缀为 `metadataSuffix` / `.s3meta` / `.db` / `.sqlite` / `.sqlite3`（与 `FileBackend.ListObjects` 对齐）
- 只计常规文件
- `object_count++`；`scanned_bytes += size`
- key = 相对桶根的 slash 路径

### 4.2 孤儿 sidecar

- 文件名以 `metadataSuffix` 结尾
- 去掉后缀后的路径**不是**存在的常规文件 → 记为孤儿

### 4.3 Apply 顺序（服务端自包含）

客户端 dry-run 结果仅供展示；apply **服务端重新扫描**，避免 TOC/TOU。

1. 重新扫盘得到最新 `scanned_bytes` 与孤儿列表  
2. 删除孤儿 sidecar  
3. 更新绑定密钥 `used_bytes = scanned_bytes`  
4. 对每个更新的 `access_key` 调用 `Invalidate`

MVP：sidecar 删除失败 → 返回 500（已删的不回滚，可重跑）；DB 更新放在删除步骤之后。

## 5. 配额回写语义

```text
UPDATE credentials SET used_bytes = ? WHERE bucket = ? AND bucket <> ''
```

- 空 `bucket` 密钥：单桶 reconcile **不更新**
- 多密钥同绑一桶：各自 `used_bytes` 均设为同一 `scanned_bytes`  
  - 产品含义：每把绑定钥的账本按该桶当前磁盘占用校正，**不是**把桶容量均分
- `QuotaBytes` 不修改；若回写后 `used_bytes > quota_bytes`，允许（文档说明）
- 回写后必须 `Invalidate(access_key)`（参见 auth-quota 指南）
- **不**修改 `request_stats`（对账不是补 DELETE 事件）

## 6. 前端

### 入口

`Buckets.vue`（或等价）当前桶操作：「存储对账」。

### 流程

1. 点击 → `POST .../reconcile` `{apply:false}` → 展示摘要  
2. 若 `orphan_sidecar_count>0` 或任一 `diff_bytes!=0`，显示「执行校正」  
3. 二次确认文案：  
   - 将删除 N 个孤儿 sidecar  
   - 将把绑定该桶的 M 把密钥 used_bytes 设为 X  
   - **无法恢复已从磁盘删除的文件**  
4. 确认 → `{apply:true}` → 成功提示；如页面有 used 展示则刷新

### API client

`pkg/webadmin/ui/src/api/client.ts` 增加 `reconcileBucket(name, apply)`。

## 7. 测试设计

| 用例 | 期望 |
|------|------|
| 桶内 2 对象 + 1 孤儿 sidecar，dry-run | count=2，orphan=1，DB/磁盘不变 |
| apply | 孤儿消失，对象仍在，绑定钥 used=两对象之和 |
| 另有 bucket="" 密钥 | used 不变 |
| 另有绑其他桶密钥 | used 不变 |
| 无会话 | 401 |
| 不存在桶 | 404 |

夹具：`t.TempDir()` 作 data_root，写真实文件与 `.s3meta`。

## 8. 文档与 spec 更新（实现阶段）

- `README.md`：管理后台章节补「存储对账」
- `.trellis/spec/backend/webadmin-guidelines.md`：新路由与 JSON 契约
- `.trellis/spec/backend/auth-quota-guidelines.md`：used_bytes 可被 reconcile 写回且必须 Invalidate
- 可选 `storage-guidelines.md`：孤儿 sidecar 定义与 list 过滤一致

## 9. 风险与权衡

| 风险 | 缓解 |
|------|------|
| 大桶扫盘阻塞请求 | 第一期同步 API；文档建议低峰操作；二期可异步 job |
| apply 误点 | dry-run 默认 + UI 二次确认 |
| 多钥同桶 used 同值 | 文档写清；不假装共享池 |
| 与外部 cp 并发 | apply 重新扫盘；不保证跨进程事务 |

## 10. 明确不做

- objects 表  
- 全量 data_root 空绑定重算（二期）  
- 自动 cron  
- 删除对象本体  
- 补写 sidecar / 导入  

## 11. 与相关任务关系

- 依赖语义：`credentials.bucket` 绑定（`07-12-credential-bucket-select`）  
- 不阻塞该任务完成；对账实现时应使用已落地的绑桶字段与删桶约束
