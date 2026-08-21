# 证书到期可观测性

> 父任务：`.trellis/tasks/08-16-node-cert-lifecycle`（承重决策 D1/D2/D3 与跨子任务 AC 见父 PRD，不得重新裁决）
> 与 `08-16-cert-auto-renew`、`08-16-panel-server-cert-renew` 并行，无硬依赖。它管「让证书自动续上」，本任务管「让到期风险被看见、让失败不再静默」。

## Goal

把证书到期风险变成**看得见**的东西：admin API 给出剩余天数与状态，UI 从两态改三/四态，dashboard 有聚合，节点侧证书类永久错误从「一条通用 warn」变成「Error 级 + 指明恢复动作」。对应父任务 AC2（无静默失败）。

## 现状（实现前必读）

### panel 侧

| 事实 | 位置 |
|---|---|
| `GET /certs` **裸序列化 `[]NodeCert` model**，`writeTransportJSON(w, 200, certs)` | `pkg/panel/adminapi.go:347-362`，返回在 `:361` |
| `NodeCert` **没有任何 json tag** → 前端拿到的是 Go 大驼峰字段名 | `pkg/panel/models.go:44-55` |
| 对照：注册令牌那边有规范的 `ExpiresAt time.Time \`json:"expires_at"\`` | `pkg/panel/adminapi.go:315` 附近 |
| `dashboard.go` **对证书零引用**（249 行全文无 cert 字段） | `pkg/panel/dashboard.go` |
| **可复用**：dashboard 已有 `Totals.Attention` + `AttentionNodes` + 后端派生的 `Severity` 机制 | `dashboard.go:18,31-45,47-52`，severity 常量 `:82-95`，`attentionSeverity` 函数、关注判定 `:155-169` |
| **可复用**：`dashboardTelemetry` 已有 valid/missing/stale 三态聚合先例 | `dashboard.go:62-68` |
| 关键设计注释（要遵循的既有约定）：「额外的 severity 是后端派生的展示排序键，**不让前端复制业务判断**」 | `dashboard.go:29-30` |
| `/metrics` 只有健康/就绪，无任何业务 gauge | `pkg/webadmin/ops.go:17` |

### 前端

| 事实 | 位置 |
|---|---|
| 证书表格 4 列：Serial / 指纹 / NotAfter / 状态 | `pkg/webadmin/ui/src/views/PanelNodeDetail.vue:96-102` |
| **状态列只按 `Revoked` 渲染两态** → 已过期未吊销的证书显示「有效」 | `PanelNodeDetail.vue:102` |
| `activeCertificateCount` = `filter(c => !c.Revoked)` → **把过期证书算作有效**，直接影响「撤销全部有效证书」按钮的 disabled 判断 | `PanelNodeDetail.vue:147`，按钮在 `:89` |
| `PanelCertificate` 接口字段为大驼峰，与后端裸序列化一致 | `pkg/webadmin/ui/src/api/client.ts:283-292` |
| API 函数 | `client.ts:587-592` `listNodeCertificates` / `revokeNodeCertificates` |
| 前端构建命令（已核实）：`npm run build` = `vue-tsc --noEmit && vite build` | `pkg/webadmin/ui/package.json` |

### 节点侧

| 事实 | 位置 |
|---|---|
| 重连循环：60s 上限无限退避，失败只打一条 `slog.Warn("control-plane connection ended", "error", err, "retry_in", backoff)` | `pkg/nodeagent/client.go:111-130`，日志在 `:121` |
| panel 对「证书过期 / 被吊销 / 节点非 active / 指纹未知」**一律返回 401** `"client certificate required"` | `pkg/panel/transport.go:159-163`（判定在 `authenticateMTLS` `:185-200`） |
| 本地证书过期时 TLS 握手阶段即被 Go 拒绝（不会走到 401） | 见 design §3.3 的两条路径区分 |

## Requirements

### R1 admin API 暴露到期信息
- R1.1 `GET /api/admin/nodes/{id}/certs` 改为返回**显式 DTO**，不再裸序列化 model（取舍见 design §1.1）。
- R1.2 DTO 字段用 snake_case json tag，与 `adminapi.go:315` 的 `expires_at` 既有规范对齐。
- R1.3 新增派生字段：剩余天数、证书状态（四态之一）。**状态在后端判定**，遵循 `dashboard.go:29-30` 的既有约定：不让前端复制业务判断。
- R1.4 四态定义：`active`（有效且非临期）/ `expiring`（有效但剩余 < 阈值）/ `expired`（已过 NotAfter）/ `revoked`（已吊销，优先级最高）。
- R1.5 阈值用**比例** `(NotAfter - NotBefore) / 3`，与父任务 D3 一致，**不引入新的绝对值配置项**。
- R1.6 不新增 DB 列：剩余天数与状态都是算出来的（本任务**不改 panel DB schema**）。

### R2 UI 多态展示
- R2.1 证书状态列改为四态渲染，中文文案，与既有 UI 用语风格一致（参照 `PanelNodeDetail.vue` 现有「已撤销」「有效」）。
- R2.2 `expiring` 与 `expired` 要有视觉区分度（不能只靠文字），沿用项目既有的状态样式体系，不新造一套配色。
- R2.3 **修正 `activeCertificateCount` 语义**（`:147`）：过期证书不得计入「有效」。这会连带修正「撤销全部有效证书」按钮的 disabled 判断（`:89`）。
- R2.4 `PanelCertificate` TS 类型随 DTO 改为 snake_case，`client.ts:283-292` 与所有引用点同步。
- R2.5 剩余天数在表格中可见（新增一列或与 NotAfter 合并展示，取舍见 design §2.2）。

### R3 dashboard 聚合
- R3.1 dashboard summary 增加证书到期聚合。**必须复用**既有 `Attention` / `Severity` 机制与 `dashboardTelemetry` 的三态聚合先例，**不新造顶层卡片**（克制原则，见 design §1.3）。
- R3.2 新增 severity 档位表示「证书临期/已过期」，并接入 `severityRank`（`dashboard.go:90-95`）的排序。档位优先级取舍见 design §2.3。
- R3.3 证书已过期的节点必须进入 `AttentionNodes`（`dashboard.go:155-169` 的关注判定），使其在首屏可见。
- R3.4 聚合口径要能区分「证书临期」与「证书已过期」两种数量，不合并成一个数。

### R4 节点侧不再静默
- R4.1 节点侧要能区分并分别报错**两条独立路径**（design §3.3）：
  - 路径 A：**本地证书已过期/损坏** —— 在 TLS 握手阶段就失败，panel 侧根本没收到请求。
  - 路径 B：**panel 返回 401** —— 证书被吊销、节点被 disable/retire、或指纹不在表内。
- R4.2 两条路径都必须是 **Error 级**、含「证书」语义、**指明恢复动作**，不得混在 `client.go:121` 的通用 `control-plane connection ended` warn 里（父任务 AC2）。
- R4.3 这类永久错误不得被无声退避掩盖：退避可以继续（保持重连能力），但日志必须每次都说清是证书问题，或以明显降频但不静默的方式持续提示。
- R4.4 **安全网 A 不得破**：日志级别提升 ≠ 让 node 退出。数据面必须继续服务本地 DB。
- R4.5 若 `08-16-cert-auto-renew` 已落地 `LoadCertificate` / `RenewalThreshold` / `NeedsRenewal`（`pkg/nodeagent/register.go`），**必须复用**，不得重写一份到期判定。

### R5 Prometheus 指标 —— 本轮不做
- R5.1 **明确不加**证书 gauge。理由见 design §1.4。该判断要写进 design，避免后续反复讨论。

## Acceptance Criteria

- [ ] AC1 `GET /certs` 返回 snake_case 字段，含剩余天数与四态状态字段。
- [ ] AC2 状态判定单测覆盖四态边界：已吊销且已过期 → `revoked`（吊销优先）；恰好 `NotAfter` 时刻 → `expired`；剩余恰好等于阈值 → 明确归属（`expiring` 或 `active`，取一种并测它）；剩余远大于阈值 → `active`。
- [ ] AC3 剩余天数对已过期证书的取值有明确定义并被测试（负数或 0，取一种，不得是未定义行为）。
- [ ] AC4 阈值随证书自身 TTL 变化：90 天证书阈值 30 天；30 天证书阈值 10 天（比例语义，AC 可机械验证）。
- [ ] AC5 UI 四态渲染正确：构造已过期未吊销的证书，界面显示「已过期」而**非**「有效」。
- [ ] AC6 `activeCertificateCount` 不再把过期证书计入有效；「撤销全部有效证书」按钮的 disabled 判断随之正确。
- [ ] AC7 `expiring` 与 `expired` 有视觉区分，且未引入项目外的新配色体系。
- [ ] AC8 `npm run build`（= `vue-tsc --noEmit && vite build`）通过，无类型错误（DTO 改名后所有引用点已同步）。
- [ ] AC9 dashboard summary 返回证书临期数与已过期数，两者可区分。
- [ ] AC10 证书已过期的节点出现在 `AttentionNodes` 中，且 `Severity` 接入了 `severityRank` 排序。
- [ ] AC11 dashboard **未新增顶层卡片**，是在既有结构内扩展（人工核对 `PanelDashboard.vue` diff）。
- [ ] AC12 节点侧路径 A（本地证书过期）：日志为 Error 级、含证书语义、指明恢复动作。
- [ ] AC13 节点侧路径 B（panel 401）：日志为 Error 级、含证书语义、指明恢复动作，且与路径 A 文案可区分。
- [ ] AC14 两条路径下 node 的 S3 数据面持续正常服务本地 DB（安全网 A，父任务 AC3）。
- [ ] AC15 未重写到期判定：若兄弟子任务已提供 `LoadCertificate`/`RenewalThreshold`/`NeedsRenewal`，本任务复用之（grep 自证无平行实现）。
- [ ] AC16 **未改 panel DB schema**（无新增列/表，grep `migrationModels` 与 `models.go` diff 自证）。
- [ ] AC17 未改节点 DB schema（安全网 C）。
- [ ] AC18 `pkg/panel/transport.go` 的 `ClientAuth` 未变，未引入放宽标准校验的 `VerifyPeerCertificate`（父任务 D2 红线，grep 自证）。
- [ ] AC19 新增日志不含私钥、CSR 或 secret 材料（父任务 AC8）。
- [ ] AC20 `go build ./... && go vet ./... && go test ./... && gofmt -l .` 全绿。

## 约束

- 不新增第三方依赖（前后端均不加）。
- **不改 panel DB schema**，不改节点 DB schema（安全网 C）。
- 不引入新的绝对值阈值配置项（R1.5，阈值是比例）。
- 不得改动客户端证书的签发/校验语义（那是 `08-16-cert-auto-renew` 的地盘）；不得改 `ClientAuth`（父任务 D2 红线）。
- 安全网 A：日志与展示改动不得让 node 退出或让数据面降级。
- 遵循 `dashboard.go:29-30` 的既有约定：业务判断在后端，前端只做展示。
- 不新造 UI 配色/状态样式体系，沿用既有。

## Notes

- `GET /certs` 的字段改名是**破坏性变更**，但该接口只有 panel 自家 UI 消费（`client.ts:587-592` 是唯一调用点），无外部契约，因此可以直接改而非加兼容层。这一点在 design §4 有版本错配矩阵。
- 与 `08-16-cert-auto-renew` 并行时，两者都会碰 `pkg/nodeagent/client.go` 的重连循环区域（它加续期触发、本任务加错误分类）。**建议实现顺序：先让 cert-auto-renew 落地**，本任务再在其基础上加错误分类，可避免同一函数的冲突。若并行，需在合并时手工核对该函数。
- 代码落盘由独立会话执行；本会话负责规划与验收。
