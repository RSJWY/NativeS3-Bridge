# Panel 自身日志查看与轮转统一

## Goal

让 Panel 管理员能在 Panel UI 查看 Panel 自身 ring、当前日志和安全枚举的轮转历史，并统一 Panel、Node、旧 standalone 的日志初始化与轮转行为。旧 standalone 只保留兼容回归，不新增产品 UI/API。

## Confirmed facts

- `cmd/panel/main.go`、`cmd/node/main.go`、`cmd/natives3bridge/main.go` 各自复制了一份 `setupSlog`。
- Panel 已创建 ring/file，但 `AdminServer` 未接收它们，也未注册 `/api/admin/logs`；standalone `pkg/webadmin.Server` 已有完整日志 API。
- `pkg/webadmin/logs.go` 已具备当前/历史文件、gzip、过滤、ring fallback 和路径/symlink 防护，可抽成 Panel 可复用 handler。
- 正式发布只构建 Panel/Node；standalone 仅需保持 `go test ./...` 和升级回滚脚本兼容。

## Requirements

1. 提取或封装可复用的日志初始化/查看能力，避免 Panel 与 standalone 使用不同的敏感字段过滤和轮转语义。
2. Panel 将有效日志文件路径和 ring 注入 `AdminServer`，注册 session-protected `GET /api/admin/logs`。
3. Panel 模式允许 `/logs` 路由并在导航显示“Panel 日志”；standalone 的现有 `/logs` 行为不变。
4. 验证 `log.dir`、旧 `log.file`、大小/备份/年龄/gzip、目录权限、异步 pruning、已有大文件启动行为，并保持 stdout + ring。
5. 更新配置/运维文档和后端/前端日志契约规范；所有输出和测试证据脱敏。

## Acceptance Criteria

- [x] 登录 Panel 后 `/logs` 可查看自身 ring/当前文件/轮转历史，并支持既有过滤器。
- [x] 未登录 Panel 日志 API 返回 401；路径穿越、symlink、非匹配文件和轮转竞态不会泄露任意文件。
- [x] Panel/Node/standalone 的日志初始化契约和轮转边界有单测；stdout、ring、文件输出保持一致。
- [x] `log.dir`/`log.file` 兼容与非法参数行为有回归；旧 standalone 编译和升级回滚测试不回归。
- [x] 相关 Go 测试、`go vet ./...`、`go build ./...`、前端生产构建通过。

## Dependency and out of scope

- 无实现前置依赖；Node child 可并行开发协议，但 parent 的最终集成验证应在本 child 完成后运行。
- 不实现 Node 远程日志查询、实时流、日志下载/删除或动态改级别。
