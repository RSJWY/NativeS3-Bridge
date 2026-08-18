# 控制协议契约修复 — 技术设计

> 对应 `prd.md`。**2026-08-18 部署策略已裁决:panel 与 node 同步升级,不做新旧混跑兼容。** 因此本设计不再围绕"双轨兼容"展开,而是把复杂度转移到"部署纪律";对恶意/故障对端的防护(钳制、上限)保持不变。

## D1 心跳间隔协商的字段与存储(R1)

- `HelloPayload` 加 `HeartbeatIntervalMS int \`json:"heartbeat_interval_ms"\``。**同步升级下该字段必填**,缺省/为 0 按协议违规处理,走 R1.3 非法值回落路径。
- panel 侧存储:连接级存 `AgentConn.HeartbeatInterval`;不新增 DB 列(`NodeState` 表已有 `last_heartbeat`,阈值只影响内存判定与读超时)。
- 阈值口径统一为 `heartbeat_interval × offline_multiplier`,不再保留"按 panel 本地配置"的第二套口径。
- `SweepOffline` 改造:优先读 Hub 在线连接上的 `HeartbeatInterval`,DB 兜底路径对无连接节点使用 panel 默认阈值(与混跑期无关,只是断连后的展示兜底)。
- 非法值钳制:`<1s` 或 `>10min` 拒绝,记 Warn 并回落到 panel 配置间隔。

## D2 版本协商升 v2 与快速失败(R4/R5)

- `controlproto` 常量改为 `ProtocolVersion = 2`、`MinCompatibleVersion = 2`。上下限同时升 v2,**不再保留 v1 支持**。
- `NegotiateVersion` 行为不变(取 min 后校验范围),但任一侧为 v1 时结果低于 MinCompatibleVersion,返回错误。
- 握手时版本不匹配即发送 `ErrorPayload{Code: ErrCodeVersionIncompatible, Fatal: true}` 并断连;两侧日志必须打印本端与对端版本以及"需同步升级"提示。
- node 侧收到 Fatal error 后让 `connectAndServe` 返回错误,`Run` 按既有退避重连。

## D3 import_report 分块格式(R4)

```go
type ImportReportChunkPayload struct {
    RequestID string `json:"request_id"`
    Seq       int    `json:"seq"`
    Total     int    `json:"total"`
    // 每块携带完整 DesiredState 中的一个片段;三类资源用同一 Seq/Total 编号空间
    Credentials []controlproto.DesiredCredential `json:"credentials,omitempty"`
    Buckets     []controlproto.DesiredBucket     `json:"buckets,omitempty"`
    Webhooks    []controlproto.DesiredWebhook    `json:"webhooks,omitempty"`
}
```

- 分块策略:node 侧按「序列化后 ≤ 512 KiB」累积条目切块;seq 从 0 开始,total 预先算出。
- request_id 复用触发帧的 envelope ID。
- panel 重组器:挂在 `AgentConn` 上,连接断开即清理。上限 32 块 / 16 MiB / 5 分钟,任一超限丢弃并断连。
- **v2 唯一形态**:node 恒用分页,不保留单帧回退分支;旧单帧 `import_report` 接收路径删除。

## D4 timeout_ms 的取消传导(R3)

- `handleTask`:`taskCtx, cancel := context.WithTimeout(ctx, timeout)`,传入 `LocalTaskRunner.Run`。
- timeout 来源:取 `task.TimeoutMS`;`<=0` 视为协议违规,使用 60s 默认值并 Warn。
- node 侧硬编码上界 10min:超出按 10min 执行并 Warn(防恶意/故障 panel 无限占住 serve 循环)。
- 三个 task 实现响应 ctx:scan/reconcile 的 WalkDir 回调里检查 `ctx.Err()`;log_query 检查入口。
- 超时后回 failed 结果;幂等台账只记成功,失败(含超时)不落 `applied_tasks`。

## D5 panel `task_timeout` 配置键(R3.5)

- `PanelConfig` 根部加 `TaskTimeout time.Duration \`yaml:"task_timeout"\``。
- `applyDefaults` 里 `<=0` 时填 `60s`(`DefaultTaskTimeout`),存量部署零变化。
- 接线:`cmd/panel/main.go` 读配置 → `adminserver deps` → `tasksRoute` 持有 → 调 `Dispatch` 时传入 `timeout`。
- `DefaultTaskTimeout` 常量保留作为默认值来源。

## D6 静默连接回收(R2)

- 方案:panel serve 循环每次 `readEnvelope` 前用 `context.WithTimeout(serveCtx, deadline)` 包裹,超时即关闭连接走既有断连清理。
- deadline = `上报间隔 × offline_multiplier + 30s` 裕量。上报值非法而回落的,按回落后的间隔计算。
- 同步升级后不存在"不上报间隔"的合法节点,**不再保留"不设读超时"的豁免分支**。
- 回收只摘连接,DB online 状态仍由 `SweepOffline` 负责,两边不打架。

## D7 测试策略

- 版本协商:v2 两端握手成功;构造 v1 对端(直接改 `ProtocolVersion`)验证握手失败、日志含版本与"需同步升级"。
- 心跳协商:60s 心跳持续 online;非法值(0、1h)回落并 Warn。
- 读超时回收:15s 上报节点静默约 75s 被回收;60s 上报节点约 210s 不被回收(用测试时钟注入或缩短倍数加速)。
- timeout_ms:假阻塞任务 60s 内被 ctx 中止并回 failed;`task_timeout: 300s` 下发 300000ms;超限钳制生效。
- import_report 分页:>1 MiB 数据分块落库成功;乱序到达重组;缺块超时/超块数/超字节上限拒绝。

## D8 不改的东西

- 既有消息类型与字段零改动(除新增字段/类型外)。
- 不引入 heartbeat ping/pong 帧。
- 不新增 DB 列/表/迁移。
