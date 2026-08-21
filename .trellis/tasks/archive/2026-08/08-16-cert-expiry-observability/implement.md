# 执行计划：证书到期可观测性

> 先读 `prd.md` → `design.md`。父任务承重决策见 `.trellis/tasks/08-16-node-cert-lifecycle/prd.md`。
> **建议在 `08-16-cert-auto-renew` 落地后开工**（design §7：两者都改 `pkg/nodeagent/client.go` 同一区域，且 Step 5 要复用其函数）。若必须并行，Step 5 合并时手工核对该函数。

## 环境已核实

- 前端构建：`cd pkg/webadmin/ui && npm run build` = `vue-tsc --noEmit && vite build`（`package.json` 已确认，无独立 lint/test script）。
- 类型检查可单独跑：`cd pkg/webadmin/ui && npx vue-tsc --noEmit`。
- `/certs` 唯一消费方是 `pkg/webadmin/ui/src/api/client.ts:587-592`，无外部契约（design §4）。

## Step 1 — 到期状态判定（后端单一实现）

- [ ] 1.1 在 `pkg/panel/` 内新增到期状态判定函数：入参 `(notBefore, notAfter time.Time, revoked bool, now time.Time)`，返回 `(status string, daysUntilExpiry int)`。**必须显式传 `now`**（可测性），不得内部取当前时间。
- [ ] 1.2 状态常量四态：`active` / `expiring` / `expired` / `revoked`，带中文注释。
- [ ] 1.3 判定顺序严格按 design §2.1（**顺序不可换**）：`revoked` → `expired` → `expiring` → `active`。
- [ ] 1.4 阈值 `(notAfter - notBefore) / 3`，用严格小于 `<` 比较（design §2.1 的边界裁定）。
- [ ] 1.5 `daysUntilExpiry` = `notAfter - now` 向下取整到天，**已过期为负**，不 clamp。
- [ ] 1.6 单测覆盖 design §2.1 的全部边界（AC2/AC3/AC4）：已吊销且已过期 → `revoked`；`now == notAfter` → `expired`；剩余恰好等于阈值 → `active`；`now < notBefore` → `active`；90 天证书阈值 30 天、30 天证书阈值 10 天。

验证：`go test ./pkg/panel/ -run 'CertStatus|Expiry' -v`

## Step 2 — admin API DTO

- [ ] 2.1 `pkg/panel/adminapi.go`：定义 `certResponse`（形状见 design §1.1），snake_case json tag，不含 `NodeID`。**不要给 `NodeCert` 加 json tag**（design §1.1 的否决理由：会把 DB 模型钉成 API 契约）。
- [ ] 2.2 改 `certsRoute` 的 GET 分支（`adminapi.go:347-362`）：查询后逐行映射为 `certResponse`，调 Step 1 的函数填 `Status` / `DaysUntilExpiry`。排序沿用 `Order("id ASC")`（`:357`）。
- [ ] 2.3 `revoke` 分支（`:363-375`）不动。
- [ ] 2.4 测试：构造四态证书，断言响应 JSON 字段名为 snake_case、`status` 与 `days_until_expiry` 正确。

验证：`go test ./pkg/panel/ -run 'Certs|CertsRoute' -v`

### Gate A — 无 schema 变更 + D2 红线
```bash
git diff --stat pkg/panel/models.go pkg/panel/migrate.go   # 应为空(AC16)
grep -n 'ClientAuth' pkg/panel/transport.go                # 仍是 tls.VerifyClientCertIfGiven
grep -rn 'VerifyPeerCertificate\|VerifyConnection' pkg/panel/ pkg/nodeagent/ --include=*.go
```
`models.go`/`migrate.go` 若有 diff → 违反 AC16，停下。第三条在非测试文件新增命中 → 违反父任务 D2（AC18）。

## Step 3 — 前端类型与四态展示

- [ ] 3.1 `pkg/webadmin/ui/src/api/client.ts:283-292`：`PanelCertificate` 改为 snake_case，增 `status` 与 `days_until_expiry`。`status` 用字面量联合类型 `'active' | 'expiring' | 'expired' | 'revoked'`（对齐仓库既有做法，参照 `PanelTaskState`）。
- [ ] 3.2 `PanelNodeDetail.vue:102`：状态列改为按 `status` 渲染四态中文 `有效` / `即将到期` / `已过期` / `已撤销`。
- [ ] 3.3 `PanelNodeDetail.vue:101`：到期时间列并入剩余天数（design §2.2：**不加新列**），形如 `2026-11-14 03:04（剩 29 天）` / `（已过期 15 天）`。
- [ ] 3.4 `expiring` / `expired` 的视觉区分沿用项目既有状态样式，**不新造配色**（AC7）。先在仓库里找现成的状态样式类再用。
- [ ] 3.5 **修正 `activeCertificateCount`（`:147`）**：改为只把 `status` 为 `active` 或 `expiring` 的计入有效。这连带修正 `:89` 撤销按钮的 disabled 判断（AC6）。
- [ ] 3.6 grep 全仓确认 `PanelCertificate` 的所有引用点已同步改名。

验证：`cd pkg/webadmin/ui && npx vue-tsc --noEmit && npm run build`

### Gate B — 前端类型全绿
```bash
cd pkg/webadmin/ui && npm run build
```
必须无类型错误（AC8）。DTO 改名后漏改引用点会在这里暴露。

## Step 4 — dashboard 聚合

- [ ] 4.1 `pkg/panel/dashboard.go`：增加证书维度的两个计数（临期数 / 已过期数），复用 `dashboardTelemetry`（`:62-68`）的三态计数范式。两个数**必须可区分**，不得合并（AC9）。
- [ ] 4.2 **批量查证书，严禁在节点循环里逐个查**（design §3.2）：一条 `WHERE node_id IN (...)`（或全表后内存分组），在进入 `:155` 附近的节点循环之前完成。`dashboard.go:96-97` 的注释明确说该接口设计目标就是避免 N+1，这是本任务最容易违反既有设计意图的地方。
- [ ] 4.3 「当前证书」口径：同节点多张未吊销证书时取 `NotAfter` 最大的那张（design §3.2；兄弟子任务 D1 的签发-激活窗口内必然出现两张）。
- [ ] 4.4 新增 severity 档位 `cert_expired` / `cert_expiring`，接入 `severityRank`（`:90-95`）：`cert_expired = 5`（最高，理由见 design §2.3），`cert_expiring` 排在 `offline`(2) 与 `pending`(1) 之间。**注意需重排既有数值或用非整数间隔**——实现时确认插入方式不破坏既有相对顺序。
- [ ] 4.5 同步更新 `attentionSeverity` 的 switch 分支，把证书档位加在**正确的判定顺序位置**，否则新档位永不被选中（design §2.3 明确警告）。
- [ ] 4.6 证书已过期的节点进入 `AttentionNodes`（扩展 `:155-160` 的关注判定条件），并计入 `Totals.Attention`（AC10）。
- [ ] 4.7 **不新增顶层卡片**：`PanelDashboard.vue` 只在既有结构内展示新计数（AC11）。
- [ ] 4.8 测试：多节点多证书场景，断言临期/已过期计数正确、过期节点进 AttentionNodes、severity 排序正确、且**查询次数不随节点数增长**（可用 GORM 的查询计数或 session 钩子断言）。

验证：`go test ./pkg/panel/ -run 'Dashboard' -v` && `cd pkg/webadmin/ui && npm run build`

### Gate C — N+1 与卡片克制
```bash
go test ./pkg/panel/ -run 'Dashboard' -v
git diff pkg/webadmin/ui/src/views/PanelDashboard.vue    # 人工核对:未新增顶层卡片(AC11)
```

## Step 5 — 节点侧错误分类

> 若 `08-16-cert-auto-renew` 已落地 `LoadCertificate` / `RenewalThreshold` / `NeedsRenewal`（`pkg/nodeagent/register.go`），**必须复用，不得重写到期判定**（AC15）。

- [ ] 5.1 **路径 A（本地证书过期/损坏）**：在 `client.go:138-149` 的 dial 之前先做一次本地证书检查（design §3.3 推荐——这样判断是确定性的，不依赖错误字符串匹配）。命中则打 **Error**：含「证书」语义 + 明确恢复动作（管理员签发一次性注册令牌 → 填入 node.yaml → 重启；父任务 D2 无宽限期）。
- [ ] 5.2 **路径 B（panel 401）**：识别 WebSocket 升级失败的 HTTP 401。先确认 `coder/websocket` 的 `Dial` 错误能否取到状态码；**若取不到，退化为检查错误文本含 401，并在代码注释里写明这个妥协**。打 Error，文案与路径 A **可区分**（AC13）。
- [ ] 5.3 **降频但不静默**（R4.3）：首次立即 Error，之后每 N 次重连重复一次（N 取值使约 10 分钟一条，配合 60s 上限退避）。计数器在连接成功时重置。不得完全静默。
- [ ] 5.4 **不破安全网 A**：以上均为日志与分类，**不得**改变控制流、不得让 node 退出、不得让数据面降级（AC14）。
- [ ] 5.5 测试：路径 A 与路径 B 各自产生 Error 级、含证书语义、文案可区分的日志；两种情况下 S3 数据面仍可服务本地 DB。
- [ ] 5.6 grep 自证无平行实现的到期判定（AC15）。

验证：`go test ./pkg/nodeagent/ -v`

### Gate D — 全量收口
```bash
go build ./... && go vet ./... && go test ./... && gofmt -l .
cd pkg/webadmin/ui && npm run build && cd -
git diff --stat pkg/panel/models.go pkg/panel/migrate.go pkg/db/    # 应为空(AC16/AC17)
grep -n 'ClientAuth' pkg/panel/transport.go
```
`gofmt -l .` 必须无输出。逐条对 `prd.md` 的 AC1–AC20 打勾。

## 回滚点

| 回滚到 | 动作 |
|---|---|
| Step 5 之前 | 节点侧回到单条通用 warn（**可观测性能力回退**，仅在分类逻辑本身出错时才回退） |
| Step 4 之前 | 移除 dashboard 计数与 severity 档位。**从 `severityRank` 移除档位时必须同步移除 `attentionSeverity` 的对应分支**，否则会返回 rank 表里不存在的字符串 |
| Step 3 之前 | 前端回到两态。**但 `activeCertificateCount` 的修正应保留**——它是独立的 bug 修复，与四态展示无关 |
| Step 2 之前 | API 回到裸序列化。**前后端必须一起回**（UI 内嵌同版本发布，无半回滚场景） |

## 已知坑

- **N+1**：`dashboard.go:96-97` 的注释就是在说这个接口存在的理由。在节点循环里查证书会当场违背它。Step 4.2 必须批量。
- **`severityRank` 插入**：现有 `sync_failed=4 > drift=3 > offline=2 > pending=1` 是紧密整数。插入 `cert_expiring` 到 2 和 1 之间需要重排或改用间隔更大的数值。别只加 map 项就以为完事。
- **`attentionSeverity` 的 switch 顺序**：只加 `severityRank` 项而不改 switch，新档位永远选不中——这类 bug 测试不写就发现不了。
- **前端字段改名漏点**：`vue-tsc --noEmit` 是唯一防线，Gate B 不能跳。
- **`coder/websocket` 的 401 获取方式**未经验证（Step 5.2），实现时先确认再落笔，取不到就按妥协方案走并注释说明。
- **同一节点两张有效证书是正常状态**（兄弟子任务 D1 的签发-激活窗口），聚合口径不能假设「一节点一证书」。
- `Edit` 工具在本仓库历史会话出现过「报成功但未落盘」，大文件改完务必读回确认。
