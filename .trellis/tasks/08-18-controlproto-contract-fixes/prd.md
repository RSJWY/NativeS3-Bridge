# 控制协议契约修复(超时/心跳协商/导入分页)

> 父任务:`.trellis/tasks/08-18-panel-node-interface-fixes`。**本任务涉及 wire 协议演进,是父任务下唯一允许动 `pkg/controlproto/` 的子任务**,红线附加:所有新增必须向后兼容,新旧 panel/node 任意混跑不得比现状更差。

## Goal

修复三个契约不对称缺陷:心跳间隔双边盲配(M6)、`timeout_ms` 发了不执行(M7)、`import_report` 无界超读限断连(M10);顺带补上 C2 移交的「静默连接回收」(其安全依据正是 M6 的协商结果)。

## 现状(实现前必读)

| 事实 | 位置 |
|---|---|
| `HelloPayload` 无心跳间隔字段 | `pkg/controlproto/payloads.go:20-28` |
| panel 判离线:`heartbeat_interval × offline_multiplier`(默认 15s×3=45s) | `pkg/panel/transport.go:834-845`、`cmd/panel/main.go:143` |
| node 心跳间隔独立配置,默认 15s | `pkg/config/node.go:125-127` |
| panel 填 `TimeoutMS=60000` 并 60s 判任务终止 | `pkg/panel/tasks.go:120-127`、`tasks.go:161-176` |
| node 端 `TimeoutMS` 零引用,任务以 serve ctx 跑到底 | `pkg/nodeagent/client.go:506-539`、`pkg/nodeagent/tasks.go:207-284` |
| 任务结果守卫 `state IN (pending,running)`,超时后结果被丢弃 | `pkg/panel/transport.go:563-572` |
| `import_report` 全量明文凭证无界单帧 | `pkg/nodeagent/client.go:482-501`、`pkg/controlproto/payloads.go:294-300` |
| panel 读上限 1 MiB,超限断连 | `pkg/panel/agentconn.go:17`、`transport.go:260` |
| 版本协商机制现成可用 | `pkg/controlproto/version.go` `NegotiateVersion` |
| 控制面解码未开 DisallowUnknownFields(新增可选字段安全)——**动手前先复核此事实** | `pkg/controlproto/envelope.go:55-101`、panel/node 两侧 payload 解码点 |
| panel 握手后无读超时,静默连接永不回收(C2 R10 移交) | `pkg/panel/transport.go:329-341`;`agentconn.go:117` LastSeen 无消费方 |

## Requirements

### R1 心跳间隔协商(M6)
- R1.1 `HelloPayload` 新增**可选**字段 `heartbeat_interval_ms`(int,毫秒)。node 在 hello 中携带本地配置的心跳间隔;省略/为 0 表示「未上报」(旧节点)。
- R1.2 panel 握手时记录该值到连接/节点状态;`SweepOffline` 与在线判定的阈值改为:**已上报节点**用 `上报间隔 × offline_multiplier`;**未上报节点**保持现状(panel 配置间隔 × multiplier),行为逐字节不变。
- R1.3 上报值合法性钳制:如 `<1s` 或 `>10min` 按未上报处理并 Warn,防恶意/故障节点把阈值推到无穷大。
- R1.4 hello 里节点自报 `node_id` 与证书身份不一致时(panel 侧 `transport.go:370-384` 目前忽略该字段)打 Warn 日志(仅可观测性,不改变以证书为准的语义)。

### R2 静默连接回收(C2 R10 移交项)
- R2.1 panel serve 读循环对已上报心跳间隔的节点设读超时:`上报间隔 × offline_multiplier + 30s` 裕量,超时即关闭连接走现有断连清理。
- R2.2 **未上报节点(旧 node)不设读超时**,维持现状——宁可不回收,不可误杀存量长心跳节点。待集群全量升级后自然生效。
- R2.3 回收只摘连接,DB online 状态仍由 `SweepOffline` 口径负责,两边不打架。

### R3 node 执行 `timeout_ms`(M7)
- R3.1 node 执行任务时以 `context.WithTimeout(serveCtx, time.Duration(TimeoutMS)×time.Millisecond)` 包裹;`TimeoutMS<=0` 视为不限(兼容不发该字段的假想对端,现网 panel 恒发 60000)。
- R3.2 超时即中止任务并回 `task_result`(state=failed,error 含 "timeout");任务实现须响应 ctx 取消——`storage_scan`/`storage_reconcile_apply` 的 WalkDir 循环每轮迭代检查 `ctx.Err()`(reconcile 中断是部分生效、重跑可续,已在父任务确认安全)。
- R3.3 保持同步执行模型(serve 循环内),**不做**任务异步化(那是另一个工程,超出本任务)。超时上限 60s 即 serve 循环最长阻塞时长,可接受。
- R3.4 panel 侧不改:`expireTask` 60s 判终止语义不变;node 超时回包若晚于 panel 判超时,结果按既有守卫丢弃——这是正常竞态结局,两端日志各自说明即可。

### R4 import_report 分页(M10,需协议 v2)
- R4.1 `pkg/controlproto` 版本上限升到 v2(下限仍 v1);v2 新增消息类型 `import_report_chunk`,payload:`{request_id, seq, total, state_chunk}`(credentials/buckets/webhooks 分块携带,单块序列化后 ≤ 512 KiB)。
- R4.2 **协商门控**:仅当握手协商结果 = v2 时 node 用分页上报;协商 = v1(旧 panel)时保持现有单帧行为(可能断连——不劣于现状,且 Warn 日志明确提示「panel 版本不支持分页导入」)。
- R4.3 panel 收到 `import_report_chunk` 按 request_id 重组,收齐 total 块后走现有导入落库路径;重组缓存设上限(如单 request 32 块/16 MiB)与超时(如 5 分钟),防恶意节点内存放大。panel 对 v1 节点仍接受旧单帧 `import_report`(现有限制下能过就过)。
- R4.4 版本号语义文档化:更新 `pkg/controlproto/version.go` 注释与 `docs/multi-node-operations.md` 的协议章节,v2 新增能力列清楚。

### R5 兼容性硬要求(本任务的红线)
- R5.1 新 node + 旧 panel:hello 多一个字段旧 panel 必须忽略(复核解码实现);协商出 v1,分页不启用;其余行为与旧 node 一致。
- R5.2 旧 node + 新 panel:阈值/读超时/导入全部走旧路径。
- R5.3 新 node + 新 panel:协商 v2,全部新行为生效。
- R5.4 三种组合各有一个集成测试或用例级论证。

## 部署顺序(写进发布说明)

1. **先升 panel**:新 panel 对旧 node 全兼容(R5.2),此刻集群行为与升级前一致。
2. **再滚动升 node**:逐台协商出 v2,新行为逐台生效。
3. 回滚任意一侧 = 换回旧二进制,协议自动退回 v1 行为,无状态残留。

## Acceptance Criteria

- [ ] AC1 `go build ./...`、`go vet ./...` 干净;`go test -race ./pkg/controlproto/... ./pkg/panel/... ./pkg/nodeagent/...` 全绿。
- [ ] AC2 R1:node 心跳 60s + panel 默认配置,新 panel 下节点持续 online 不抖动;旧 node(不带字段)行为与升级前一致。
- [ ] AC3 R2:模拟上报 15s 的节点握手后静默,~75s 内连接被回收;未上报节点静默不被回收。
- [ ] AC4 R3:注入一个永远阻塞的假任务,60s 内被 ctx 中止并回 failed(timeout);reconcile 中途取消不 panic、不产生半写的 sidecar 状态(重跑可续)。
- [ ] AC5 R4:v2 对端下构造 >1 MiB 的导入数据,分块传输成功落库,连接不断;重组超上限/超时拒绝并日志。
- [ ] AC6 R5 三组合:新node+旧panel、旧node+新panel、新node+新panel 各自的握手与心跳/任务/导入路径测试通过(v1/v2 协商可用 test 双端直接构造)。
- [ ] AC7 `git diff` 中 `pkg/controlproto/` 的变更仅为:HelloPayload 加可选字段、新增 chunk 消息类型、版本上限提升——**既有字段零改动**。
- [ ] AC8 `docs/multi-node-operations.md` 协议章节更新,包含 v2 能力与部署顺序。

## 依赖与排序

依赖 C2 完成(R2 建立在 C2 的写路径修复之上,避免读回收+写卡死叠加出新的半死状态);建议在 C1-C3 全部合并后最后实施。若 C2 尚未合并,本任务实施者需先确认 `agentconn` 写路径形态再动手 R2。
