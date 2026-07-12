# Implement: 密钥绑定桶下拉选择与删桶约束

## Checklist

1. **共享/存储层（若需要）**
   - [x] 在 webadmin/handlers 侧通过 DB 查询绑定，保持 storage 无凭证依赖。
   - [x] 桶存在性使用 `BucketStore.GetACL` 的 `exists`。

2. **管理端 API**
   - [x] `createCredential` / `updateCredential`：`normalizeCredentialBucket` 后，非空则校验桶存在，失败 400。
   - [x] `deleteBucket`：删除前 `Count credentials where bucket = name`；>0 则 409 `bucket has bound credentials`。
   - [x] 旁路映射绑定冲突错误。
   - [x] 更新/新增测试：
     - 绑定不存在桶 → 400
     - 先建桶再绑定 → 200/201
     - 有绑定删桶 → 409 文案
     - 解绑/改绑后可删
     - 修 `TestCredentialBucketScoping`：先 POST buckets

3. **S3 DeleteBucket**
   - [x] `BucketHandler` 注入 credential 绑定检查回调。
   - [x] `boundCredentialChecker func(bucket string) (bool, error)` 由 main/router 注入 GORM count。
   - [x] 有绑定 → `WriteS3Error(..., "BucketNotEmpty", 409, ...)`。
   - [x] 单测：空桶但有绑定 → BucketNotEmpty；无绑定空桶 → 204。

4. **Seed**
   - [x] `seedCredential` 前校验非空 bucket 存在。
   - [x] 失败由 main `slog.Error` + exit。
   - [x] README 补充：非空 `-seed-bucket` 须先存在于 `buckets` 表。

5. **前端**
   - [x] `Credentials.vue`：绑定桶改为 select；加载 buckets；悬空历史 option；列表异常提示。
   - [x] `Buckets.vue`：确认文案 + `toBucketError` 映射。
   - [x] `npm run build --prefix pkg/webadmin/ui`。

6. **文档/规格（实现后）**
   - [x] README 管理 API 删除桶说明：非空或有绑定均不可删。
   - [x] backend/frontend webadmin specs 已同步。

## Validation

```bash
go test ./pkg/webadmin/ ./pkg/handlers/ ./pkg/server/ ./pkg/storage/ -count=1
go test ./... -count=1
npm run build --prefix pkg/webadmin/ui
go build -o /tmp/natives3bridge ./cmd/natives3bridge
```

手动（可选）：

1. 建桶 A → 建密钥绑 A → 删桶 A → 应失败。
2. 密钥改「全部桶」→ 再删 A → 成功。
3. 密钥绑不存在名（API）→ 400。
4. 编辑悬空密钥 → 下拉强制改绑。

## Risk / Rollback points

| 风险 | 缓解 |
|------|------|
| S3 handler 无 DB 依赖，注入检查回调 | wiring 在 `server.NewRouter` / `NewBucketHandler` |
| 测试先绑后建桶 | 改 fixture 顺序 |
| 仅校验 `buckets` 表、历史仅有目录无行 | 与现有 Create/List 一致；不存在元数据则不可绑（与 listBuckets UI 一致） |

## Order

1. 后端校验 + 删桶约束 + 测试  
2. seed + README  
3. 前端下拉与文案  
4. 全量 test + UI build  

## Ready for start when

- [x] prd 决策闭合  
- [x] design 边界与错误契约明确  
- [x] 用户已授权按顺序实施全部规划任务。  
