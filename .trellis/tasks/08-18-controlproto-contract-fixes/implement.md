# 控制协议契约修复 — 执行计划

> 前置:已读 `prd.md`、`design.md`;C2 已合并归档,agentconn 写路径已改为 ctx 感知信号量。本任务动 `pkg/controlproto/`、`pkg/panel/`、`pkg/nodeagent/`、`cmd/panel/`、`configs/`、`docs/` 六处。

## 环境基线

```bash
go build ./... && go vet ./...
go test -race ./pkg/controlproto/... ./pkg/panel/... ./pkg/nodeagent/...
# 复核:控制面 payload 解码未开 DisallowUnknownFields(新增可选字段的前提)
grep -rn "DisallowUnknownFields" pkg/controlproto/ pkg/panel/transport.go pkg/nodeagent/
```

## 执行清单

### Step 1 R1 心跳间隔协商

1. `HelloPayload` 加 `HeartbeatIntervalMS int \`json:"heartbeat_interval_ms"\``。
2. node `handshake` 填 `c.cfg.HeartbeatInterval` 毫秒值。
3. panel `handshake` 解析并钳制(1s~10min),存到 `AgentConn.HeartbeatInterval`;非法值回落到 `s.deps.HeartbeatInterval` 并 Warn。
4. panel `SweepOffline` 按连接上的 `HeartbeatInterval` 计算阈值;无连接节点用 panel 默认阈值。
5. R1.4:hello 的 `NodeID` 与证书身份不一致时 Warn。
6. 测试:60s 心跳不抖动;非法值回落;阈值口径正确。
7. 验证:`go test -race ./pkg/controlproto/ ./pkg/panel/ -run 'TestHello|TestSweep|TestHeartbeat'`

### Step 2 R2 静默连接回收

1. panel `serve` 循环每次 `readEnvelope` 前包 `context.WithTimeout(serveCtx, readTimeout)`。
2. `readTimeout = conn.heartbeatInterval() × offline_multiplier + 30s`;未协商到有效值时使用 panel 默认。
3. 测试:上报 15s 的节点静默约 75s 断连;上报 60s 的节点 75s 时不断连。
4. 验证:`go test -race ./pkg/panel/ -run 'TestTransport'`

### Step 3 R3 node 执行 timeout_ms + panel task_timeout 配置键

1. `LocalTaskRunner.Run` 签名已含 `ctx`;确保 `runStorageScan` 的 WalkDir 回调检查 `ctx.Err()`。
2. `nodeagent.Client.handleTask` 用 `taskTimeout(task.TimeoutMS)` 计算实际超时。
3. node 侧 timeout 钳制:>10min 按 10min、<=0 按 60s,都 Warn。
4. 超时后回 `task_result{state=failed,error=timeout}`;失败不落 `applied_tasks`。
5. `PanelConfig` 加 `TaskTimeout time.Duration \`yaml:"task_timeout"\``;默认值 60s。
6. `cmd/panel/main.go` 把 `cfg.TaskTimeout` 传入 `NewTaskOrchestrator`;adminserver deps → tasksRoute 接线。
7. `configs/panel.example.yaml` 加注释键。
8. 测试:阻塞任务 60s 内中止;`task_timeout: 300s` 下发 300000ms;缺省 60s;超限钳制。
9. 验证:`go test -race ./pkg/nodeagent/ ./pkg/config/ ./pkg/panel/ -run 'TestTask|TestReconcile|TestConfig|TestDispatch'`

### Step 4 R4 版本 v2 + import_report 分页

1. `controlproto/envelope.go` 常量改为 `ProtocolVersion = 2`、`MinCompatibleVersion = 2`;消息类型加 `TypeImportReportChunk`。
2. `controlproto/payloads.go` 加 `ImportReportChunkPayload`。
3. `controlproto/version.go` 注释更新:v2 能力、v1 不再支持、需同步升级。
4. node `handleImportRequest` 按 v2 分块发送(≤512 KiB/块);用 `request_id = env.ID`。
5. panel `dispatch` 增加 `TypeImportReportChunk` 分支;`handleImportReportChunk` 在 `AgentConn` 上重组,收齐后调用 `MigrationSink.ingestReport`。
6. 重组缓存上限:单 request 32 块 / 16 MiB / 5 分钟,超限丢弃并断连。
7. 删除旧 `TypeImportReport` 单帧接收路径(保留消息类型常量以维持命名空间,但 panel 不再处理)。
8. 测试:v2 两端成功;v1 对端握手失败并日志提示;分块重组/乱序/超限。
9. 验证:`go test -race ./pkg/controlproto/... ./pkg/panel/... ./pkg/nodeagent/...`

### Step 5 文档与全量回归

1. `docs/multi-node-operations.md` 协议章节:v2 能力、v1 不再受支持、同步升级/成对回滚。
2. 全量:

```bash
go build ./... && go vet ./...
go test -race ./pkg/controlproto/... ./pkg/panel/... ./pkg/nodeagent/... ./cmd/...
git diff pkg/controlproto/   # 人工复核:既有字段零改动
```

## 审查门禁(trellis-check 要点)

- 逐条核 R1-R5,重点 R5.2 快速失败日志是否含本端/对端版本与"需同步升级"。
- 红线:除本任务列明处,既有 wire 字段/DB schema 零改动。
- 与 C2 的交接面:agentconn 写超时形态、读超时机制是否同构(ctx 可中断)。

## 回滚点

Step 1-2、3、4 各自独立提交;部署回滚 = **两侧二进制同时回退**,无数据迁移。**只回滚一侧会导致协议不匹配、控制面失联**,需成对执行。
