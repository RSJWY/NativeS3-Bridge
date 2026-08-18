# 配置校验加固(弱密钥/scheme/默认值)

> 父任务:`.trellis/tasks/08-18-panel-node-interface-fixes`(部署安全红线见父任务 PRD,本任务不得违反)

## Goal

堵住两个高危配置陷阱(占位 session_secret 通过校验、node 控制面 URL 允许明文 scheme),顺带修复三处低危配置/测试缺陷。全部为**启动期校验/默认值**改动,不碰运行时行为与 wire 协议。

## 现状(实现前必读)

| 事实 | 位置 |
|---|---|
| 弱密钥黑名单,漏 `-value` 后缀变体 | `pkg/config/config.go:409-420` `isWeakSessionSecret` |
| panel 示例配置的占位值(42 字节,通过长度检查) | `configs/panel.example.yaml:58` `replace-with-a-random-32-byte-secret-value` |
| session_secret 直接用作 cookie HMAC 密钥 | `pkg/webadmin/auth.go:111` |
| `NodeConfig.Validate()` 对 `agent_url` 只查非空 | `pkg/config/node.go:132-171` |
| `ws→http` 推导说明 ws:// 是被代码接受的路径 | `pkg/nodeagent/register.go:217-224` `renewURLFromAgentURL` |
| 注册响应 CA 无认证落盘为信任根 | `pkg/nodeagent/register.go:265-268` |
| `public_healthz` 被无条件赋 true(非缺省填充) | `pkg/config/config.go:271`(panel 路径 `pkg/config/panel.go:119` 需一并核实) |
| 日期炸弹测试,当前已 FAIL | `pkg/panel/adminapi_certs_test.go:122` 硬编码 `time.Date(2026,8,16,...)` vs handler 走 `nowUTC()`(`pkg/panel/adminapi.go:430`) |
| node.yaml 含明文注册 token/DSN,无权限检查 | `pkg/config/node.go:71-85` `LoadNode` |

## Requirements

### R1 弱密钥黑名单补漏(H1)
- R1.1 在 `isWeakSessionSecret` 黑名单中补上 `replace-with-a-random-32-byte-secret-value`。**同时审视整个黑名单策略**:占位值类匹配应改为「包含常见占位词(如 `replace-with`、`change-me`、`example`、`todo`,大小写不敏感)即拒绝」,而不是逐串精确匹配,防止下一个变体再漏网。精确串保留作为兜底。
- R1.2 拒绝时的报错信息必须包含:「当前值疑似示例占位符」+ 生成安全随机密钥的命令提示(如 `openssl rand -base64 32`)。
- R1.3 `configs/panel.example.yaml:58` 保留占位值不动(它本来就是给人替换的模板),但在该行上方加注释:「直接复制此值将无法通过启动校验」。
- R1.4 核实 `natives3bridge` 单体与 `panel` 两条路径共用同一校验函数,修复后两边行为一致。

### R2 控制面 URL 强制加密 scheme(H2)
- R2.1 `NodeConfig.Validate()` 新增校验:`panel.agent_url` 必须是 `wss://`,`panel.register_url`(若非空)必须是 `https://`。`ws://`/`http://`/其他 scheme 一律拒绝,报错信息写明「明文 scheme 会使 mTLS 失效,存在 MITM 风险;如确属同机回环测试用途,请使用 …」。
- R2.2 为本地开发/测试留**显式逃生门**:新增可选配置键 `panel.allow_insecure_transport`(默认 `false`),显式置 true 时放行 `ws://`/`http://`,且启动时打 Warn 级日志。默认值 false 保证存量正确配置与忘记配置者都走安全路径。
- R2.3 核实现有测试/脚本(`scripts/test-panel-node-e2e.sh`、`scripts/test-upgrade-rollback.sh`、`cmd/node/main_test.go` 及 nodeagent 测试)是否有用 `ws://`/`http://` 的用例;有则改为显式设置逃生门或改用 wss,**不得为迁就测试而放宽校验**。
- R2.4 `renewURLFromAgentURL` 的 `ws→http` 分支在校验加严后成为理论死路径;保留该分支(配合 R2.2 逃生门仍有意义),不删。

### R3 `public_healthz` 缺省填充修正
- R3.1 `applyDefaults` 中对 `PublicHealthz` 的无条件赋值改为「指针/可判零值语义下,仅当用户未显式设置时才填 true」。若当前结构体是 `bool` 无法区分「未设置」与「显式 false」,改为 `*bool`(nil 时默认 true)——注意同步检查所有读取点的空值处理。**这是配置语义修复,显式写了 `public_healthz: false` 的存量部署在升级后行为会从 true 变 false,属预期修复,需在升级说明中点名。**

### R4 测试日期炸弹修复
- R4.1 `TestCertsRouteReturnsSnakeCaseDTO` 不得再硬编码日历日期断言 `days_until_expiry` 的具体区间。改法(任选,以与现有测试风格一致为准):构造证书 fixture 时以 `time.Now()` 为基准动态生成有效期;或引入可注入的时钟(若改动面大则选前者,禁止为本测试重构产品代码)。
- R4.2 修完后 `go test ./pkg/panel/...` 必须全绿。

### R5 node.yaml 权限警告(仅警告,不拦截)
- R5.1 `LoadNode` 成功解析后,若配置文件权限宽于 `0640`(如 0644/0666),打 Warn 日志:「配置文件含注册令牌/DSN 等敏感信息,建议 chmod 0600」。**只警告不拒绝启动**(避免破坏存量部署)。Windows/权限位不可用的平台静默跳过。
- R5.2 `configs/node.example.yaml` 顶部注释加一句权限建议。

## 升级前自查步骤(写进 implement 或随 PR 说明)

1. 检查线上 panel 配置 `session_secret` 是否仍是示例占位值——若是,先换成随机值再升级,否则新二进制**拒绝启动**(这是预期行为)。
2. 检查线上 node 配置 `agent_url`/`register_url` 的 scheme——若用了 `ws://`/`http://`,先评估是否确需明文(同机回环),确需则升级同时加 `allow_insecure_transport: true`,否则改 `wss://` 并确认证书链。
3. 检查是否有配置显式写了 `public_healthz: false`——升级后它将真正生效,确认 healthz 暴露策略符合预期。

## Acceptance Criteria

- [x] AC1 用 `configs/panel.example.yaml` 原样(仅改路径类字段)启动新 panel → 拒绝启动,错误信息明确指向 `session_secret` 为占位符并给出生成方法。
      **实测**:`panel -check-config` exit=1,报错 `webadmin.session_secret looks like an example placeholder (matched "replace-with-a-random-32-byte-secret-value"); ... Replace it with a random value: \`openssl rand -base64 32\``。
- [x] AC2 把占位值改成 `openssl rand -base64 32` 的输出 → 正常启动(回归保护:合法值不被误杀)。
      **实测**:同一份配置换随机 secret 后 exit=0 / `panel config check passed`。另有单测 `TestValidateAcceptsRandomSessionSecrets` 覆盖 base64/hex 两种生成形状。
- [x] AC3 node 配置 `agent_url: ws://127.0.0.1:9000/agent` 且未设逃生门 → 拒绝启动,报错含「mTLS 失效」说明;设 `allow_insecure_transport: true` → 启动成功并打 Warn。
      **实测**:无逃生门 exit=1,报错含 `which disables mTLS` 与 MITM 落盘信任根的说明;加逃生门后 exit=0 且打出 `level=WARN msg="panel.allow_insecure_transport is enabled..."`。
- [x] AC4 node 配置 `agent_url: wss://...`(存量正常配置)→ 正常启动,行为与升级前一致。
      **实测**:exit=0、无任何新增 Warn。全仓库脚本/测试的 agent_url/register_url 已 grep 核对,无一使用 `ws://`/`http://`(R2.3 因此无需改动任何测试)。
- [x] AC5 yaml 显式写 `public_healthz: false` → 生效(healthz 需鉴权);不写 → 默认 true,与升级前一致。
      **实测**(单体 `natives3bridge` 真实起进程 + curl):显式 false → `GET /healthz` 404;不写 ops 块 → 200 `ok`。
      **注**:panel 二进制当前根本不挂载 `/healthz`(`cmd/panel/main.go` 与 `pkg/panel/adminserver.go` 无 ops 路由),故 `pkg/config/panel.go` 那处修复今天是防御性的,真正可观测的是单体路径。
- [x] AC6 `go test ./pkg/panel/... ./pkg/config/...` 全绿;把系统时钟意念调整到任意未来日期,`TestCertsRouteReturnsSnakeCaseDTO` 仍通过(即断言不依赖具体日历日)。
      **实测**:全套 `go test ./...` 绿。日期无关性由构造保证——fixture 全部相对 `time.Now()` 生成,且刻意把过期量取在 15.5 天(整天桶中间)使断言恒为 -16,不再有日历字面量。
      **未做**:本机无 `faketime`,没有在真实改期的时钟下跑过。
- [x] AC7 node.yaml 权限 0644 启动 → 有 Warn 日志;0600 → 无 Warn;两种情况下都正常启动。
      **实测**:0644 → `level=WARN msg="node config file permissions are too permissive" mode=0644`、exit=0;0600 与边界值 0640 → 无 Warn、exit=0。
- [x] AC8 无 DB schema 变更、无 wire 协议变更、无既有配置键删除或改名。
      **核对**:diff 未触碰任何 migration/controlproto 文件;`public_healthz` 键名不变(仅 Go 类型 bool→*bool,YAML 表示不变);唯一新增键为 `panel.allow_insecure_transport`,在父任务冻结集合内。

## 实施记录(2026-08-18)

- 落盘范围与 R1-R5 一一对应,无超范围改动。
- **额外必需改动(超出 PRD 字面但为 AC5 所必需)**:`pkg/webadmin/ops.go` 的 `NewOpsHandler` 里还有第二处 healthz 覆盖——「三项全 false 且无 metrics token 就把 healthz 掰回 true」。panel/单体配置只写 `public_healthz: false` 时正好命中这个形状,会二次吞掉用户的显式 false。指针语义已使该兜底冗余,故删除。
- **红线 #2 的例外**:`allow_insecure_transport` 默认 false 意味着 `ws://` 存量配置升级后会被拒绝启动,这与「新增键默认值等于现状行为」字面冲突,但正是 H2 要求的加严,由红线 #3(加严类改动需配升级前自查步骤)授权。自查步骤见上。
- **复核阶段自查出并修掉的误报**:R5 的权限判定最初写成 `mode &^ 0640 != 0`,把属主位/执行位也算进暴露面,`chmod 0700`(rwx------,别人根本读不到,比 0640 更严)会被倒过来警告成「权限过宽」。改为只看真正造成暴露的位 `0o026`(组可写 + 其他用户可读写),并把 `{0o700, false}`、`{0o750, false}` 钉进测试表防回归。
- 未运行 `scripts/test-panel-node-e2e.sh`(耗时过长);`scripts/test-distribution-contract.sh` 已跑通。
- **spec 沉淀**:三条可复用教训写入 `.trellis/spec/backend/quality-guidelines.md`(此前是空模板)——F1 日期炸弹判定规则、F2 可选布尔配置项必须查第二处覆盖点、F3 权限检查看暴露位而非模式上限;另有 R1 占位值特征词匹配、R2 启动期告警时序、R3 运行时字符串用英文。

## 复杂度判定

轻量偏中,PRD-only + 实施者自查清单(见上「升级前自查步骤」)。实施窗口直接按 R1-R5 逐条落地即可。
