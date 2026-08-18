# Panel 服务端接口加固 — 技术设计

> 对应 `prd.md`。只记录需要决策的点;PRD 已写死的事实性内容不重复。

## D1 writeMu 可中断化(R3)的选型

**决策**:把 `agentConn.writeMu sync.Mutex` 换成 `sem chan struct{}`(容量 1),获取处:

```go
select {
case c.sem <- struct{}{}:
    defer func() { <-c.sem }()
case <-ctx.Done():
    return ctx.Err()
}
```

**权衡**:Go 的 `sync.Mutex` 锁等待不可中断,这是 M3 的根因;channel 信号量是改动最小的等价物,全文件只有 `send` 一处获取点,风险可控。不引入 `golang.org/x/sync/semaphore`(查 go.mod 若已有依赖则可用,没有就别为它加依赖)。

**配套的 ctx 来源**:心跳 ack(`transport.go` handleHeartbeat)与 `Dispatch` 下发统一包一层 `context.WithTimeout(serveCtx, writeTimeout)`;`PushDesiredState` 路径同样处理。writeTimeout 沿用 tasks.go 已有的 10s 常量,若该常量是 tasks.go 私有则上提到 agentconn 或 transport 共用。

**断开清理**:写超时返回错误后,调用方走现有断连路径;`serve` 循环退出时 ctx 取消,卡在 select 上的写方随之释放——不再有永久泄漏。

## D2 心跳节流(R5)的内存/DB 双轨口径

问题:`SweepOffline` 依据 DB `last_seen` 判离线,而节流跳过了 DB 写。若内存 LastSeen 更新了但 DB 没写,45s 后 SweepOffline 会把一个活跃节点标 offline。

**决策**:节流的是「每帧都 upsert」这个放大行为,而不是让 DB 变旧。具体:每帧到 → 更新内存 LastSeen → 若距上次**落库** ≥ threshold 则 upsert 并记录落库时间 → 回 ack。threshold = `heartbeat_interval / 2`(默认 15s/2 = 7.5s)。正常 cadence(15s 一帧)下每帧都满足落库条件,DB 行为与现状**逐帧一致**;狂发场景下 DB 写上限 = 2/interval。**同时**把 `SweepOffline` 的判定源从 DB last_seen 改为优先看 Hub 内存 LastSeen(Hub 内有连接即在线),DB 口径仅作兜底展示——若此改动面超预期,则保持 SweepOffline 不动,依赖上述「正常 cadence 每帧落库」不变性,并在代码注释写明该依赖。实施者二选一,倾向后者(更小 diff)。

## D3 管理面超时(R2)的取值与豁免

- 先核实:`dashboard.go`、`logs.go` 是否有 SSE/流式端点。若有流式 **GET**(无 body),`ReadTimeout` 不影响(ReadTimeout 只管到 body 读完),安全;若有长 body 上传型端点(如导入),需单独核算。
- 取值:`ReadTimeout: 30s`、`IdleTimeout: 120s`,硬编码,与既有 `ReadHeaderTimeout: 10s` 风格一致。
- 不设 `WriteTimeout`(仪表盘首屏在大数据量下可能慢)。

## D4 `/renew` 限频(R7)的选型

**决策**:按节点身份计数,滑动窗口 1 小时上限 10 次,超限 429 + `Retry-After`。内存计数即可(panel 单进程),重启清零可接受。不做「未激活证书上限」第二道闸——R7.1 一道已把 CA 签名 CPU 与表增长都压到有界,少一个机制少一份复杂度。若实施者发现内存计数实现比查表计数更绕,允许改为「查 `node_certs` 近 1 小时该节点行数 ≥ 10 则 429」,两种实现都满足 PRD。

## D5 瞬时错误分级(R6)的判定方式

`handleTaskResult`/ack 回调返回的 error 目前无法区分「DB 瞬时」与「协议错误」。**决策**:在回调内部就把存储错误消化掉(打 Error 日志 + 计数,返回 nil),而不是在 serve 里分类 error——回调最知道错因。连续失败计数挂在 agentConn 上,≥5 次时由回调返回一个哨兵错误触发断连。协议解析错误维持原路径(返回非 nil → 断连)。

## D6 不改的东西(明确边界)

- 不动 `pkg/controlproto/`、`pkg/nodeagent/`、`cmd/node/`。
- 不动 `models.go` 任何字段、`migrate.go` 迁移表。
- 不给 Hub 加 Ping/pong(coder/websocket 的 ping 对旧 node 客户端行为未验证,且回收问题归 C4)。
- 不重构 `nodeToResponse` 的 N+1(性能项,父任务已排除)。
