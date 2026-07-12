# Design: S3 访问与鉴权失败日志

## Approach

在现有中间件链路上做最小增强：

1. **Logging**：用 status-capturing `ResponseWriter` 包装，在 defer 中输出 `status`。
2. **Auth**：各 403 分支增加结构化 slog（含 `reason` 或 `code`），不改变返回的 `WriteS3Error` 行为。

不引入新配置项（默认开启）；若后续要降噪再加 `log_auth_failures` 一类开关。

## Key files

| 文件 | 变更 |
|---|---|
| `pkg/server/router.go` | `Logging` 记录 status；`Auth` 失败分支打诊断日志 |
| `pkg/server/router_test.go` | 覆盖 status 捕获与鉴权失败路径（沿用现有 httptest + stub authenticator） |
| 可选 `pkg/server/logging_response_writer.go` | 若 `router.go` 过长则抽出 writer 类型；否则内联小类型即可 |

## Logging 设计

```text
statusResponseWriter embeds http.ResponseWriter
- status int  // 默认 200（若 handler 未 WriteHeader 则按 net/http 惯例）
- WriteHeader(code) 记录 code
- 若需兼容 flush/hijack：按现有依赖决定是否 forward；S3 路径通常不需要 Hijacker
```

access log 字段：

```text
msg="s3 request"
request_id, method, path, status, elapsed
```

可选增强（非必须）：`bytes` 若易得则记；本任务不做 body 计量。

## Auth 失败日志设计

统一字段建议：

```text
msg="s3 auth denied"
request_id  // 从 w.Header().Get("x-amz-request-id")，Logging 已先设置
method, path
reason      // 稳定短字符串
code        // S3/auth 错误码，与响应 XML Code 一致时优先
```

| 场景 | reason 建议值 | code |
|---|---|---|
| Verify 失败 | `verify_failed` | `auth.ErrorCode(err)` |
| 绑桶不匹配 | `bucket_mismatch` | `AccessDenied`；附加 `bound_bucket`、`request_bucket` |
| 无凭证且非匿名可读 | `credentials_required` | `AccessDenied` |
| ACL 查询错误 | `acl_lookup_error` | `InternalError`（已有 Error 级可保留，避免重复可只留一处） |
| 非 public-read / 桶不存在 | `anonymous_not_allowed` | `AccessDenied`；可选 `bucket`、`acl`/`exists` |

**禁止**：打印 `Authorization`、secret、完整 query 中的签名值。

## 中间件顺序与 request_id

当前：`Logging` 在 `Auth` 之外层，且 Logging 入口即设置 `x-amz-request-id`。  
Auth 失败日志读取该 header 即可与 access log 关联；access log 的 status 为 403。

## 测试策略

1. 扩展或新增 `TestLogging*` / 在现有 Auth 测试中注入可观察 logger 困难时：  
   - 优先断言 **HTTP 行为不变**；  
   - 对 Logging：用自定义 handler 写 403/404，经 Logging 包装后检查 recorder 与（若用 slog test handler）日志属性。  
2. 若项目尚无 slog test 工具，可采用 `slog.New(slog.NewTextHandler(&buf, nil))` 临时替换默认 logger 的模式（测完恢复），或仅测 `statusResponseWriter` 单元行为 + Auth 仍测 status code。  
3. 绑桶不匹配：沿用 `TestAuthBoundCredential...` 路径，确认仍为 403。

## Tradeoffs

| 方案 | 利 | 弊 |
|---|---|---|
| A. 只改 Logging + Auth（选用） | 改动面小、立刻可排障 | 无采样/配置开关 |
| B. 独立 audit middleware | 更干净 | 多层重复包 ResponseWriter |
| C. 改 WriteS3Error 统一打日志 | 覆盖所有 S3 错误 | 噪音大、非鉴权 404 也刷屏 |

选用 A：鉴权失败单独打一条，access log 统一带 status。

## Compatibility / Rollback

- 纯日志变更，回滚即还原 `router.go` 相关片段。
- 日志字段新增不破坏 API。
