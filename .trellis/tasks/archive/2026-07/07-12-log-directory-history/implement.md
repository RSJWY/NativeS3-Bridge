# Implement: 日志目录化与历史文件管理

## Checklist

1. [ ] 扩展 `config.LogConfig`：增加 `dir`、互斥校验和 effective active-file helper
2. [ ] 更新 `setupSlog` 与 server options，统一传递 effective active file
3. [ ] 在 `pkg/webadmin/logs.go` 实现安全文件枚举、选择校验及 gzip reader
4. [ ] 扩展 `/api/admin/logs` 响应和 `file` 参数，保留默认 current/ring 行为
5. [ ] 更新 `src/api/client.ts` 类型与请求参数
6. [ ] 更新 `Logs.vue` 文件选择器、错误状态和历史文件展示
7. [ ] 补配置、初始化、API、安全、gzip 和 UI 构建验证
8. [ ] 更新 README 配置与兼容说明

## Affected Files

- `pkg/config/config.go`, `pkg/config/config_test.go`
- `cmd/natives3bridge/main.go`, `cmd/natives3bridge/main_test.go`
- `pkg/webadmin/server.go`, `pkg/webadmin/api.go`, `pkg/webadmin/logs.go`, `pkg/webadmin/logs_reconcile_test.go`
- `pkg/webadmin/ui/src/api/client.ts`, `pkg/webadmin/ui/src/views/Logs.vue`, 必要时 `src/styles.css`
- `README.md`

## Validation

```bash
gofmt -w pkg/config/config.go pkg/config/config_test.go cmd/natives3bridge/main.go cmd/natives3bridge/main_test.go pkg/webadmin/server.go pkg/webadmin/api.go pkg/webadmin/logs.go pkg/webadmin/logs_reconcile_test.go
go test ./pkg/config/ ./pkg/logging/ ./pkg/webadmin/ ./cmd/natives3bridge/ -count=1
cd pkg/webadmin/ui && npm run build
```

随后按仓库情况运行更广验证：

```bash
go test ./...
```

## Risk And Rollback Points

- 路径安全：任何 file id 到磁盘路径的转换都必须重新匹配 allowlist，并测试 traversal/symlink。
- 轮转竞态：枚举后文件可能被删除；显式历史选择必须返回错误，不能回退并展示错误文件。
- 配置兼容：旧 `log.file` 必须继续工作；`log.dir` 与 `log.file` 冲突必须在启动前失败。
- 嵌入前端：确认 Vite 构建输出符合 `go:embed dist` 的项目流程。

## Dependencies

- 与 `.trellis/tasks/07-12-s3-auth-access-logging` 无代码依赖；按用户指定拆分为独立任务，建议在前一任务完成后实施。
