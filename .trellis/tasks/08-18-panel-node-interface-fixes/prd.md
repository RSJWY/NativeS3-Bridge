# Node/Panel 对外接口缺陷修复(安全审计后续)

## 背景与需求来源

2026-08-18 对 node 与 panel 的对外接口做了四路并行代码审计(panel 节点侧、panel 管理侧、nodeagent 控制面、启动配置与协议契约),确认了一批真实 bug。本父任务承载全部审计结论、子任务划分、跨子任务验收标准与部署安全红线。

**关键约束:服务已实际部署运行。** 所有修复必须保证:同版本两端互认不破坏、滚动升级期间新旧版本混跑不破坏、升级后有明确回滚路径、不因配置校验加严而把存量正常配置挡在启动门外(存量配置若命中新校验,必须有升级前检查步骤兜底)。

## 审计结论总表(权威清单)

### 高危

| # | 问题 | 位置 | 归属子任务 |
|---|---|---|---|
| H1 | `panel.example.yaml` 占位 `session_secret` 通过弱密钥校验(黑名单漏 `-value` 后缀变体),可伪造 admin 会话 | `configs/panel.example.yaml:58`、`pkg/config/config.go:414` | C1 |
| H2 | node `agent_url`/`register_url` 不强制加密 scheme,`ws://`/`http://` 下 mTLS 消失、注册 CA 可被 MITM 落盘为信任根 | `pkg/config/node.go:132-171`、`pkg/nodeagent/register.go:265-268` | C1 |

### 中危

| # | 问题 | 位置 | 归属子任务 |
|---|---|---|---|
| M1 | panel 模式 `trustForwarded` 无配置入口,反代部署下登录限流按代理 IP 聚合,可远程 DoS 管理员 | `cmd/panel/main.go:108`、`pkg/panel/adminserver.go:51`、`pkg/webadmin/auth.go:39,137` | C2 |
| M2 | 管理 HTTP server 只有 `ReadHeaderTimeout`,慢速 body 攻击耗尽连接 | `pkg/panel/adminserver.go:75-79` | C2 |
| M3 | 控制面写路径 `writeMu` 锁等待不感知 ctx + 心跳 ack 无超时,可永久卡死推送/任务下发 | `pkg/panel/agentconn.go:78-80`、`pkg/panel/transport.go:463`、`pkg/panel/tasks.go:128` | C2 |
| M4 | `Dispatch` 后 `markState(running)` 无状态守卫,竞态把终态任务覆盖回 running | `pkg/panel/tasks.go:138,225-235` | C2 |
| M5 | 心跳无频率限制,每帧放大为一次 SQLite 写 + 一次回写 | `pkg/panel/transport.go:457-466` | C2 |
| M6 | 心跳间隔双边独立配置、协议无协商,错配导致节点 online/offline 抖动 | `pkg/controlproto/payloads.go:20-28`、`pkg/panel/transport.go:834-845`、`pkg/config/node.go:125-127` | C4 |
| M7 | `timeout_ms` 契约不对称:panel 填并发、node 完全不执行 | `pkg/panel/tasks.go:120-127`、`pkg/nodeagent/tasks.go`、`pkg/nodeagent/client.go:506-539` | C4 |
| M8 | node 续期/注册返回证书不校验即非原子覆盖落盘,可永久失联 | `pkg/nodeagent/client.go:313-322`、`pkg/nodeagent/register.go:254-269,286` | C3 |
| M9 | node 握手/serve 循环无读超时,挂死 panel 可永久钉住 agent | `pkg/nodeagent/client.go:231-249,355-401` | C3 |
| M10 | `import_report` 无大小上限,超 panel 1 MiB 读限制即断连,大节点迁移导入死循环 | `pkg/nodeagent/client.go:482-501`、`pkg/panel/agentconn.go:17` | C4 |

### 低危(全部纳入,见各子任务 PRD)

- C2:`updateNode`/`retireNode` 双写响应(`adminapi.go:271,307`);`certsRoute` 不校验节点存在(`adminapi.go:417-453`);`tokens`/`certs/revoke` 不检查多余路径段(`adminapi.go:150,438`);hash mismatch 映射 500(`adminapi.go:708-714`);退役节点仍可建草稿资源(`adminapi.go:475-480`);`/renew` 无频率限制(`transport.go:162-232`);hello 的 `content_hash` 未消毒落库(`transport.go:684-691`);瞬时 DB 错误拆连接(`transport.go:337-340`)。静默连接无回收(`transport.go:329-341`)**移交 C4**(需心跳协商提供安全依据,C2 PRD R10 有说明)。
- C3:续期后主动断连被当失败处理、重连延迟 ~90s(`client.go:147-162`);畸形 task 缺 `task_id` 被执行并污染幂等台账(`client.go:506-538`);收到 envelope 不校验 `version`;`--health` 探针对任意 HTTP 服务判存活、`"[::]"` 死分支(`cmd/node/main.go:150-179`);node.yaml 敏感信息无权限检查(`pkg/config/node.go:71-85`)。
- C1:`public_healthz: false` 被无条件覆盖为 true(`pkg/config/panel.go:119`、`pkg/config/config.go:271`);测试 `TestCertsRouteReturnsSnakeCaseDTO` 硬编码日期,2026-08-17 后必挂(`pkg/panel/adminapi_certs_test.go:122`)——**当前测试套件已红,优先修**。

### 明确排除(审计已核实无问题,不得在修复中"顺手"改动)

- 注册 token 原子消费/重放绑定/ConstantTimeCompare;CSR 模板化签发;1 MiB 帧上限;管理路由鉴权覆盖;SQL 全参数化;draft 条纹锁与条件更新;版本协商 `NegotiateVersion` 逻辑;content hash 双侧规范化对称。
- `ConsumeRegistrationToken` 无生产调用方是死代码,**不在本任务删除**(避免无关 diff)。
- Dashboard N+1(`dashboard.go:126-150`)是性能问题不是 bug,不在本任务范围。

## 子任务映射

| 子任务 | 范围 | 部署单元 | 协议变更 |
|---|---|---|---|
| `08-18-config-validation-hardening`(C1) | H1、H2、public_healthz、测试日期炸弹、node.yaml 权限警告 | 两边(仅启动期校验) | 无 |
| `08-18-panel-interface-hardening`(C2) | M1-M5 + panel 侧全部低危 | 仅 panel | 无 |
| `08-18-nodeagent-hardening`(C3) | M8、M9 + node 侧低危 | 仅 node | 无 |
| `08-18-controlproto-contract-fixes`(C4) | M6、M7、M10 | 两边 | **有(需向后兼容设计)** |

实施顺序:C1 → C2 → C3 → C4。C1/C2/C3 互不依赖可并行,但 C4 涉及协议演进放最后;每个子任务独立验收、独立提交。

## 部署安全红线(所有子任务必须遵守)

1. **不改 wire 协议的既有字段语义**;新增字段必须可选,旧端收到须忽略(先核实控制面解码未开 `DisallowUnknownFields`,若开了则新增字段方案作废,改走版本协商)。
2. **不改配置文件的既有键名/默认值语义**;新增配置键必须有与原行为一致的默认值。**本批修复的新增配置键全集已冻结**:`panel.allow_insecure_transport`(C1)、`webadmin.trust_forwarded`(C2)、`task_timeout`(C4),三者默认值均等于现状行为;其余阈值一律硬编码或从既有配置推导,取舍理由记录在各子任务 design.md,实施中不得临时新增配置键(确有需要的先回报父任务评审)。
3. **校验加严类改动**(H1 黑名单、H2 scheme 白名单)必须在 implement.md 里写"升级前配置自查步骤",并在报错信息里给出明确修复指引。
4. **行为默认值不变**:限流/超时类新增机制的默认阈值不得改变现有正常流量下的可观察行为(如心跳节流阈值必须高于协议正常心跳频率)。
5. 每个子任务的 implement.md 必须包含:构建命令、受影响二进制的滚动升级顺序、回滚方式(二进制回退即可,不得有 DB schema 变更;**本父任务下禁止任何数据库迁移**)。
6. DB 只读/既有列写入,不增列不增表不改索引——若某修复被认为必须改 schema,停下来回报父任务重新评估。

## 跨子任务验收标准(集成验收)

- [ ] X1 `go build ./...` 通过;`go vet ./...` 干净。
- [ ] X2 `go test ./...` 全绿(含修复后的 `TestCertsRouteReturnsSnakeCaseDTO`,且不再依赖真实时钟日期)。
- [ ] X3 `go test -race ./pkg/panel/... ./pkg/nodeagent/... ./pkg/controlproto/...` 通过。
- [ ] X4 兼容性自证:C2 只动 panel、C3 只动 node,C4 的协议变更提供"新 panel + 旧 node"与"旧 panel + 新 node"两个方向的测试或论证。
- [ ] X5 升级前自查脚本/步骤验证:用 `configs/panel.example.yaml` 原样启动新 panel 二进制,必须**拒绝启动**并打印指向 session_secret 的明确错误;用 `agent_url: ws://...` 启动新 node,必须拒绝启动并提示改用 `wss://`。
- [ ] X6 现有 shell 测试套件不被破坏:`scripts/test-panel-node-e2e.sh`、`scripts/test-distribution-contract.sh`(若本机环境可运行)。
- [ ] X7 审计清单中的每一项在对应子任务 PRD 的验收标准里都有勾选项,无遗漏、无超范围改动。

## 交接说明

代码落盘在另一窗口执行。实施窗口应先 `task.py start <子任务目录>` 再动手;每个子任务按自身 `implement.md` 执行,完成后跑该任务的验收命令,全部子任务完成后回到本文件核对 X1-X7。
