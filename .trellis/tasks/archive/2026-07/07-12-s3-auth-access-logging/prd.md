# 增强 S3 访问与鉴权失败日志

## Goal

让 S3 访问日志足以诊断「上传成功、建目录 HEAD 403」这类问题：每次请求能看到 HTTP 状态，鉴权失败时能看到失败原因（无凭证 / 验签失败码 / 绑桶不匹配），无需抓包或猜 request id。

## Background

OpenList 等客户端建目录会先 `HeadObject` 检查路径；对象不存在应返回 **404**，客户端继续写占位对象。当前若返回 **403**，客户端直接失败。

现有 `Logging` 中间件只记录：

- `request_id` / `method` / `path` / `elapsed`

不记录：

- HTTP status
- 鉴权失败原因（`AccessDenied` / `SignatureDoesNotMatch` / `InvalidAccessKeyId` / `RequestTimeTooSkewed` / bound-bucket mismatch）
- 是否带凭证、绑定桶与请求桶是否一致

因此同一行 `s3 request method=HEAD path=/library/2` 无法区分 404 与 403，排障成本高。

本地用绑定桶密钥复现 OpenList mkdir 链路：`HEAD /library/2` → 404、`PUT .../.openlist` → 200 均正常；403 仅在「无签名」或「请求桶 ≠ 绑定桶」时出现。问题侧重点是可观测性，不是 mkdir 语义本身。

## Requirements

1. **访问日志带状态码**  
   每条 S3 请求结束日志至少包含：`request_id`、`method`、`path`、`status`、`elapsed`。

2. **鉴权失败可诊断**  
   在 Auth 拒绝请求时写一条明确日志（level 建议 `WARN` 或 `INFO`，与现有风格一致即可），至少区分：
   - 无凭证（anonymous denied）
   - 验签/鉴权错误码（使用 `auth.ErrorCode`，如 `SignatureDoesNotMatch`、`InvalidAccessKeyId`、`AccessDenied`、`RequestTimeTooSkewed`）
   - 密钥绑定桶与请求 path 中的 bucket 不一致（bound bucket mismatch）
   - public-read 校验失败（桶不存在 / ACL 非 public-read）时的简要原因  

   日志须带 `request_id`（或与 access log 可关联的字段）及 `path`/`method`；**不得**打印 secret key 或完整 `Authorization` 头。

3. **行为不变**  
   仅增强日志与必要的 response writer 包装；S3 API 状态码、错误 XML、鉴权判定逻辑保持不变。

4. **可测**  
   至少覆盖：显式 404 的 access log status；鉴权失败（验签失败 / 无凭证 / 绑桶不匹配 / 匿名桶非 public-read）的 reason/code；合法签名 HEAD 缺失 key 的 404 链路；敏感凭证值不进入日志。沿用 `pkg/server/router_test.go` 已有的临时替换 `slog.Default()` + JSON handler 模式，并在 cleanup 中恢复。

## Confirmed Facts

- 当前链路由 `newRouter` 固定为 `Recover → Logging → AnonRateLimit → Auth → Quota → dispatch`；`Logging` 已在调用下游前设置 `x-amz-request-id`，Auth 可直接从响应头读取同一 ID。
- `Logging` 当前仅记录 `request_id`、`method`、`path`、`elapsed`，没有 status；现有测试已能捕获并断言 JSON slog 字段。
- `Auth` 的拒绝分支集中在 `pkg/server/router.go`：Verify 错误、绑定桶不匹配、请求不满足匿名对象读取条件、ACL 查询错误、桶不存在或 ACL 非 `public-read`。
- `auth.ErrorCode(err)` 已提供稳定的 S3 错误码；无需记录原始 Authorization 或解析后的签名材料。
- `handlers.WriteS3Error` 复用响应头中的 `x-amz-request-id`；`ObjectHandler.Head` 将 `storage.ErrNoSuchKey` 翻译为 HTTP 404 `NoSuchKey`。
- access/auth 日志使用 `r.URL.Path` 而非 `r.URL.String()` 或 `RawQuery`，因此不会把预签名 query 中的 `X-Amz-Signature` 写入日志。

## Out of Scope

- OpenList 客户端适配或文档站大改（可在 Notes 留一句对接建议）
- 结构化日志后端（ELK 等）接入
- 改变 403/404 语义或兼容「目录对象」尾斜杠特殊行为（另任务）
- 管理端 UI 展示访问日志

## Constraints

- 不记录 `secret_key`、完整签名头、明文密码
- 保持现有 slog 文本/JSON 风格，字段名简洁稳定
- 性能：仅包装 `ResponseWriter` 记录 status，避免大对象拷贝
- 兼容现有中间件顺序：`Recover → Logging → AnonRateLimit → Auth → Quota → dispatch`

## Acceptance Criteria

- [ ] 正常完成的 S3 请求 access log 含 `status`，并与首次写出的 HTTP status 一致；未显式 `WriteHeader` 时按 net/http 语义记录 200
- [ ] 无凭证访问 private 对象 → 403，且 `s3 auth denied` 含 `reason=anonymous_not_allowed`、`code=AccessDenied`
- [ ] 无凭证执行非匿名对象读取操作 → 403，且日志含 `reason=credentials_required`、`code=AccessDenied`
- [ ] 绑定桶密钥访问其他桶 → 403，且日志含 `reason=bucket_mismatch`、`code=AccessDenied`、`bound_bucket`、`request_bucket`
- [ ] 验签失败 → 403，且日志含 `reason=verify_failed` 及 `auth.ErrorCode` 对应 `code`
- [ ] ACL 查询失败 → 500，且日志含 `reason=acl_lookup_failed`、`code=InternalError`
- [ ] 合法签名 `HeadObject` 不存在 key → 404（不是 403），access log status=404
- [ ] 单测覆盖 status、关键 Auth reason/code、request_id 关联及签名 HEAD 缺失 key 链路
- [ ] 日志不包含 secret、完整 Authorization 或预签名 query/signature

## Open Question

- 当前 `Recover` 位于 `Logging` 外层；panic 展开时 `Logging` 的 defer 会先记录默认 status，随后 `Recover` 才写 500。是否为保证 recovered panic 也记录 status=500 而调整中间件顺序，仍需评审确认；本任务核心 403/404 路径不受该边界影响。

## Notes

- 相关排障结论：OpenList mkdir 依赖 HEAD 的 404；Force path style、bucket 与绑定一致、root=`/` 是客户端侧前提。
- 本任务交付后，用户可用 bridge 日志直接判断 403 原因，无需再猜。
- 若实现时发现 Logging 与 Auth 分层导致 request_id 关联困难，允许在 Auth 失败日志中读取已由 Logging 写入的 `x-amz-request-id` 响应头。
- 日志目录化和管理后台历史文件选择已拆分为独立任务 `.trellis/tasks/07-12-log-directory-history`，不属于本任务改动范围。
