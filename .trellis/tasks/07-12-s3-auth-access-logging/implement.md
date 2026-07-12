# Implement: S3 访问与鉴权失败日志

## Checklist

1. [ ] 在 `pkg/server/router.go` 实现 `statusResponseWriter`（或等价），`Logging` defer 输出 `status`
2. [ ] 在 `Auth` 各拒绝分支补充 `slog` 诊断字段（`reason`/`code`/`request_id`/桶信息），不改响应语义
3. [ ] 单测：Logging 记录/透传 status；Auth 无凭证与绑桶不匹配仍 403；可选断言日志含 reason
4. [ ] `go test ./pkg/server/ ./pkg/auth/ ...` 相关包通过
5. [ ] 本地手测（可选）：临时起实例，匿名 HEAD private → 日志 status=403 + reason；签名 HEAD 缺失 key → status=404

## Validation

```bash
go test ./pkg/server/ -count=1
go test ./pkg/auth/ -count=1
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
- 中间件顺序未打乱

## Rollback

还原 `pkg/server/router.go`（及新增测试文件）即可。
