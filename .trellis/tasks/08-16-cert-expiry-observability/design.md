# 设计：证书到期可观测性

## 1. 边界与取舍

### 1.1 DTO vs 加 json tag —— 选**DTO**

现状：`adminapi.go:361` 把 `[]NodeCert` 直接 `writeTransportJSON` 出去，`NodeCert`（`models.go:44-55`）无任何 json tag，前端因此拿到 `NotAfter`/`Revoked` 这种 Go 大驼峰。

**备选 A：给 `NodeCert` 加 json tag**
- 优点：改动最小。
- **否决理由**：`NodeCert` 是 GORM 持久化模型。给它加 json tag 等于把 DB 模型钉成 API 契约——以后加一个内部列（比如兄弟子任务要加的 `activated_at`）就会自动泄漏到 API 上。这个耦合正是要避免的。`adminapi.go:315` 附近的注册令牌已经示范了正确做法：单独的响应结构 + `json:"expires_at"`。

**备选 B（采用）：显式 DTO**
```go
// certResponse 是证书列表的 API 形状。派生字段(状态/剩余天数)在后端算,
// 遵循 dashboard.go 的既有约定:不让前端复制业务判断。
type certResponse struct {
    ID             uint       `json:"id"`
    Fingerprint    string     `json:"fingerprint"`
    Serial         string     `json:"serial"`
    NotBefore      time.Time  `json:"not_before"`
    NotAfter       time.Time  `json:"not_after"`
    Revoked        bool       `json:"revoked"`
    RevokedAt      *time.Time `json:"revoked_at,omitempty"`
    CreatedAt      time.Time  `json:"created_at"`
    Status         string     `json:"status"`            // active|expiring|expired|revoked
    DaysUntilExpiry int       `json:"days_until_expiry"` // 已过期为负
}
```
- `NodeID` 不必回传（路径里已有 `{id}`）。
- 破坏性影响：前端 `PanelCertificate`（`client.ts:283-292`）字段全改。**可接受**，因为唯一消费方是自家 UI（`client.ts:587-592` 是唯一调用点），无外部契约。

### 1.2 状态判定放后端

遵循 `dashboard.go:29-30` 已写明的约定：「额外的 severity 是后端派生的展示排序键，不让前端复制业务判断」。到期状态同理——阈值是 `(NotAfter-NotBefore)/3` 这种业务规则，放前端算会在 UI 与 dashboard 两处各出现一份，必然漂移。

### 1.3 dashboard 不新造顶层卡片

`dashboard.go` 已有两套成熟机制可复用：
- `Totals.Attention` + `AttentionNodes[]` + 后端派生 `Severity`（`:18,31-45,82-95,155-169`）——用于「哪些节点需要处理」。
- `dashboardTelemetry` 的 `ValidNodes/MissingNodes/StaleNodes`（`:62-68`）——三态计数的现成范式。

证书到期天然属于「需要处理」，所以：**在 `dashboardHealth` 或新增一个小结构里加两个计数（临期/已过期），并让已过期节点进 `AttentionNodes`**，而不是在首屏加一张新卡片。理由：首屏卡片是稀缺资源，证书到期是低频事件（90 天一次），常态下计数为 0，占一张卡片是纯噪音；但它一旦发生就必须显眼，进 `AttentionNodes` 正好满足「平时不占地方、出事立刻在列表顶部」。

### 1.4 本轮**不加** Prometheus 证书 gauge —— 明确判断

`pkg/webadmin/ops.go:17` 的 `/metrics` 目前只有健康/就绪，**没有任何业务 gauge**。加证书 gauge 意味着要先建立「panel 业务指标」这一整类东西：指标命名规范、label 基数控制（按 node_id 打 label 会随节点数增长）、注册表管理、以及配套的抓取/告警配置。

**判断：不做。** 理由：
1. 本任务已通过 admin API + UI + dashboard + 节点日志四处覆盖了可见性，指标是第五处，边际收益低。
2. 建立业务指标体系是独立的量级，塞进本任务会让范围失控，且做得草率反而留下坏范式（比如高基数 label）。
3. 真正需要机器告警的用户，`GET /certs` 的 `days_until_expiry` 已经足够被外部脚本轮询。

若日后要做，应作为独立任务先定 panel 指标规范，再一次性补齐节点/证书/任务多类指标。

## 2. 契约

### 2.1 `GET /api/admin/nodes/{id}/certs`

响应：`certResponse[]`（形状见 §1.1），排序沿用现状 `Order("id ASC")`（`adminapi.go:357`）。

状态判定优先级（**顺序不可换**）：
```
1. Revoked            → "revoked"      （吊销优先于一切，即使同时已过期）
2. now >= NotAfter    → "expired"
3. remaining < (NotAfter-NotBefore)/3 → "expiring"
4. 否则               → "active"
```
边界裁定（必须被测，AC2）：
- `now == NotAfter` → `expired`（与 `pki.go:162` 的 `!now.Before(NotAfter)` 语义一致，保持全仓一致）。
- `remaining == 阈值` → `active`（用严格小于 `<`，与 R1.4 的「剩余 < 阈值」字面一致）。
- `now < NotBefore`（尚未生效）→ 归入 `active`。这种证书在本系统里不会出现（签发时 `NotBefore` 已回拨一分钟，`pki.go:103`），不值得为它加第五态。

`DaysUntilExpiry`：`NotAfter - now` 向下取整到天，**已过期为负数**（AC3 取此定义）。不要 clamp 到 0——运维需要知道「过期了多久」。

### 2.2 UI 表格形状

现有 4 列（`PanelNodeDetail.vue:96-102`）：Serial / 指纹 / NotAfter / 状态。

**采用：不加新列，把剩余天数并入「到期时间」列**，形如 `2026-11-14 03:04（剩 29 天）` / `2026-08-01 03:04（已过期 15 天）`。理由：该表已有一列很宽的指纹（`fingerprint-code` 样式，`:100`），再加列会挤；且剩余天数与到期时间是同一件事的两种读法，并排放反而更好读。

状态列四态中文文案：`有效` / `即将到期` / `已过期` / `已撤销`。视觉区分沿用项目既有状态样式，不新造配色（R2.2）。

`activeCertificateCount`（`:147`）改为按后端 `status` 判定，只有 `active` 与 `expiring` 计入「有效」——过期与吊销都不算。这样 `:89` 的「撤销全部有效证书」按钮语义才正确。

### 2.3 dashboard 聚合与 severity

计数：在 dashboard summary 增加证书维度的两个数（临期/已过期），复用 `dashboardTelemetry` 的三态计数范式（`:62-68`）。

新 severity 档位接入 `severityRank`（`:90-95`）。现有排序：`sync_failed=4 > drift=3 > offline=2 > pending=1`。

**裁定：`cert_expired` 排在最高（5），`cert_expiring` 排在 `offline` 与 `pending` 之间。**

理由：证书已过期 = 控制面**永久**失联且不会自愈（父任务 P0 故障形态），比 `sync_failed`（通常可重试）更严重，所以给 5。而 `cert_expiring` 是「还没坏但要动手」，比 `offline`（已经坏了）轻，比 `pending`（正常流转中）重。

注意 `severityRank` 是纯排序键，加档位需同时更新 `attentionSeverity`（`dashboard.go` 内）的 switch 分支顺序，否则新档位永远不会被选中。

## 3. 数据流

### 3.1 panel 侧

```
GET /certs → 查 node_certs(node_id) → 逐行算 status + days_until_expiry → certResponse[]
```
纯计算，无新增查询、无 DB 写入、无 schema 变更（R1.6 / AC16）。

### 3.2 dashboard

```
DashboardSummary → 现有节点循环
  └─ 额外查该节点的未吊销证书(取最晚 NotAfter 的那张作为"当前证书")
     └─ 算 status → 累加临期/已过期计数
        └─ status == expired → 进 AttentionNodes，Severity = cert_expired
```
**N+1 风险**：现有循环是按节点遍历（`dashboard.go:155` 附近）。逐节点查证书会产生 N+1 查询，而 `dashboard.go:96-97` 的注释明确说该接口的设计目标就是「一次响应覆盖前端首屏，避免对每个节点发起 N+1 请求」。
→ **必须一次批量查全部节点的证书**（一条 `WHERE node_id IN (...)` 或全表扫后在内存按 node_id 分组），不得在循环里逐个查。这是本任务最容易违反既有设计意图的地方。

「当前证书」口径：同一节点可能有多张未吊销证书（兄弟子任务 D1 的签发-激活窗口内就会出现两张）。取 **`NotAfter` 最大的那张**作为该节点的当前证书——它代表节点最终能用到什么时候。

### 3.3 节点侧两条错误路径（R4.1 的关键区分）

这两条路径**失败位置完全不同**，必须分别识别：

| | 路径 A：本地证书过期/损坏 | 路径 B：panel 返回 401 |
|---|---|---|
| 触发 | 证书过 `NotAfter`，或文件损坏 | 证书被吊销 / 节点 disabled/retired / 指纹不在表内 |
| 失败位置 | **TLS 握手阶段**，Go 客户端侧就拒了；请求根本没到 panel | HTTP 层，panel 的 `transport.go:159-163` 返回 401 |
| 如何识别 | 拨号前主动 `LoadCertificate` 检查 `NotAfter`（复用兄弟子任务的函数，R4.5）；或从 dial 错误中识别证书类错误 | 检查 WebSocket 升级失败的 HTTP 状态码 == 401 |
| 恢复动作 | 管理员签发一次性注册令牌 → 填入 node.yaml → 重启（父任务 D2：**无宽限期**） | 需管理员在管理面检查节点状态；若被吊销则同上走重注册 |

**推荐实现**：在 `dial`（`client.go:138-149` 区域）之前先做一次本地证书检查，这样路径 A 能给出确定性的、不依赖错误字符串匹配的判断。路径 B 靠 401 状态码识别——`coder/websocket` 的 `Dial` 失败时能拿到响应，实现时确认其错误类型能否取到状态码；若取不到，退化为检查错误文本包含 401，并在代码注释里说明这个妥协。

**R4.3 的降频**：这类错误每次重连都会复现，60s 一次刷 Error 会淹没日志。裁定：**首次立即 Error，之后每 N 次重连（建议 N 使得约 10 分钟一条）重复一次 Error**，不得完全静默。计数器在连接成功时重置。

## 4. 兼容性

| 组合 | 行为 |
|---|---|
| 新 panel + 新 UI | 正常。UI 内嵌在 panel 镜像里（`adminapi` 内嵌 `webadmin/ui`），二者**总是同版本发布**，不存在错配 |
| 新 panel + 旧缓存 UI | 浏览器可能缓存旧 JS。旧 JS 读 `NotAfter`（大驼峰）会得到 `undefined` → 证书表格显示空。属于强刷即解的临时现象，**不为此加兼容层**（理由：内嵌同版本发布 + 无外部消费方） |
| 外部脚本消费 `/certs` | 字段改名会破坏。经核查无此类消费方（唯一调用点 `client.ts:587-592`）。变更需在子任务 4 的文档里记一笔 |
| 新 node + 旧 panel | 节点侧错误分类是纯本地逻辑，不依赖 panel 版本，行为正常 |
| 旧 node + 新 panel | 旧 node 仍打通用 warn（静默问题依旧），但 panel 侧 API/UI/dashboard 的可见性**照常生效** —— 这正是 panel 侧可观测性的价值：不依赖节点升级也能看到到期风险 |

## 5. 安全考量

- **不泄露敏感材料**：DTO 只含指纹、序列号、时间与派生状态。指纹与序列号本身不是秘密（证书是公开材料）。不得回传证书 PEM（现状也没有，保持）。
- **日志**：新增的 Error 日志只说「证书过期/被拒 + 恢复动作」，不打印证书内容、不打印私钥路径以外的文件内容（父任务 AC8 / AC19）。
- **不削弱认证**：本任务纯读+展示，不碰 `transport.go` 的 `ClientAuth`、不碰 `IsCertValid`（父任务 D2 红线，AC18 grep 自证）。
- **安全网 A**：日志级别提升不改变控制流，node 继续服务本地 DB（AC14）。
- **信息披露面**：`/certs` 与 dashboard 都在已鉴权的 admin 路由后（复用 `webadmin.Auth`），未扩大暴露面。

## 6. 回滚形状

| 层面 | 回滚方式 |
|---|---|
| API DTO | 恢复裸序列化 `[]NodeCert`，同时回滚前端 `PanelCertificate` 类型。**前后端必须一起回**（同版本发布，不存在半回滚场景） |
| UI 四态 | 回到 `Revoked ? '已撤销' : '有效'`。注意 `activeCertificateCount` 的修正是**独立的 bug 修复**，即使回滚四态展示也应保留它 |
| dashboard 聚合 | 移除新增计数与 severity 档位。**注意**：从 `severityRank` 移除档位时，`attentionSeverity` 的 switch 分支也要同步移除，否则会返回一个 rank 表里没有的字符串 |
| 节点侧错误分类 | 回到单条通用 warn。这是**可观测性能力回退**，仅在分类逻辑本身出错时才回退 |

## 7. 未决与移交

- **Prometheus 业务指标体系**：本轮明确不做（§1.4）。若要做，先立独立任务定 panel 指标规范（命名、label 基数、注册表），再一次性补齐节点/证书/任务多类指标。
- **移交子任务 4（`08-16-cert-docs-correction`）**：
  - `/certs` 响应字段改名需在文档中体现（若文档有列此 API 的字段，参照 `README.md:484-485` 附近）。
  - 到期巡检指引（父 PRD 缺陷清单 F6、子任务 4 的 R2.5）需描述本任务最终的 UI 四态与 dashboard 计数位置。
- **与 `08-16-cert-auto-renew` 的交界**：两者都会改 `pkg/nodeagent/client.go` 的重连/连接建立区域。**建议 cert-auto-renew 先落地**，本任务在其 `LoadCertificate`/`RenewalThreshold`/`NeedsRenewal` 基础上加错误分类（R4.5 / AC15）。若并行，合并时需手工核对该函数。
