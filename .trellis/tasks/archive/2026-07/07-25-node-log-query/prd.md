# Node 日志拉取与 Panel 展示

## Goal

把现有但不可达的 `log_query` 控制面任务补成可用的 Node 诊断功能：Panel 管理员在 Node 详情页发起受限查询，看到结构化 ring 日志和明确的任务状态。

## Confirmed facts

- `controlproto.TaskLogQuery`、Panel `TaskOrchestrator`、任务 REST 路由和 Node `LocalTaskRunner` 已存在。
- Node runner 当前只读 ring、忽略 level/since/until、只返回格式化字符串；Panel 前端没有任务 API 或日志组件。
- 控制面已有 mTLS、任务 ID 幂等、在途上限和断线状态；不得扩展为任意 shell 或无限日志流。

## Requirements

1. 扩展兼容的 `log_query` 参数/结果契约：级别、关键字、时间范围、条数上限、响应字节上限、截断标记，以及结构化时间/级别/消息/属性；保留旧文本结果 fallback。
2. Node ring 查询必须真正应用过滤并在边界参数非法或超限时安全失败/截断；查询不读取或传输轮转历史文件。
3. Panel 任务响应改为类型化、节点作用域安全的 JSON；不得把日志查询关键字、secret 或结果原样写入不必要的审计/持久日志；完善超时/晚到结果处理。
4. Node 详情页提供拉取、过滤、轮询、空态、离线、超时、失败和截断状态；组件不保留敏感 one-time 值。
5. 添加协议、runner、Panel API、前端构建和真实 mTLS 查询回归。

## Acceptance Criteria

- [x] 在线 Node 可从 Panel UI 发起 `log_query`，看到结构化结果和 `truncated` 状态。
- [x] level/keyword/since/until/limit 过滤在 Node 端生效；旧 Node 的文本结果仍可显示。
- [x] Node 离线、控制面断开、任务超时和失败均显示可读状态，不影响 Node S3 数据面。
- [x] 查询结果受条数/字节上限约束，不开放任意命令，不远程读取轮转历史文件。
- [x] 任务查询只能读取目标 Node 的 task；响应、审计和测试产物不包含敏感材料。
- [x] 相关 Go 测试、`go vet ./...`、`go build ./...`、前端生产构建和 Panel↔Node E2E 通过。

## Dependency and out of scope

- 可在公共日志初始化 child 之后或并行实现；若共享 setup 签名变化，必须在集成前同步调整。
- Parent 的最终 E2E 必须在两个 child 都完成后运行；目录顺序不构成依赖。
- 不实现 Node 轮转历史远程查看、实时 WebSocket 日志流、日志下载/删除或任意 shell。
