# Panel/Node 日志拉取、查看与滚动完善

## Goal

让管理员在 Panel 中同时看到两类日志：Panel 自身运行日志，以及通过 mTLS 控制面从指定 Node 拉取的诊断日志；同时确认 Panel 和 Node 的日志落盘、轮转、保留和安全过滤行为在生产配置下可用。日志查看必须是只读、受管理员会话保护，并且不能把控制面变成任意命令或无限制日志传输通道。

本任务的正式运行时范围是当前 Panel/Node 双进程部署。旧 `cmd/natives3bridge` 单体不新增日志功能，仅保留编译、测试和升级回滚兼容。

## User value

- Panel 故障、节点注册/同步失败和 S3 请求异常可以在管理页面定位，不再依赖分别登录 Panel/Node 主机执行 `docker logs`。
- Node 离线、任务超时、日志文件被轮转清理等状态在页面上有明确反馈。
- 日志文件达到大小上限时继续安全轮转，stdout、内存 ring 和历史文件查看不会互相破坏。

## Confirmed facts from repository audit

### Panel 自身日志

- `cmd/panel/main.go:202-238` 已创建 stdout + 可选 lumberjack 文件 + 内存 ring，支持 `log.dir`/旧 `log.file`、`max_size_mb`、`max_backups`、`max_age_days`、`compress`。
- `pkg/config/panel.go:152-157` 已校验 `log.dir` 与 `log.file` 互斥以及启用文件日志时的大小约束；Panel/Node 配置默认值均为 100 MB、5 个备份、0 天保留限制。
- `cmd/panel/main.go:111-121` 没有把 `setupSlog` 返回的 ring/file 传给 `panel.AdminServer`。
- `pkg/panel/adminserver.go:45-66` 只挂载登录和 `/api/admin/nodes*`，没有挂载 `/api/admin/logs`；因此 Panel 模式的 `/logs` 页面无法读取自身日志。
- 同一份 SPA 在 Panel 模式主动隐藏 `/logs`（`pkg/webadmin/ui/src/App.vue:27-36`），这是已有服务模式隔离的结果，不是反向代理路径问题。

### Node 日志与控制面

- `cmd/node/main.go:231-268` 已创建 Node stdout + 可选轮转文件 + 内存 ring，并将 ring注入 `nodeagent.LocalTaskRunner`。
- `controlproto.TaskLogQuery`、Panel `TaskOrchestrator`、`POST/GET /api/admin/nodes/{id}/tasks...` 已存在（`pkg/controlproto/payloads.go:83-145`、`pkg/panel/tasks.go`、`pkg/panel/adminapi.go:623-674`）。
- `pkg/nodeagent/tasks.go:58-87` 的 `log_query` 只查询内存 ring，只返回格式化字符串；`since`/`until`/级别过滤未生效，且没有读取 Node 当前或轮转文件。
- Panel 前端 `src/api/client.ts` 和 `PanelNodeDetail.vue` 没有任务 dispatch/poll 类型、方法或日志展示区，因此现有 Node 日志任务没有用户可达入口。
- 控制面已有任务并发上限、超时、断线状态和结果持久化机制；设计约束是不开放任意 shell，也不把控制连接当作无限制 bulk log stream。

### 已有自身日志查看实现与轮转证据

- `pkg/webadmin/logs.go` 已实现管理员保护的日志 API、ring fallback、当前/历史 lumberjack 文件枚举、gzip 历史读取、level/query/limit 过滤和路径/symlink 防护；`pkg/webadmin/server.go:53-70` 已挂载该 API。
- `pkg/logging/ring.go`/`handler.go` 已实现线程安全 ring（默认 2000 条）及敏感属性键过滤；`pkg/db/db.go` 的 GORM SQL 字面量会脱敏。
- 现有轮转测试覆盖通用 lumberjack 和 standalone `cmd/natives3bridge` 初始化，但没有 Panel/Node 二进制各自的初始化、轮转、历史读取和控制面展示验证。
- 当前相关包基线测试和前端构建均通过：`go test ./pkg/config ./pkg/logging ./pkg/webadmin ./pkg/panel ./pkg/nodeagent`、`go test ./cmd/panel ./cmd/node`、`npm run build --prefix pkg/webadmin/ui`。

## Requirements

### R1. Panel 自身日志查看

- Panel 模式导航提供“Panel 日志”入口，复用现有只读日志查看能力和过滤器。
- Panel 新增受 session middleware 保护的 `/api/admin/logs`；无日志文件时返回 ring，有日志文件时优先读取当前文件并列出/选择安全的轮转历史（包括 gzip），读取失败时按既有兼容规则回退或返回明确错误。
- API/UI 不允许客户端提交任意路径；不展示 secret、token、Authorization、Cookie、SQL 字面量或对象内容。

### R2. Node 日志拉取

- Node 详情页提供“拉取日志”操作、级别/关键字/条数过滤、加载中/空结果/失败/超时/节点离线/结果截断状态。
- 使用现有预定义 `log_query` 任务和 mTLS 控制面；不得新增任意命令执行或未经限制的日志流。
- Panel API/client/UI 必须以类型化契约消费任务状态和结果；任务结果不得把敏感材料写入 Panel 的持久日志或审计记录。
- Node 端必须正确执行请求的时间范围（如保留该参数）、级别和关键字过滤，并返回可稳定渲染的时间、级别、消息和截断信息，而不是让 UI 重新解析非结构化字符串。
- 第一阶段只通过控制面拉取 Node 当前内存 ring；Node 本地文件及轮转历史仍由 Node 自身维护和验证，不通过控制面传输原始历史文件。

### R3. Panel/Node 日志初始化与轮转审计

- Panel、Node 和旧 standalone 三条启动路径都保持 stdout 输出；启用文件时同时写入内存 ring 和 lumberjack 文件。
- 明确并测试 `log.dir` 与旧 `log.file` 兼容规则、默认值、非法轮转参数、父目录/权限失败、`max_backups: 0`、gzip、按大小轮转、按天清理和异步备份修剪。
- 轮转后当前文件与历史文件的命名、排序、读取和清理竞态有可验证行为；启动时不能因为已有大文件而产生未声明的额外轮转副作用。
- Panel 自身日志和 Node 拉取日志均显示来源（ring/current/history/remote）及必要的告警，避免把“控制面离线”误报成“没有日志”。

### R4. 文档、回归与运维安全

- 更新配置示例、README/多节点运维文档和后端/前端可执行规范，说明 Panel 日志路径、Node 日志拉取边界、轮转参数和 Docker 卷挂载。
- 增加后端、协议、Node runner、Panel API、前端类型/构建及至少一条真实 Panel↔Node 控制面日志拉取回归。
- 测试与失败诊断必须脱敏；不得持久化注册 token、S3 secret、私钥、session cookie、完整签名 URL 或日志查询中的敏感字段。

## Acceptance Criteria

- [x] 登录 Panel 后可在“Panel 日志”页面查看自身 ring/当前文件/轮转历史，并使用级别、关键字、条数过滤。
- [x] 登录 Panel 后进入某 Node 详情，在线 Node 可发起受限日志查询并看到结构化结果、截断标记和任务状态；Node 离线/超时/失败有可读错误，不能调用任意命令。
- [x] Node 查询确实读取 Node 本地日志能力并应用约定的过滤参数；控制面断线不会影响 Node S3 数据面。
- [x] Panel、Node、standalone 的 stdout + ring + 可选文件行为一致；大小轮转、备份上限、gzip/按日清理及异步修剪均有稳定测试证据。
- [x] `log.dir`/`log.file` 兼容、非法配置和文件权限错误在启动前或初始化时给出明确结果；历史文件路径穿越、symlink 和轮转竞态不能泄露任意文件。
- [x] 全部日志查看 API 仍受管理员会话保护，响应、任务结果、审计和测试产物不含敏感材料。
- [x] 相关 Go 测试、`go vet ./...`、`go build ./...`、前端生产构建和控制面回归通过。

## Scope decisions

- 正式功能只覆盖 Panel/Node；standalone 只做编译、既有测试和升级回滚兼容验证。
- Panel 自身日志支持当前文件、ring 和安全选择的 lumberjack 轮转历史。
- Node 远程日志第一阶段只支持受限的当前内存 ring，不经控制面传输轮转历史原始文件；Node 本地轮转仍必须完整验证。

## Delivery map

- `07-25-panel-log-viewer-rotation`：公共日志初始化/轮转契约、Panel 自身日志 API/UI、Panel/Node 初始化验证。
- `07-25-node-log-query`：`log_query` 结构化协议、Node ring 查询、Panel 类型化任务 API、Node 日志 UI。
- 两个 child 都完成后，由本 parent 做跨层集成回归（真实 Panel↔Node mTLS 查询、脱敏、完整构建与测试）。Child 间依赖和集成顺序写在各自 PRD 中，不依赖目录顺序。

## Out of scope unless explicitly added

- 实时 WebSocket 日志流、全文索引、日志数据库、ELK/Loki 外发。
- 通过 Panel 修改、删除、下载原始日志文件或动态修改日志级别。
- 跨多个 Node 的聚合检索和批量导出。
- 任意 shell/脚本执行。
