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
   至少覆盖：成功请求日志含 status；鉴权失败（无凭证 / 绑桶不匹配）有对应失败原因日志或可断言的日志字段行为（单测用可注入/可观察方式，避免脆弱的全局 logger 耦合；若现有测试模式已有更好做法则沿用）。

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

- [ ] 任意 S3 请求完成后的 access log 含 `status`（至少 2xx/4xx/5xx 正确）
- [ ] 无凭证访问 private 对象 → 403，且日志能看出 anonymous/unauthenticated denied
- [ ] 绑定桶密钥访问其他桶 → 403，且日志能看出 bound-bucket mismatch（含 bound 与 requested bucket，若可得）
- [ ] 验签失败 → 403，且日志含 `auth.ErrorCode` 对应码
- [ ] 合法签名 `HeadObject` 不存在 key → 404（不是 403），access log status=404
- [ ] 单测覆盖上述关键路径中至少「status 记录」+「绑桶不匹配或无凭证失败日志」
- [ ] 不泄露密钥材料

## Notes

- 相关排障结论：OpenList mkdir 依赖 HEAD 的 404；Force path style、bucket 与绑定一致、root=`/` 是客户端侧前提。
- 本任务交付后，用户可用 bridge 日志直接判断 403 原因，无需再猜。
- 若实现时发现 Logging 与 Auth 分层导致 request_id 关联困难，允许在 Auth 失败日志中读取已由 Logging 写入的 `x-amz-request-id` 响应头。
