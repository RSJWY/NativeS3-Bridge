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

**核实结论(2026-08-18,R2.2 前置已完成,可直接实施)**:

1. **两个 server 物理隔离**:管理面是 `pkg/panel/adminserver.go:75` 的 `http.Server`;节点控制面是 `cmd/panel/main.go:134` 的**另一个** `http.Server`。`grep websocket` 命中全部落在 `pkg/panel/transport.go` / `agentconn.go`,即只挂在控制面 server 上。给管理 server 加超时对节点 websocket 长连接**零影响**。
2. **管理面无流式端点**:`grep 'Flush()|text/event-stream|Hijack|http.Flusher' pkg/panel pkg/webadmin` 在 `pkg/webadmin` 零命中。唯一像流式的 `/api/admin/logs` 走 `LogsViewer.ServeHTTP`(`pkg/webadmin/logs.go:63`),是一次性 JSON 快照的 GET,不是 SSE。
3. **最大 body 已有硬上限**:管理面唯一的大 body 入口是 `pkg/panel/adminapi.go:995` 的 `io.LimitReader(r.Body, 1<<20)` = 1 MiB。30s 读完 1 MiB 对任何真实链路都富余。

结论:`ReadTimeout` 无需任何 per-handler 豁免,直接加在 server 上即可。

- 取值:`ReadTimeout: 30s`、`IdleTimeout: 120s`,硬编码,与既有 `ReadHeaderTimeout: 10s` 风格一致。
- 不设 `WriteTimeout`(仪表盘首屏在大数据量下可能慢)。
- **决策记录(2026-08-18 评审)**:这两个超时**不做成配置项**。理由:admin JSON body 上限 1 MiB,30s 对任何真实网络都富余;管理面按文档推荐挂在反代后时,代理与面板同机/同内网,不存在需要调优的慢链路场景。实施者不要临时起意加配置键——本批修复的新增配置键全集固定为 `allow_insecure_transport`(C1)、`trust_forwarded`(C2)、`task_timeout`(C4),见父任务 PRD 红线。

## D4 `/renew` 限频(R7)的选型

**决策**:按节点身份计数,滑动窗口 1 小时上限 10 次,超限 429 + `Retry-After`。内存计数即可(panel 单进程),重启清零可接受。不做「未激活证书上限」第二道闸——R7.1 一道已把 CA 签名 CPU 与表增长都压到有界,少一个机制少一份复杂度。若实施者发现内存计数实现比查表计数更绕,允许改为「查 `node_certs` 近 1 小时该节点行数 ≥ 10 则 429」,两种实现都满足 PRD。

## D5 瞬时错误分级(R6)的判定方式

`handleTaskResult`/ack 回调返回的 error 目前无法区分「DB 瞬时」与「协议错误」。**决策**:在回调内部就把存储错误消化掉(打 Error 日志 + 计数,返回 nil),而不是在 serve 里分类 error——回调最知道错因。连续失败计数挂在 agentConn 上,≥5 次时由回调返回一个哨兵错误触发断连。协议解析错误维持原路径(返回非 nil → 断连)。

## D7 R9.5 退役节点:拒写但保留只读(实施期补充决策,2026-08-18)

PRD R9.5 字面写「credentials/buckets/webhooks/rate-limit 路由对已退役节点返回 409」。实施时发现这四条路由是 **GET 与写操作混在同一入口**,若整条路由 409,管理员将再也看不到退役节点的历史凭证/桶/webhook 列表——而 `retireNode` 的设计原意明确是「node 行保留用于审计关系」。整条 409 会直接摧毁这个审计可见性。

**决策**:只拒绝写方法(POST/PATCH/PUT/DELETE)返回 409,GET 保持 200。这满足 PRD 要修的真实缺陷(「退役节点仍可建 credential/bucket/webhook 草稿」——草稿永远下发不出去),同时不牺牲审计追溯。实现为 `adminapi.go` 的共用 helper `rejectRetiredWrite`,四条路由统一调用,语义与 `updateNode:226`、`issueToken` 的既有 409 对齐。

## D8 R1 `trust_forwarded` 的落键位置(实施期补充决策,2026-08-18)

PRD R1.1 写「`PanelConfig` 的 **webadmin 段**新增 `trust_forwarded`」。实施时发现 `webadmin:` 段映射的 `config.WebAdminConfig` 是**单体版与 panel 共用**的类型:往里加字段会让单体版的 `webadmin.trust_forwarded` 也变成一个可写但**无人读取**的键(单体版真正生效的是 `rate_limit.trust_forwarded`,见 `config.go:123`),这是比"放哪个段"严重得多的配置陷阱。

**决策**:落在 `PanelConfig` **顶层**,与 `admin_addr` / `admin_tls` 同层——三者都描述管理面入口属性,而 panel 没有 `rate_limit` 段可归。共用类型 `WebAdminConfig` 不动,单体版零影响。键名仍是 `trust_forwarded`,不算新增第四个配置键(仍在父任务冻结的键集内)。

配套发现:`webadmin.clientIP`(`net.go:9`)取 `X-Forwarded-For` 的**最右**一段,与 `docs/public-deployment.md` 里 nginx 示例的 `$proxy_add_x_forwarded_for`(追加模式)正好配套安全——客户端伪造的值留在左侧被忽略。文档已补该口径说明,避免运维误改成"只透传客户端原值"而使锁定失效。

## D9 实施期发现的两处 PRD 覆盖缺口(2026-08-18)

**D9.1 R3 漏了第四处控制面写路径。** PRD R3 列了三处(心跳 ack、Dispatch、PushDesiredState),实际还有 `migration.go` 的 `import_request`(`conn.sendMessage`)。核实结论:该路径的 ctx 来自 `adminapi.go` 的 `requestImport`,已经包了 `context.WithTimeout(r.Context(), 30*time.Second)`,写等待本就有界,不会永久阻塞。因此**不改**,只在此记录已核实,避免后续审查误判为漏修。

**D9.2 R8 漏了 `content_hash` 的第二个写入点。** PRD R8 只说 hello 的 `content_hash`,但 `handleAck` 的 `SyncStateSynced/Drift` 分支也把节点自报的 `ack.ContentHash` 写进同一列。只消毒 hello 等于只堵一半——脏值照样能从 ack 进库。**已一并消毒**,并把 region/hash 共用的消毒逻辑抽成 `sanitizeReportedText`,不留两份复制。注意落库用消毒值、一致性比较仍用原值(合法的 64 位摘要不受消毒影响,而比较的语义是"节点自报的是否等于已发布的")。

## D10 质量检查发现并修掉的三项(2026-08-18)

**D10.1 消毒按字节截断会产出非法 UTF-8(真缺陷,已修)。** `sanitizeReportedText` 原先用 `value[:maxBytes]`,边界落在多字节字符中间时切出半个 rune(已复现:62 个 `a` + `中文` → 尾部 `[228 184]`,`utf8.ValidString` 为 false)。危害链条:panel 支持 MySQL/Postgres(`config/panel.go:173`),utf8mb4 列拒收非法字节序列 → 落库失败 → 计入本任务新加的 R6 连续存储失败计数 → 连续 5 次断连。等于把"一个脏值写不进去"放大成"节点反复掉线"。而 region 是运维在 node 本地 yaml 里填的,写中文地区名很正常,不是理论风险。

修复:截断收敛到最后一个完整 rune,仍以字节计量(列宽是字节数)。这同时是**向既有惯例对齐**——同文件的 `safeReportedApplyError` 处理同类节点自报文本时本来就用 `[]rune` 截断。缺陷继承自原有的 `sanitizeReportedRegion`,但本任务把它的适用面扩大到了 `content_hash`,故在本任务修掉。

**D10.2 节流时间戳在落库失败时也被推进(逻辑瑕疵,已修)。** `shouldPersistHeartbeat` 原先在"决定落库"时就写 `lastPersistedBeat`,但 upsert 可能失败。失败后时间戳已推进,下一帧会被节流跳过——DB 短暂故障期间反而自己减少了重试机会,且"已落库"的语义与事实不符。改为两段式:`shouldPersistHeartbeat` 只判断,落库成功后由 `markHeartbeatPersisted` 记录。

**D10.3 R7 缺 handler 层测试(覆盖缺口,已补)。** 原先只测了 `renewLimiter` 的单元逻辑,没验证接线。AC6 的字面要求是"单节点连续 11 次 `/renew`(合法 CSR),第 11 次起 429"。已补 `renewlimit_handler_test.go`:走真实 mTLS + 真实 CSR,断言前 10 次 200、第 11 次 429 且带合法 `Retry-After`、被拒请求**不签发证书**(证书行数不变)、且留下 `rate_limited` 审计。

**已用"临时回退→确认变红→恢复"验证过的测试**(证明不是永远绿的断言):R9.1 双写(还顺带暴露旧行为会吐一个全零 node 对象)、R1 trustForwarded 接线、R3 写等待永久阻塞、R5 节流(未节流 600 次 vs 节流后 ≤9 次)、R6 错误分级。

**记为后续项(不在本任务范围)**:`audit.go:61` 的 `redactResource` 也是字节截断,但它截的是 ASCII 标识符(access key / fingerprint),且属既有代码、不在本任务改动面内,未扩大范围去动。

## D6 不改的东西(明确边界)

- 不动 `pkg/controlproto/`、`pkg/nodeagent/`、`cmd/node/`。
- 不动 `models.go` 任何字段、`migrate.go` 迁移表。
- 不给 Hub 加 Ping/pong(coder/websocket 的 ping 对旧 node 客户端行为未验证,且回收问题归 C4)。
- 不重构 `nodeToResponse` 的 N+1(性能项,父任务已排除)。
