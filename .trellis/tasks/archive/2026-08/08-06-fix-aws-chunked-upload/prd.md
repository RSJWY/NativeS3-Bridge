# 修复 aws-chunked 上传被误判 QuotaExceeded 且对象损坏

来源：GitHub issue #2 — "PutObject 返回 QuotaExceeded（credential quota），但底层磁盘空间充足"

## 背景与真实根因

报告者的现象是所有 PutObject 都返回 `403 QuotaExceeded`，磁盘空间充足。**根因与配额无关。**

`quota_bytes = 0` 已经表示无限：`quota.Check` 只在 `QuotaBytes > 0` 时比较（`pkg/quota/quota.go:59`），`Reserve` 的 SQL 条件是 `quota_bytes = 0 OR used_bytes <= quota_bytes - ?`（`:70`），默认值就是 0（`pkg/db/models.go:12`）。所以配额既没配小，也没有只增不减的统计问题。

真实原因：boto3 ≥ 1.36 对 PutObject 默认启用 CRC32 校验和，请求变成

```
Content-Encoding: aws-chunked
x-amz-content-sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER
x-amz-trailer: x-amz-checksum-crc32
x-amz-decoded-content-length: <原始字节数>
Content-Length: <含分块框架的线上字节数>

b\r\n<11 字节负载>\r\n0\r\nx-amz-checksum-crc32:0D4A1185\r\n\r\n
```

Bridge 从不解码 aws-chunked 框架。全仓 grep `aws-chunked` / `x-amz-trailer` / `x-amz-checksum` 均无命中；`x-amz-content-sha256` 的 STREAMING-* 取值只在 `isIgnoredPayloadSHA256`（`pkg/handlers/object.go:421-428`）里被"忽略"，即跳过摘要校验，而不是触发解码。

由此产生两个并列故障，均已在本任务规划期用一次性测试在真实代码上复现：

**故障 A — 误报 QuotaExceeded（issue 描述的现象）**

`quotaMiddleware` 用 `x-amz-decoded-content-length`（解码后长度）作为 `size`，然后用它包裹**未解码的线上 body**：`r.Body = &quotaLimitReadCloser{remaining: size}`（`pkg/server/router.go:407`、`:423`）。分块框架使线上字节数 > 解码长度，读到超出部分时 `quotaLimitReadCloser.Read` 返回 `quota.ErrQuotaExceeded`（`:442`），`FileBackend` 把它冒泡给 `writeStorageError`，映射为 `QuotaExceeded`（`pkg/handlers/object.go:473-474`）。

实测：`403 QuotaExceeded`，对象未落盘。与报告完全一致。

**故障 B — 静默数据损坏（issue 未发现，比故障 A 更严重）**

若客户端不发 `x-amz-decoded-content-length`（部分 SDK/配置组合），`contentLengthForQuota` 回落到 `r.ContentLength`（`pkg/server/router.go:466`），即线上长度，没有任何字节被截断，分块头与 trailer 被原样写入对象。

实测：11 字节负载 `hello world` 存成 52 字节 `"b\r\nhello world\r\n0\r\nx-amz-checksum-crc32:0D4A1185\r\n\r\n"`，返回 **200 成功**。ETag 与 `used_bytes` 也都按损坏后的字节计算。

## Requirements

### R1 解码 aws-chunked 请求体

- 识别 aws-chunked 语义并把请求体替换为解码后的流，使下游处理器只看到原始对象字节。
- 判定条件必须同时覆盖：`x-amz-content-sha256` 为 `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` / `STREAMING-UNSIGNED-PAYLOAD-TRAILER` / `STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER`，以及 `Content-Encoding` 含 `aws-chunked`。二者任一成立即需解码。
- 支持带 `chunk-signature=` 的签名分块（本任务只解析并跳过分块签名，不验证；理由写入 design.md）。
- 支持 trailer 段，trailer 头不得进入对象字节。
- 长度截断必须由解码器自身完成，以 `x-amz-decoded-content-length` 为准（存在时）：超出报错，不足报错。

### R2 配额与统计以解码后长度为准

- 配额预检、预留、结算、`used_bytes`、`request_stats.bytes_in` 全部以解码后的真实对象字节数为准。
- `quotaLimitReadCloser` 不得再用解码后长度去截断未解码的流。

### R3 覆盖所有写入路径

- `PutObject`（`pkg/handlers/object.go:54`）
- `UploadPart`（`pkg/handlers/multipart.go:63`，当前直接把 `r.Body` 交给 `store.UploadPart`）
- 以上两条是 aws-chunked 可能出现的写入路径；`CompleteMultipartUpload` 与 `PutObjectTagging` 是 XML body，不走 aws-chunked，但不得因改动而破坏。

### R4 校验和处理

- `x-amz-trailer` 声明的 `x-amz-checksum-crc32` / `crc32c` / `sha1` / `sha256` 在 trailer 中出现时，若能校验则校验，不匹配返回 `BadDigest`；无法识别的算法忽略而不报错。
- 头部形式的 `x-amz-checksum-*`（非 trailer）不得导致失败。
- 既有 `Content-MD5` 与具体摘要形式的 `x-amz-content-sha256` 校验语义不得变化。

### R5 错误语义修正

- 分块框架格式错误（长度行非法、缺少 CRLF、chunk 长度与实际不符）→ `400 InvalidArgument` 或 `400 IncompleteBody`，**不得**再返回 `QuotaExceeded`。
- 真正超出配额仍返回 `403 QuotaExceeded`。
- 解码后长度与 `x-amz-decoded-content-length` 不一致 → 明确的 4xx，不得静默接受。

### R6 不改变既有对象与非 chunked 行为

- 普通（非 aws-chunked）PUT 的字节、ETag、sidecar、`used_bytes` 结果完全不变。
- 不改动 `FileBackend` 的写入语义与 sidecar 结构。
- 已落盘的历史对象不做任何迁移或改写。

## Acceptance Criteria

- [ ] 带 `x-amz-decoded-content-length` 的 aws-chunked PUT 返回 200，落盘字节 == 解码后负载，ETag == 负载的 MD5，`used_bytes` 增量 == 解码后长度
- [ ] **不带** `x-amz-decoded-content-length` 的 aws-chunked PUT 落盘字节同样 == 解码后负载（故障 B 关闭）
- [ ] 带 `chunk-signature=` 的多分块 PUT 正确解码，分块签名不进入对象字节
- [ ] trailer 头不进入对象字节
- [ ] trailer 中 CRC32 与实际负载不匹配 → `400 BadDigest`，且不留下对象、sidecar 或 `.tmp-*` 残留
- [ ] aws-chunked 的 `UploadPart` + `CompleteMultipartUpload` 全流程字节正确，`used_bytes` == 合并后对象大小
- [ ] 非法分块框架返回 `400`，响应码不是 `QuotaExceeded`
- [ ] 配额确实不足时仍返回 `403 QuotaExceeded`（既有 `TestRouterQuotaManagerConcurrentPutsCannotExceedQuota`、`TestRouterQuotaManagerUnderreportedPutPreservesObject`、`TestRouterQuotaManagerOverwriteSettlesNetGrowth` 保持通过）
- [ ] 非 chunked PUT 的既有测试全部保持通过，落盘字节与 ETag 不变
- [ ] `go build ./...`、`go vet ./...`、`go test -count=1 ./...` 全绿
- [ ] `git diff --check` 无输出
- [ ] `.trellis/spec/backend/auth-quota-guidelines.md` 与 `storage-guidelines.md` 补充 aws-chunked 契约（含"配额以解码后长度为准"与"框架错误不得报 QuotaExceeded"）

## 验证方式

无 boto3 环境时，用手工构造的 aws-chunked 请求覆盖上述断言即可。若能装 `boto3>=1.36`，追加一条真实 SDK 的 `put_object` 冒烟（这是报告者的原始场景）。

## Notes

- 实现代码由其他 AI 落盘。规划期的一次性 repro 测试已删除，未留在工作树；本任务需把等价断言固化为 `pkg/server` / `pkg/handlers` 下的正式测试。
- issue 中"配额在哪里配置/查看"的待确认项：面板 credential 表单的 `quota_bytes`（`pkg/panel/adminapi.go:367`、`pkg/webadmin/api.go:79`），0 = 无限。这一点在关闭 issue 的评论中说明即可，不需要代码改动。
