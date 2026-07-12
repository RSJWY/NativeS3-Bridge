# Implement: S3 访问与鉴权失败日志

## Checklist

1. [ ] 在 `pkg/server/router.go` 实现小型 `statusResponseWriter`：默认 200、首次 `WriteHeader` 生效、提供 `Unwrap`
2. [ ] `Logging` 将 wrapper 传给下游，并在现有 access log 增加 `status`
3. [ ] 增加统一的 `s3 auth denied` 日志 helper，基础字段固定为 `request_id`/`method`/`path`/`reason`/`code`
4. [ ] 拆分并覆盖 Auth 的 Verify、桶绑定、无凭证、ACL lookup、匿名非 public-read 拒绝分支，保持原 HTTP/XML 语义
5. [ ] 扩展 `pkg/server/router_test.go`：status writer、Auth reason/code/桶字段/request_id、安全字段、合法签名 HEAD missing key 404
6. [ ] 执行 gofmt 和目标测试；仅修复本任务引入的问题
7. [ ] 本地手测（可选）：匿名 HEAD private → status=403 + reason；签名 HEAD 缺失 key → status=404

## Validation

```bash
gofmt -w pkg/server/router.go pkg/server/router_test.go
go test ./pkg/server/ ./pkg/auth/ -count=1
```

手测要点：

```bash
# 匿名 → 403 + credentials_required / anonymous_not_allowed
curl -sI "http://127.0.0.1:9000/library/2"

# 合法 AK/SK head 不存在 → 404
aws --endpoint-url http://127.0.0.1:9000 s3api head-object --bucket library --key missing
```

## Review gates

- 无密钥/Authorization 泄露
- 404 与 403 在 access log 可区分
- Auth denied 与 access log 的 `request_id` 和响应头一致
- `statusResponseWriter` 记录首次状态且不破坏底层 writer 透传
- 中间件顺序未打乱

## Pre-start Decision

- 默认方案：保持 `Recover → Logging`，不把 recovered panic 的 status 精确性纳入本任务。
- 若评审要求 panic access log 也必须为 500：改为 `Logging → Recover`，同步更新 review gate 并增加 panic 单测后再 `task.py start`。

## Rollback

还原 `pkg/server/router.go`（及新增测试文件）即可。
