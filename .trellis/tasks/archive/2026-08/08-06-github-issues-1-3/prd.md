# 修复 GitHub issues #1-#3（父任务）

## Goal

聚合 https://github.com/RSJWY/NativeS3-Bridge/issues 上三个开放 issue 的需求集、任务映射与跨子任务验收标准。父任务本身不承担实现工作，只负责需求归口与最终集成验收。

## 来源需求集

| issue | 标题 | 标签 | 子任务 |
|---|---|---|---|
| #2 | PutObject 返回 QuotaExceeded，但底层磁盘空间充足 | bug | `08-06-fix-aws-chunked-upload` |
| #3 | S3 v2 签名下含非 ASCII 字符的 key 验签失败 | — | `08-06-implement-sigv2-auth` |
| #1 | 增加面板直接查看节点配置的区域 | enhancement | `08-06-panel-node-config-view` |

## 实测复现结论（2026-08-06，先于任何实现）

在真实代码上跑一次性 repro 测试得到的结论，纠正了 issue 原文中的若干猜测：

1. **#2 与配额配置无关。** `quota_bytes = 0` 表示无限（`pkg/quota/quota.go:59`、`:70`），报告者的配额并没有配错。真实原因是 boto3 ≥ 1.36 默认发送 `Content-Encoding: aws-chunked` + `x-amz-content-sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER`，而 Bridge 从不解码 aws-chunked 分块框架。由此产生两个并列故障：
   - 带 `x-amz-decoded-content-length` 时，`quotaLimitReadCloser`（`pkg/server/router.go:431-452`）按解码后长度截断线上 body，多出的分块头字节触发 `ErrQuotaExceeded`，被 `writeStorageError` 映射为 `QuotaExceeded`（`pkg/handlers/object.go:473`）。实测：403 QuotaExceeded，对象未落盘。
   - 不带该头时无任何长度上限，分块头与 trailer 被原样写入对象。实测：11 字节负载存成 52 字节 `"b\r\nhello world\r\n0\r\nx-amz-checksum-crc32:0D4A1185\r\n\r\n"`，返回 200。**这是静默数据损坏，比报错更严重。**
2. **#3 与非 ASCII 无关。** SigV2 完全未实现——`ParseAuthorization`（`pkg/auth/sigv4.go:34`）要求 `Authorization` 以 `AWS4-HMAC-SHA256 ` 开头，否则直接 `SignatureDoesNotMatch`。实测 ASCII key 与中文 key 的 v2 请求都失败，报告者「v2 + 纯 ASCII key 正常」的观察无法在代码层成立（很可能其客户端在该场景实际回落到了 v4）。
3. **#1 需要新增读端点。** 目前没有任何端点返回节点配置内容：`nodeResponse`（`pkg/panel/adminapi.go:64-77`）只有版本/哈希元数据，`desiredStateRoute`（`:544`）只接受 POST，`DesiredConfig.ContentJSON` 从未序列化进任何 HTTP 响应。

## 跨子任务约束

- **红线：secret 不得新增暴露面。** `secret_key` 只允许出现在 credential 创建与轮转响应中（`.trellis/spec/backend/panel-authoritative-config-guidelines.md:96`）。#1 的新端点既不得返回 `secret_key`，也不得返回 `secret_key_cipher` 或原始 `ContentJSON`。
- **红线：不得改变已落盘对象字节。** #2 的解码改动只能作用于请求体解析，不得改变 `FileBackend` 的写入语义、sidecar 结构或既有对象。
- **红线：不得放宽既有 SigV4 行为。** #3 新增 v2 路径必须与 v4 路径完全隔离，v4 的现有测试全部保持通过。
- 执行顺序：#2 → #3 → #1。#2 是数据损坏级别，优先；#3 与 #1 相互独立，#1 涉及前端构建产物，放最后减少 `dist/` 冲突。
- 三个子任务共享同一套质量门禁（见下）。

## Acceptance Criteria（父任务集成验收）

- [ ] 三个子任务均已通过各自 `prd.md` 的验收标准并归档
- [ ] `go build ./...`、`go vet ./...`、`go test -count=1 ./...` 全绿
- [ ] `pkg/webadmin/ui` 下 `npm ci && npm run build` 通过（含 `vue-tsc --noEmit`）
- [ ] `git diff --check` 无空白错误
- [ ] 全仓 grep 确认响应结构体中不存在新增的 `secret_key` / `secret_key_cipher` 输出路径
- [ ] 三个 issue 均在 GitHub 上以「修复内容 + 验证方式」评论关闭

## Notes

- 实现代码由其他 AI 落盘；本会话负责规划与验收，不写最终修复代码。
- 复现用的一次性测试已删除，未留在工作树中。子任务需把等价断言固化为正式测试。
