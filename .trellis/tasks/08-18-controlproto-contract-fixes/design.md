# 控制协议契约修复 — 技术设计

> 对应 `prd.md`。本任务全部复杂度集中在「协议演进不破坏混跑」,设计围绕此展开。

## D1 心跳协商的字段与存储(R1)

- `HelloPayload` 加 `HeartbeatIntervalMS int \`json:"heartbeat_interval_ms,omitempty"\``。omitempty 保证旧字段集序列化结果不变。
- panel 存储位置:`agentConn` 上加字段 + 随 `NodeState` 落库(heartbeat 路径已写库,捎带一列即可——**注意:若 NodeState 落库需要新增 DB 列则违反父任务红线,此时改为只存内存,SweepOffline 读内存值,节点断连后该次会话的阈值随连接消亡,可接受**)。实施者先查 `NodeState` 模型与 migrate 注册表,确认是否已有可复用列;没有就内存方案,不加列。
- `SweepOffline` 改造:遍历在线连接时逐节点用各自阈值;DB 兜底路径对未上报节点用 panel 默认阈值。

## D2 版本协商升 v2 的机制(R4)

`version.go` 现状:`MinSupported=1, MaxSupported=1`。改为 `MaxSupported=2`。`NegotiateVersion` 取 min(两端 Max),只要 ≥ Min 即兼容——旧端 Max=1,自动协商出 1。无需新增握手往返。

**能力表**(放在 version.go 注释):v1 = 基础集;v2 = v1 + import_report_chunk。心跳协商字段(R1)不设版本门控:可选字段天然安全,旧端忽略即可。

## D3 import_report 分块格式(R4)

```go
type ImportReportChunkPayload struct {
    RequestID string                       `json:"request_id"`
    Seq       int                          `json:"seq"`
    Total     int                          `json:"total"`
    // 每块只带一类资源的一段;三类资源分别编号到同一个 total
    Credentials []CredentialImportItem    `json:"credentials,omitempty"`
    Buckets     []BucketImportItem        `json:"buckets,omitempty"`
    Webhooks    []WebhookImportItem       `json:"webhooks,omitempty"`
}
```

(具体类型名以 payloads.go 现有 import 相关结构为准,上面是示意。)

- 分块策略:按「序列化后 ≤ 512 KiB」累积条目切块;seq 从 0,total 预先算出(panel 重组需要总数)。
- request_id 复用触发帧的 envelope ID,与现有 `import_report` 的对应关系一致。
- panel 重组器:`map[requestID]*重组态`,挂在 agentConn 上,连接断开即清理;每请求上限 32 块、5 分钟超时(设计值,实现可微调)。

## D4 timeout_ms 的取消传导(R3)

- `handleTask`:`taskCtx, cancel := context.WithTimeout(ctx, ...)`,传入 `LocalTaskRunner.Run`。
- 三个 task 实现逐个加 ctx 检查:scan/reconcile 的 WalkDir 回调里 `if ctx.Err() != nil { return ctx.Err() }`;log_query 本来有界,检查入口即可。
- 超时后回 failed 结果;**注意幂等台账**:超时失败的任务**不落** applied_tasks(它不是成功执行),重发的新 task_id 可正常执行。核对现有代码在失败时是否落台账,保持「只记成功」语义。

## D5 读超时回收的实现(R2)

- coder/websocket 无内建读 deadline;用 `ws.SetReadLimit` 同款思路不行。**方案**:serve 循环每次 Read 前 `ctx, cancel := context.WithTimeout(serveCtx, deadline)`,Read 返回后 cancel 换下轮;或看门狗 goroutine(与 C3 node 侧 D3 对称)。二选一,倾向看门狗(与 C3 一致的机制,审阅者心智负担小)。
- 仅对 `HeartbeatIntervalMS > 0` 的连接启用。

## D6 测试策略

- 版本混跑:测试里直接构造两个 `controlproto` 端点(而非起真实进程),分别钉死 MaxSupported=1/2,验证协商与能力开关。
- 分块重组:构造 3 块乱序到达、重复块、缺块超时、超限 五个用例。
- 心跳协商:钳制边界(0、500ms、11min)三例。

## D7 不改的东西

- 既有消息类型与字段零改动;v1 行为路径一行不动。
- panel 对 v1 节点的 import 处理逻辑原样保留。
- 不引入 heartbeat ping/pong 帧(看门狗已够,少一个新消息类型)。
