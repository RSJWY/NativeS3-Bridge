# NodeAgent 控制面连接加固

> 父任务:`.trellis/tasks/08-18-panel-node-interface-fixes`。**部署策略变更(2026-08-18,用户裁决,覆盖本文件与父任务 PRD 的原兼容性要求):panel 与 node 同步升级,不做新旧混跑兼容。**
>
> 原先要求"只改 node 二进制、panel 侧零感知、新旧 panel 均兼容"**已作废**。本任务仍只改 node 二进制,但协议版本不匹配时快速失败断连,不保留兼容分支。回滚须两侧成对执行;无 DB schema 变更。

## Goal

修复 nodeagent 的 2 个中危(证书落盘不校验/非原子、连接无读超时)与 4 个低危(续期断连误报、畸形 task 污染台账、envelope version 不校验、health 探针失真),保证节点在 panel 异常/网络半开时能自愈,在 panel 返回坏数据时不自残。同步升级策略下,版本不匹配即断连重试,不降级、不静默。

## 现状(实现前必读)

| 事实 | 位置 |
|---|---|
| 续期成功路径:仅查非空即覆盖落盘 | `pkg/nodeagent/client.go:313-322` |
| 注册成功路径:CA 与证书同样无校验落盘 | `pkg/nodeagent/register.go:254-269` |
| `persistPEM` 用 `os.WriteFile` 直接截断写 | `pkg/nodeagent/register.go:286` |
| 重连主循环与退避 | `pkg/nodeagent/client.go:111-130` |
| dial 超时只覆盖建连,handshake/serve 用长生命周期 ctx | `pkg/nodeagent/client.go:231-249,355-401` |
| serve 循环读无 deadline、无 ack watchdog | `pkg/nodeagent/client.go:405-431` |
| 心跳发送失败仅 `return`,不关连接不 cancel | `pkg/nodeagent/client.go:594-596` |
| 续期后主动断连走失败分支:Warn + 退避延迟 | `pkg/nodeagent/client.go:147-162,607-611` |
| `handleTask` 不校验 `task_id` 非空;幂等台账以 task_id 为唯一键 | `pkg/nodeagent/client.go:506-538`、`pkg/nodeagent/state.go:38-45` |
| `DecodeEnvelope`/serve 分发不校验 `version` | `pkg/controlproto/envelope.go:55-101`、`pkg/nodeagent/client.go:405-431` |
| `--health` 探针:状态码检查恒真、`"[::]"` 死分支、公网地址带 InsecureSkipVerify | `cmd/node/main.go:150-179` |
| 心跳间隔节点本地配置(默认 15s) | `pkg/config/node.go:125-127` |

## Requirements

### R1 证书落盘前校验(M8)
- R1.1 续期(`renewCertificate`)与注册(`RegisterContext`)对 panel 返回的 `cert_pem` 在**落盘前**完成四项校验:可解析为 X.509;证书公钥与本次 CSR 所用私钥匹配;能被当前本地 CA 池验证链(注册场景:先用响应内 `ca_cert_pem` 自洽验证——证书须由该 CA 签发,且 CA 本身可解析;MITM 防护由 C1 的 https 强制 + 注册令牌机密性承担,本项只防坏数据);`NotAfter` 在未来且不超过合理上界(如 10 年,防 panel 异常签发世纪证书)。任一失败:**保留旧文件不动**,返回错误走现有重试/告警路径。
- R1.2 校验要用到响应里的 `not_after` 字段与返回证书实际值交叉核对(不一致以证书为准并 Warn,不阻断)。

### R2 PEM 原子落盘(M8)
- R2.1 `persistPEM` 改为 同目录临时文件 + `fsync` + `os.Rename` 三步;私钥保持 0600、证书 0644 语义不变。
- R2.2 写新证前先把旧文件备份为 `<name>.bak`(同目录,覆盖式单份),rename 成功后保留 `.bak`;下次 `clientTLS` 加载失败时的自救逻辑**不做**(超出范围),但 `.bak` 为人工恢复留了路。若实施者认为 `.bak` 增加运维噪音,允许省略,design.md 记录取舍——原子写本身已消除截断损坏。

### R3 握手与 serve 读侧超时(M9)
- R3.1 `handshake` 整体包独立超时(如 30s),超时即视为建连失败走现有退避重连。
- R3.2 serve 期间加「收帧看门狗」:goroutine 监控最后一次成功 `Read` 的时间,超过 `max(3 × heartbeat_interval, 60s)` 无任何帧(ack/desired/task 都算)即 cancel serveCtx 触发重连。阈值基于**节点本地** heartbeat_interval,与 panel 配置无关,新旧 panel 均安全(正常 panel 每心跳必回 ack,静默必然异常)。
- R3.3 心跳发送失败时(`client.go:594-596`)必须关闭 ws 并 cancel serveCtx,让 Run 立即重连,而不是空挂。
- R3.4 看门狗误杀防护:看门狗计时必须以「收到任意帧」重置,不是只看 ack。

### R4 续期后主动断连的语义修正(L1)
- R4.1 续期成功 → 主动断连 → 用新证立即重连(退避重置为最小值),日志从 Warn 降为 Info 并写明「续期完成,按预期重连」。
- R4.2 顺带修正 `Run` 中 `err == nil` 才重置退避的死代码分支:区分「计划内断连」与「异常断连」,计划内(续期)立即重连,异常保持现有退避。

### R5 畸形 task 防护(L3)
- R5.1 `handleTask` 入口校验:`task_id` 为空或 `type` 不在已知枚举内 → 不执行、不写幂等台账,回 `task_result`(state=failed,error 说明原因)若 task_id 非空;task_id 为空则仅 Warn 日志(无法回包)。
- R5.2 台账缓存命中时校验缓存条目的 type 与本次一致,不一致视为新任务执行(防同 id 换 type 重放)。

### R6 envelope version 校验(L4)
- R6.1 serve 分发时校验 `env.Version`:高于本端支持版本 → Warn + 丢弃该帧(不执行);缺失/非法 → Warn + 丢弃。**不断连**——这是逐帧的防御性校验(防畸形/错版帧被当正常帧执行),与握手期的版本协商是两件事。握手协商失败该不该断连由 C4 决定(同步升级下:失败即断连),这里只管已建立连接上单帧的合法性。
- R6.2 hello_ack 协商出的版本号此后作为分发基准记录在连接状态里,供 R6.1 使用。

### R7 `--health` 探针修正(L5/L7)
- R7.1 删除 `"[::]"` 死分支(`net.SplitHostPort` 返回的 host 不含方括号)。
- R7.2 探针必须能区分「我们的 S3 网关」与「占用端口的别的 HTTP 服务」:对 `GET /` 的响应校验是否为本网关的 S3 XML 错误结构(`<Error>` 且含预期 Code,如 AccessDenied/InvalidRequest——以当前 gateway 实际返回为准);不匹配则探针失败。实现时先在 design.md 记录当前 `GET /` 的真实响应体样例。
- R7.3 仅当监听地址为通配符时归一到 127.0.0.1;绑定具体地址时向该地址探测,并移除 `InsecureSkipVerify`(若 S3 监听是 https 且证书对探测地址不可验,则在 design.md 记录后允许对 loopback 保留跳过,公网地址不允许)。

## 部署与协议不匹配语义

- **升级方式**:panel 与全部 node 同步升级,不承诺混跑期行为。控制面在升级窗口内可能失联,但各 node 继续用最后一次落地的本地配置服务 S3,业务不中断。
- **不匹配时快速失败**:版本协商失败即断开,node 侧带退避持续重连,日志必须明确写出"panel 与 node 协议版本不匹配,需同步升级"(含两侧版本号),不允许静默重试、不降级。
- **回滚**:两侧二进制同时回退。无 DB schema 变更,无状态残留。只回滚一侧会导致协议不匹配、控制面持续失联(数据面仍在服务),因此回滚必须成对执行。
- **仍然必须保留的防护**(与版本无关,防的是恶意或故障对端):R1 证书校验、R3.5 node 侧 `timeout_ms` 十分钟上界钳制、R4.3 重组缓存上限(由 controlproto 子任务负责)。这些不能因"兼容"而删除。

## Acceptance Criteria

- [x] AC1 `go build ./...`、`go vet ./...` 干净;`go test -race ./pkg/nodeagent/... ./pkg/controlproto/... ./cmd/node/...` 全绿。
- [x] AC2 构造 mock panel 返回坏证书(不可解析/公钥不匹配/链不合法,各一例):节点拒绝落盘,旧证书文件逐字节不变,错误日志明确。
- [x] AC3 `kill -9` 于写盘时机无法用单测模拟,改为:构造临时目录,断言 `persistPEM` 写完后存在完整 PEM 且过程中出现过的临时文件已清理;`.bak` 策略按 R2.2 决策验证。
- [x] AC4 mock panel 接受 WS 后不应答 hello → 节点在握手超时内放弃并重连(测试断言耗时 < 超时上限)。
- [x] AC5 mock panel 建立连接后完全静默 → 节点在看门狗阈值内断开重连;正常每 15s 心跳+ack 的连接永不误杀(跑 3 分钟以上模拟或时钟注入)。
- [ ] AC6 心跳发送注入失败 → 连接立即关闭并进入重连。(注:真实 WebSocket 写失败难以注入,已在实现层保证发送失败即 Close+return;留待后续集成测试覆盖。)
- [ ] AC7 续期成功路径日志为 Info,且从断连到用新证重连的间隔 ≈ 最小退避(非最长 90s)。(注:已在实现层引入 errRenewedReconnect 哨兵与退避清零;留待后续集成测试覆盖完整路径。)
- [x] AC8 无 task_id 的 task:不执行、台账无 `""` 记录、有 Warn;同 id 换 type:不被缓存命中跳过。
- [x] AC9 高于支持版本的 envelope 被 Warn+丢弃,连接保持。
- [x] AC10 端口被普通 HTTP 服务(如 `python3 -m http.server`)占用时 `node --health` 返回非零;被真实网关监听时返回 0。
- [x] AC11 `git diff` 不涉及 `pkg/panel/`、`pkg/controlproto/` 的 wire 字段、任何 DB schema。
