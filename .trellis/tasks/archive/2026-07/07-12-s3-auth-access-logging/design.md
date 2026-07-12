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
- status int       // 初始化为 200，覆盖 handler 未显式 WriteHeader 的 net/http 语义
- wroteHeader bool // 仅记录第一次 WriteHeader，避免后续重复调用覆盖真实状态
- WriteHeader(code) 先锁定首次状态，再透传给底层 writer
- Unwrap() 返回底层 writer，保持 http.ResponseController 的可穿透性
```

仓库内没有对 `http.Flusher`、`http.Hijacker`、`http.Pusher` 或 `http.ResponseController` 的直接使用，因此本任务不人为宣称底层不支持的可选接口；小类型直接放在 `router.go`。

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
| 无凭证且请求不符合匿名对象读取条件 | `credentials_required` | `AccessDenied` |
| ACL lookup 不可用 | `acl_lookup_unavailable` | `AccessDenied`，保持现有响应语义 |
| ACL 查询错误 | `acl_lookup_failed` | `InternalError`；附加 `bucket`、`error` |
| 非 public-read / 桶不存在 | `anonymous_not_allowed` | `AccessDenied`；附加 `bucket`、`bucket_exists`、`acl` |

**禁止**：打印 `Authorization`、secret、完整 query 中的签名值。

实现使用一个包内 helper 统一输出 `s3 auth denied` 的基础字段：

```text
request_id = w.Header().Get("x-amz-request-id")
method = r.Method
path = r.URL.Path
reason, code
```

Verify 失败只记录 `auth.ErrorCode(err)`，不记录请求头、完整 URL、原始鉴权错误或 Identity 的 access key。ACL 查询失败可保留底层 `error`，但客户端响应仍仅为 `InternalError`。

## 中间件顺序与 request_id

当前：`Logging` 在 `Auth` 之外层，且 Logging 入口即设置 `x-amz-request-id`。  
Auth 失败日志读取该 header 即可与 access log 关联；access log 的 status 为 403。

## 测试策略

1. 扩展 `TestLoggingAddsRequestIDHeaderAndLogField`，断言 status；另覆盖显式 404、隐式 200 和重复 `WriteHeader` 仅保留首次状态。
2. 沿用现有 `slog.SetDefault(slog.NewJSONHandler(&buf, ...))` 测试模式；Auth 测试外包 `Logging`，同时断言 403/500 响应、`s3 auth denied` 字段与 access log status，并核对两条日志的 request_id 与响应头一致。
3. 表驱动覆盖 `verify_failed`、`credentials_required`、`anonymous_not_allowed`；扩展桶绑定测试或新增独立用例断言 `bucket_mismatch` 的两个桶字段。
4. 用现有 `newOpsTestRouter` + `headerSignedRequest` 发起 `HEAD /test-bucket/missing`，断言 404、`NoSuchKey` 语义及 access log `status=404`。
5. 在 Verify 失败请求中放入唯一 Authorization sentinel，并断言日志不包含 sentinel、`Authorization`、`X-Amz-Signature` 或 secret sentinel。

## Panic Recovery Boundary

当前顺序是 `Recover → Logging → ...`。若下游 panic，栈展开时 Logging defer 先运行，Recover 后写 500，因此单纯 wrapper 无法让该条 access log 得到 500。推荐为本任务保持现有顺序与行为，把验收聚焦正常返回的 2xx/4xx/5xx（尤其 403/404）；若评审要求 recovered panic 也必须准确，则将链路改为 `Logging → Recover → ...` 并新增 panic status 测试，代价是 Logging 自身不再受 Recover 保护。

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
- 默认不改变中间件顺序；若评审选择覆盖 recovered panic，则顺序调整需作为显式兼容性变更测试。
