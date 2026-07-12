# 日志落盘与管理端日志查看

## Goal

为 NativeS3-Bridge 增加**可选日志落盘（带标准轮转）**，并在管理后台提供**只读日志查看页**，便于运维在页面上查看最近运行/访问日志，而不必只靠 `docker logs` / journal。

本任务**仅规划、不实现**；后续准备开发时再 `task.py start`。

## Background / Problem

- 现状：`setupSlog` 使用 `slog.NewTextHandler(os.Stdout, ...)`，配置仅有 `log_level`。
- S3 访问日志：`Logging` 中间件 `slog.Info("s3 request", request_id, method, path, elapsed)` → 同样 stdout。
- 无 `log_file`、无轮转、无管理端日志 API/UI。
- `request_stats` / `/metrics` 是聚合计数，不是逐条日志；`06-06-observability` 明确不做管理端日志 UI。
- 用户需要：落盘 + 单文件大小上限 + 保留文件数量上限（标准轮转）+ 管理页查看。

## Confirmed Facts

- 配置：`pkg/config/config.go` → `LogLevel string \`yaml:"log_level"\``；无 log 文件段。
- 示例配置：`configs/config.example.yaml` 等仅 `log_level: "info"`。
- 启动：`cmd/natives3bridge/main.go` → `setupSlog(cfg.LogLevel)`；`handler := slog.NewTextHandler(os.Stdout, ...)`。
- 管理端路由：`pkg/webadmin/server.go` 会话保护 `/api/admin/*`；侧栏仅仪表盘/密钥/桶。
- 公开 ops：`/healthz` `/readyz` `/metrics` — **不得**挂日志查看。
- `go.mod` 当前无 lumberjack 依赖。
- Docker 常见挂载：`data` + `state`；日志应落在 **state** 侧，不写 `data_root` 对象目录。

## Decisions

| 决策 | 选择 | 说明 |
|------|------|------|
| 默认是否落盘 | **否**（`log.file` 空 = 仅 stdout） | 兼容现网 Docker/journal |
| stdout | **始终保留** | `MultiWriter(stdout, file?)` |
| 轮转库 | **`gopkg.in/natefinch/lumberjack.v2`** | MaxSize / MaxBackups / MaxAge / Compress |
| 路径 | **state 卷**，禁止默认写 `data_root` | 例：`/state/logs/natives3bridge.log` |
| 打开文件失败 | **fail-fast 启动失败** | 配置了 file 却打不开则不带病运行 |
| 管理页数据源 | **内存 ring 始终写；有落盘时优先 tail 文件** | 无 file 时页面仍可看最近 ring；有 file 时 tail 为主、失败回退 ring |
| 日志入库 | **不做** | 不写 DB 日志表 |
| 权限 | **仅管理员会话** | 401；不进公开 ops |
| 敏感信息 | **禁止** secret_key / 密码 / 对象 body | 沿用现有 slog 字段；ring 过滤敏感 attr key |
| 与 storage-reconcile | **独立任务** | 对账只规划，不塞日志实现 |

## Requirements

### R1. 可选落盘 + 轮转

配置示例：

```yaml
log_level: "info"
log:
  file: ""                 # 空 = 不落盘
  # file: "/state/logs/natives3bridge.log"
  max_size_mb: 100         # 单文件上限（MB）
  max_backups: 5           # 历史文件数量上限（不含当前）
  max_age_days: 14         # 0 = 不按天删
  compress: false          # 旧文件是否 gzip
```

- `file` 非空：stdout + 轮转文件双写；所有 `slog`（含 S3 access、GORM via slog、启动日志）进同一流。
- 轮转满足：单文件大小上限、保留个数上限；可选按天清理、压缩。
- `file` 为空：行为与现网一致（仅 stdout + ring 供管理页）。
- 校验：`file` 非空时 `max_size_mb < 1` 或 `max_backups < 0` 或 `max_age_days < 0` → 配置错误。
- 父目录可 `MkdirAll`；仍失败则启动错误退出。

### R2. 内存环形缓冲（管理页基础）

- 自定义 `slog.Handler` wrapper：在写 stdout/file 的同时写入进程内 ring（线程安全）。
- 默认容量约 2000 条（常量或可配置，design 钉死）。
- 重启清空；不持久化 ring。

### R3. 管理 API：只读日志

- `GET /api/admin/logs?limit=200&level=&q=`（参数名 design 钉死）。
- 必须 `Auth.Middleware`；未登录 401。
- 响应 JSON 行列表：time、level、msg、attrs 摘要（含 request_id 若有）；**无** secret。
- `limit` 有上限（如 max 1000）。
- 有 `log.file` 时：优先 tail 当前日志文件末尾 N 行；失败回退 ring。
- 无 `log.file` 时：仅返回 ring，并标明 `source: "ring"`。
- **不接受**客户端任意文件路径参数。

### R4. 管理 UI

- 侧栏新增「日志」；路由 `/logs`；页面只读。
- 展示最近日志表格/等宽文本；支持刷新、limit、可选 level/关键词过滤。
- 文案说明：无落盘时仅进程内存最近日志、重启丢失；有落盘时展示文件 tail + 轮转说明。
- 不做：改日志、删日志文件、下载整包、实时 WebSocket（轮询/手动刷新即可）。

### R5. 文档与示例

- README / config.example：说明 `log.*`、Docker 挂载 state 日志路径、轮转参数。
- 明确：对象目录 `data_root` 不要当日志目录。

### R6. 测试

- 配置：空 file 不创建文件；非空校验非法 max_size。
- ring：并发 Append；Snapshot limit/level/q；敏感 key 过滤。
- API：401、有 ring 数据时 200、limit 截断。
- 可选：TempDir + 很小 MaxSize 触发轮转出现备份文件。

## Acceptance Criteria

- [x] `log.file=""` 时行为与现网一致：日志仍进 stdout；进程可启动；无强制日志文件。
- [x] `log.file` 有值时：同一条 slog 同时出现在 stdout 与文件；S3 `s3 request` 含 request_id。
- [x] 写入超过 `max_size_mb` 后发生轮转；历史文件数不超过 `max_backups`（+ 当前文件）。
- [x] `max_age_days>0` 时超龄文件可被清理（lumberjack 行为，有测或文档验收）。
- [x] `log.file` 配置了但无法创建/打开时进程启动失败并有明确错误。
- [x] `GET /api/admin/logs` 无会话 401；有会话返回最近日志 JSON。
- [x] 管理 UI 有日志页，可刷新查看；侧栏可进入。
- [x] 日志 API/UI 不出现在公开 ops；不返回 secret_key/密码。
- [x] config.example + README 已更新；相关单测通过。

## Out of Scope

- 日志写入数据库 / 外发 Loki/ELK。
- 默认强制落盘。
- 日志写进 `storage.data_root`。
- 全量历史检索、跨节点聚合、WebSocket 流式。
- 管理端修改/清空日志文件、改 log_level 热更新。
- 并入 `07-12-storage-reconcile` 实现范围。

## Planning status

- 状态：`planning`（用户要求：落盘 + 管理页一起规划；不实现）
- 实现前：确认 prd/design → `task.py start`
- **禁止**未 start 前改业务代码
