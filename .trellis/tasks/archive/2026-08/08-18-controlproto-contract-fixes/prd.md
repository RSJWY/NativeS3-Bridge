# 控制协议契约修复(超时/心跳协商/导入分页)

> 父任务:`.trellis/tasks/08-18-panel-node-interface-fixes`。**本任务涉及 wire 协议演进,是父任务下唯一允许动 `pkg/controlproto/` 的子任务**。
>
> **⚠️ 部署策略变更(2026-08-18,用户裁决,覆盖本文件与父任务 PRD 的原兼容性要求):panel 与 node 同步升级,不做新旧混跑兼容。**
>
> 原先要求"所有新增向后兼容、新旧任意混跑不得比现状更差"(以及父任务 X4 的双向验证、第 62 条的滚动升级顺序),**均已作废**。取而代之的约定见下方「部署与协议不匹配语义」。这不是放松要求,而是把复杂度从"双轨兼容逻辑"转移到"部署纪律";因此**恶意/故障对端防护一条都不能删**——那些防的不是版本差异。

## 部署与协议不匹配语义(替代原兼容性红线)

- **升级方式**:panel 与全部 node 同步升级,不承诺混跑期行为。协议版本上限与下限**同时**升到 v2(不再保留 v1 下限)。
- **不匹配时快速失败**:版本协商失败即断开,node 侧带退避持续重连,日志必须明确写出"panel 与 node 协议版本不匹配,需同步升级"(含两侧版本号),不允许静默重试。
- **风险边界(已核实,写给实施者)**:控制面断开**不影响 S3 数据面**——node 继续用最后一次落地的本地配置服务 S3(`cmd/node/main.go:5` 的设计注释、`pkg/nodeagent/client.go:102`)。所以协议不匹配期间是"管理面失联",不是"业务中断";升级窗口内的失联可接受,升完自动恢复。**不要**因此把不匹配当 P0 去设计降级通道。
- **回滚**:两侧二进制同时回退。仍然**禁止任何 DB schema 变更/迁移**(父任务红线此条继续有效),保证回退无数据包袱。
- **仍然必须保留的防护**(与版本无关,防的是恶意或故障对端):R1.3 心跳间隔区间钳制、R3.5 node 侧 `timeout_ms` 十分钟上界钳制、R4.3 重组缓存的块数/字节/超时上限。删掉任何一条都会让单个故障节点能把 panel 拖坏。


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
- R1.1 `HelloPayload` 新增字段 `heartbeat_interval_ms`(int,毫秒)。同步升级下 node **必填**;缺省/为 0 视为协议违规,按 R1.3 的非法值处理(不再有"旧节点未上报"这一类)。
- R1.2 panel 握手时记录该值到连接/节点状态;`SweepOffline` 与在线判定的阈值统一用 **上报间隔 × offline_multiplier**,不再保留"按 panel 本地配置间隔"的第二套口径。
- R1.3 **上报值合法性钳制(必须保留)**:`<1s` 或 `>10min` 一律拒绝——记 Warn 并回落到 panel 配置间隔。这防的是**故障或恶意节点**上报一个巨大值把离线阈值推到无穷大,从而永远不被判离线,与版本兼容无关。
- R1.4 hello 里节点自报 `node_id` 与证书身份不一致时(panel 侧 `transport.go:370-384` 目前忽略该字段)打 Warn 日志(仅可观测性,不改变以证书为准的语义)。

### R2 静默连接回收(C2 R10 移交项)
- R2.1 panel serve 读循环对所有节点设读超时:`上报间隔 × offline_multiplier + 30s` 裕量,超时即关闭连接走现有断连清理。
- R2.2 上报值非法而回落到 panel 配置间隔的连接(见 R1.3),读超时按回落后的间隔计算——同步升级后不存在"不上报间隔"的合法节点,因此**不再保留"不设读超时"的豁免分支**(这正是 C2 当初把 R10 移交过来的原因:固定读超时会误杀长心跳节点,而协商后阈值随节点自报值走,该风险消失)。
- R2.3 回收只摘连接,DB online 状态仍由 `SweepOffline` 口径负责,两边不打架。

### R3 node 执行 `timeout_ms`(M7)
- R3.1 node 执行任务时以 `context.WithTimeout(serveCtx, time.Duration(TimeoutMS)×time.Millisecond)` 包裹。同步升级下 panel 恒发正值(默认 60000);`TimeoutMS<=0` 视为协议违规,按 R3.5 的默认值执行并 Warn,**不再**视为"不限时长"(那是留给旧对端的语义,现已作废,且"不限"本身就是个隐患)。
- R3.2 超时即中止任务并回 `task_result`(state=failed,error 含 "timeout");任务实现须响应 ctx 取消——`storage_scan`/`storage_reconcile_apply` 的 WalkDir 循环每轮迭代检查 `ctx.Err()`(reconcile 中断是部分生效、重跑可续,已在父任务确认安全)。
- R3.3 保持同步执行模型(serve 循环内),**不做**任务异步化(那是另一个工程,超出本任务)。超时上限即 serve 循环最长阻塞时长,默认 60s 可接受。
- R3.4 panel 侧判终止语义不变(`expireTask` 按下发时的 timeout 判超时);node 超时回包若晚于 panel 判超时,结果按既有守卫丢弃——这是正常竞态结局,两端日志各自说明即可。
- R3.5 panel 新增配置键 `task_timeout`(`time.Duration`,默认 60s,≤0 或缺省回落默认),接线 `cmd/panel/main.go` → adminserver deps → tasksRoute 的 `Dispatch` 调用点(`Dispatch` 本就接受 timeout 参数,`tasks.go:51-54` 非正数回落常量,此处只是改为注入配置值)。这是 C4 让 node 真正执行 `timeout_ms` 后给「合法长任务」(大桶全量扫描)留的出口。node 侧对 `timeout_ms` 设硬编码上界钳制 10 分钟:超出按上界执行并 Warn(防故障/恶意 panel 让任务无限占住 serve 循环)。

### R4 import_report 分页(M10,协议 v2)
- R4.1 `pkg/controlproto` 版本上限与下限**同时**升到 v2;v2 新增消息类型 `import_report_chunk`,payload:`{request_id, seq, total, state_chunk}`(credentials/buckets/webhooks 分块携带,单块序列化后 ≤ 512 KiB)。
- R4.2 分页是 v2 的唯一上报形态,node 恒用分页,**不保留单帧回退分支**。协商失败(对端只支持 v1)按「部署与协议不匹配语义」快速失败断连并给出需同步升级的日志,而不是降级成单帧。
- R4.3 panel 收到 `import_report_chunk` 按 request_id 重组,收齐 total 块后走现有导入落库路径。**重组缓存上限必须保留**:单 request 32 块 / 16 MiB、5 分钟超时,超限即丢弃并断连。这防的是**恶意节点用不收尾的分块做内存放大**,与版本无关。旧单帧 `import_report` 的接收路径可以删除(v1 已不在支持范围)。
- R4.4 版本号语义文档化:更新 `pkg/controlproto/version.go` 注释与 `docs/multi-node-operations.md` 的协议章节,写明 v2 新增能力**以及 v1 已不再受支持、两端必须同步升级**。

### R5 同步升级下的两端一致性要求(替代原兼容性红线)
- R5.1 v2 两端:协商成功,心跳阈值随节点自报值、读超时生效、`timeout_ms` 被执行、导入走分页。
- R5.2 版本不匹配(任一侧仍是 v1):握手即失败断连,两侧日志各自打印本端与对端版本并指明需同步升级;node 带退避持续重连,**不降级、不静默**。
- R5.3 不匹配期间 node 的 S3 数据面必须照常服务(用最后一次落地的本地配置)。这一点是硬切换可接受的前提,必须有测试或明确论证。
- R5.4 上面三条各有一个集成测试或用例级论证(R5.2 可用 test 双端直接构造版本号)。

## 部署顺序(写进发布说明)

1. **panel 与全部 node 同步升级**,不承诺混跑期行为。升级窗口内控制面可能失联(协议不匹配即断连),此时各 node 继续用最后一次落地的配置服务 S3,业务不中断。
2. 升级完成后各 node 自动重连成功,协商出 v2,新行为全量生效。
3. 回滚 = **两侧二进制同时**回退。无 DB schema 变更,无状态残留。**注意**:只回滚一侧会导致协议不匹配、控制面持续失联(数据面仍在服务),因此回滚必须成对执行。

## Acceptance Criteria

- [x] AC1 `go build ./...`、`go vet ./...` 干净;`go test -race ./pkg/controlproto/... ./pkg/panel/... ./pkg/nodeagent/...` 全绿。
- [x] AC2 R1:node 心跳 60s + panel 默认配置,新 panel 下节点持续 online 不抖动;上报值非法(如 0 / 1h)时回落到 panel 配置间隔并 Warn,不被永久判为在线。
- [x] AC3 R2:模拟上报 15s 的节点握手后静默,~75s 内连接被回收;上报 60s 的节点在 ~75s 时**不**被回收(证明阈值随自报值走,而不是固定值误杀长心跳节点)。
- [x] AC4 R3:注入一个永远阻塞的假任务,60s 内被 ctx 中止并回 failed(timeout);reconcile 中途取消不 panic、不产生半写的 sidecar 状态(重跑可续)。
- [x] AC4b R3.5:panel.yaml 设 `task_timeout: 300s` → 下发的 `timeout_ms` = 300000,node 按 300s 执行;不设该键 → 60s;panel 下发 `timeout_ms` > 10min 或 ≤0 时 node 分别按 10min / 默认值执行并 Warn。
- [x] AC5 R4:构造 >1 MiB 的导入数据,分块传输成功落库,连接不断;重组超块数/字节/超时上限时拒绝并日志。
- [x] AC6 R5:v2 两端全路径通过;版本不匹配时握手失败且日志含两端版本号与"需同步升级"提示;不匹配期间 node 的 S3 读写仍成功。
- [x] AC7 `git diff` 中 `pkg/controlproto/` 的变更仅为:HelloPayload 加心跳间隔字段、新增 chunk 消息类型、版本上下限提升(v1 支持移除)——**其余既有字段零改动**。
- [x] AC8 `docs/multi-node-operations.md` 协议章节更新,包含 v2 能力、v1 不再受支持、以及"必须同步升级/成对回滚"的部署说明。

## 依赖与排序

依赖 C2 完成(R2 建立在 C2 的写路径修复之上,避免读回收+写卡死叠加出新的半死状态);建议在 C1-C3 全部合并后最后实施。若 C2 尚未合并,本任务实施者需先确认 `agentconn` 写路径形态再动手 R2。
