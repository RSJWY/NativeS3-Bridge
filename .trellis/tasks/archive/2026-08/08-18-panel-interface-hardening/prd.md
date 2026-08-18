# Panel 服务端接口加固

> 父任务:`.trellis/tasks/08-18-panel-node-interface-fixes`(部署安全红线见父任务 PRD)。本任务**只改 panel 二进制**,不改 wire 协议、不改 DB schema、不改配置键名,node 侧零感知。

## Goal

修复 panel 两个对外面(管理 HTTP API、节点控制面接入)的 5 个中危 + 9 个低危缺陷,全部保持线上滚动升级兼容:旧 node 对新 panel 无任何感知。

## 现状(实现前必读)

| 事实 | 位置 |
|---|---|
| 管理路由注册与中间件挂载 | `pkg/panel/adminserver.go:51-79` |
| 单体版已示范 trustForwarded 接线 | `cmd/natives3bridge/main.go:113-114`、`pkg/webadmin/server.go:46`、`pkg/webadmin/auth.go:39,137` |
| PanelConfig 无 trust_forwarded 字段 | `pkg/config/panel.go:17-44` |
| 管理 server 只有 ReadHeaderTimeout | `pkg/panel/adminserver.go:75-79` |
| 控制面写锁 `writeMu`(sync.Mutex),锁等待不感知 ctx | `pkg/panel/agentconn.go:78-80` |
| 心跳 ack 用无超时的 serve ctx 回写 | `pkg/panel/transport.go:457-466` |
| 任务下发写超时 10s 只覆盖拿到锁之后 | `pkg/panel/tasks.go:128` |
| `Dispatch` 成功后无条件 `markState(running)`;`markState` UPDATE 无 state 守卫且清 error | `pkg/panel/tasks.go:138,225-235`;对照有守卫的 `transport.go:563-566` |
| 心跳每帧触发 DB upsert(含 busy 重试)+ ack 回写,无节流 | `pkg/panel/transport.go:658-679`、`transport.go:711-753` |
| `/renew` 无频率限制,每调用一次 = CA 签名 + `node_certs` 插一行 | `pkg/panel/transport.go:162-232` |
| 瞬时 DB 错误导致整条控制连接被关闭 | `pkg/panel/transport.go:337-340` + `503-505,567-569` |
| hello 的 `content_hash` 未消毒落库(`region` 有消毒先例) | `pkg/panel/transport.go:684-691` vs `696-708` |
| `updateNode`/`retireNode` 忽略 `loadNode` 的 ok,双写响应 | `pkg/panel/adminapi.go:271-272,307-308`(loadNode 失败已写 500,见 `929-940`) |
| `certsRoute` 全程无 `loadNode` 404 检查 | `pkg/panel/adminapi.go:417-453` |
| `tokens`、`certs/revoke` 不检查多余路径段 | `pkg/panel/adminapi.go:150-151,438` |
| hash mismatch 落 default 分支返回 500 | `pkg/panel/adminapi.go:708-714`(`desiredPushAdminMessage:726` 已有文案) |
| 退役节点仍可建 credential/bucket/webhook 草稿 | `pkg/panel/adminapi.go:475-480` 起;退役语义见 `retireNode:278` 与 `updateNode:226` 的先例 |
| 握手后无读超时,静默连接永不回收 | `pkg/panel/transport.go:329-341`;`SweepOffline:834-845` 只写 DB 不摘连接 |

## Requirements

### R1 trustForwarded 配置接线(M1)
- R1.1 `PanelConfig` 的 webadmin 段新增 `trust_forwarded` 布尔键,**默认 false**(与现状一致,存量部署零变化)。
- R1.2 接线链路参照单体版:`cmd/panel/main.go` → `adminserver` → `webadmin.NewServer`/`NewAuthForServiceMode`,使登录限流与 audit 的 `clientIP` 在开启后取 `X-Forwarded-For`。
- R1.3 `configs/panel.example.yaml` 加注释说明:仅当管理面在受信反代之后才可开启,否则攻击者可伪造来源 IP 绕过登录限流。
- R1.4 文档 `docs/public-deployment.md`(其推荐反代部署)补一句配置指引。

### R2 管理 HTTP server 超时补全(M2)
- R2.1 `http.Server` 增加 `ReadTimeout` 与 `IdleTimeout`(沿用现有硬编码风格,不设配置键)。取值必须宽容于正常管理操作:admin JSON body 上限 1 MiB,ReadTimeout 建议 30s 级;IdleTimeout 120s 级。
- R2.2 **实现前先核实**管理面是否有长连接型端点(SSE/WebSocket/长轮询,如 dashboard 实时推送、日志流):若有,ReadTimeout 不得覆盖该类端点(用 per-handler 超时或豁免),并在 design.md 记录结论。
- R2.3 不设 `WriteTimeout`(大响应/慢客户端的正常下载不能被误杀),除非 R2.2 证明安全。

### R3 控制面写路径可中断(M3)
- R3.1 `agentConn.writeMu` 由 `sync.Mutex` 改为可感知 ctx 的获取方式(带缓冲为 1 的 channel 信号量,`select { case sem <- struct{}{}: case <-ctx.Done(): }`)。锁等待必须能被 ctx 取消。
- R3.2 心跳 ack 回写改用带超时的 ctx(沿用现有 `writeTimeout` 量级,10s)。
- R3.3 写失败/超时后的连接清理语义不变(仍走现有断连路径),只是不再永久阻塞。
- R3.4 补充并发测试:模拟"对端不读"的连接,验证 `send`/`Dispatch`/心跳 ack 在超时内返回错误而不是永久阻塞,且 `disconnect` 清理最终发生。

### R4 任务状态迁移加守卫(M4)
- R4.1 `markState` 的 UPDATE 增加状态前置条件:迁到 `running` 仅当当前为 `pending`(语义即"只能向前流转");不再无条件清空 `error` 列。
- R4.2 守卫未命中(RowsAffected=0,说明结果已先落库)时打 Info 日志并静默放过——这是竞态的正常结局,不是错误。
- R4.3 回归测试:模拟"结果先于 markState 落库"的交错,断言终态 success 不被覆盖、error 不被清空。

### R5 心跳写放大节流(M5)
- R5.1 `handleHeartbeat`:收到帧后**总是**更新内存态(LastSeen/Hub)并回 ack,但 DB `upsertNodeState` 按节点节流——距该节点上次落库不足阈值则跳过。阈值默认值取 panel 配置的 `heartbeat_interval` 的 1/2(正常心跳 cadence 下落库行为与现状一致;狂发帧时 DB 写压力有上限)。不设新配置键。
- R5.2 节流不得影响 `touchHeartbeat` 对 online 状态的既有语义:被节流跳过的帧不得让 `SweepOffline` 误判(即内存 LastSeen 与 DB last_seen 的口径要想清楚,在 design.md 写明选择及理由)。

### R6 瞬时 DB 错误不再拆连接
- R6.1 `serve` 的 dispatch 错误处理分级:消息处理回调返回的**存储类瞬时错误**(ack/task_result 落库失败)降级为 Error 日志 + 继续服务;仅协议错误(畸形帧、版本不兼容)与 IO 错误才关闭连接。
- R6.2 不得因此吞掉真正的持久故障:同一连接连续 N 次(如 5 次)存储错误仍应断开,避免挂着一条永远写不进库的连接。

### R7 `/renew` 频率限制
- R7.1 按节点(mTLS 身份)限频:固定阈值如每小时 10 次,超限返回 429 + `Retry-After`。阈值硬编码,量级必须远高于正常续期频率(每 90 天 1 次)。
- R7.2 顺带兜底:同一节点未激活证书数量设上限(如 20 张),达到上限拒绝签发并提示先连接激活。实现时二选一或都做,以简单为准,在 design.md 记录选择。

### R8 hello 的 content_hash 消毒
- R8.1 落库前对 `content_hash` 做与 `sanitizeReportedRegion` 同策略的消毒(长度截断 + 控制字符剥离)。失败/超限的处理方式与 region 对齐。

### R9 管理 API 语义修正(5 项)
- R9.1 `updateNode`/`retireNode`:`loadNode` 返回 ok=false 时直接 return,不再写第二段响应。
- R9.2 `certsRoute` 入口加 `loadNode` 404 检查,与其他子路由一致。
- R9.3 `tokens`、`certs/revoke` 分支校验路径段数,多余段一律 404。
- R9.4 `pushDesiredState`:`ErrDesiredSnapshotHashMismatch` 映射 409(用 `desiredPushAdminMessage` 已有文案),不再 500。
- R9.5 credentials/buckets/webhooks/rate-limit 等节点子资源路由:对已退役节点返回 409(对齐 `updateNode`/`issueToken` 的既有做法)。

### R10 静默连接回收 —— **不在本任务**
依赖心跳间隔协商(C4)落地后才有安全依据(固定读超时可能误杀心跳间隔被调大的存量节点)。本任务只做 R3 保证写侧不卡死;读侧回收在 C4 实施。

## 兼容性论证(每项一句话)

- R1:新键默认 false = 现状;R2:正常请求 body 远小且快,30s 无感;R3:仅异常路径从"永久阻塞"变"超时断开",正常路径无变化;R4:只堵住错误的竞态结局;R5:正常 cadence 下落库频率不变;R6:仅瞬时错误不再误杀;R7:正常续期频率远低于阈值;R8/R9:错误输入/错误调用的响应码修正,正常调用不变。

## Acceptance Criteria

- [x] AC1 `go build ./...` + `go vet ./...` 干净;`go test -race ./pkg/panel/...` 全绿。
- [x] AC2 R3 并发测试通过:对端不读时,推送/下发/心跳 ack 均在 ~10s 量级返回错误,无 goroutine 泄漏(测试用 runtime.NumGoroutine 或超时断言)。
- [x] AC3 R4 竞态测试通过:结果先落库后 `markState(running)` 不覆盖终态。
- [x] AC4 R5:以 15s 间隔正常心跳的节点,DB `last_seen` 更新行为与升级前一致;以 100ms 间隔狂发心跳,DB 写频率被压到阈值以下且连接不被断开。
- [x] AC5 R6:注入一次临时 DB 错误(可用测试钩子),连接保持;连续超阈值后断开。
- [x] AC6 R7:单节点连续 11 次 `/renew`(合法 CSR),第 11 次起 429;正常周期续期不受影响。
- [x] AC7 R9:五个语义修正各有测试覆盖(双写→单 500;不存在节点 certs→404;多余路径段→404;hash mismatch→409;退役节点建草稿→409)。
- [x] AC8 R1:panel.yaml 加 `trust_forwarded: true` 并经反带头访问,登录限流按真实客户端 IP 计数(测试或手工验证记录);不写该键行为与升级前完全一致。
- [x] AC9 R8:hello 携带含控制字符的超长 content_hash,落库值已消毒。
- [x] AC10 配置/协议/DB 零变更确认:node 二进制未动;`git diff` 不涉及 `pkg/controlproto/`、`pkg/nodeagent/`、`models.go` 的 schema 字段、`migrate.go`。
