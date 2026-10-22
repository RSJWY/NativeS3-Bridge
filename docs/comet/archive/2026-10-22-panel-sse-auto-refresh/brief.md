# 目标

管理面板（WebAdmin UI）的关键数据实现实时自动更新：在现有 admin 监听器（admin_addr，默认 9001）上新增 SSE（Server-Sent Events）事件端点，后端复用 Hub 连接注册与任务编排器的状态流转作为事件源，前端用原生 EventSource 订阅，节点上线/下线、任务进度等状态变化在页面上自动呈现，无需手动刷新浏览器。

# 范围

- 后端：panel 包内新增轻量事件总线；在节点 WebSocket 注册/断开（transport 的 Hub.Register/Unregister 调用点）与任务状态流转（tasks.go）处发布事件；admin 服务器新增 `GET /api/admin/events` SSE 端点，复用现有 webadmin 会话 Cookie 鉴权中间件。
- 事件类型：节点上线（node_online）、节点下线（node_offline）、任务状态变更（task_updated，覆盖 pending/running/success/failed/unknown 全部流转）。
- 前端：pkg/webadmin/ui 新增 EventSource 订阅 composable；PanelNodes、PanelDashboard 页面接入事件驱动的自动刷新；PanelNodeDetail 接入节点在线状态与任务进度事件（现有 800ms 任务轮询替换为事件驱动）。
- 心跳与降级：服务端定期发送 SSE 心跳；客户端超过阈值未收到任何事件/心跳时自动降级为慢速定时轮询，SSE 恢复后停止轮询。
- 事件采用「通知式」：SSE 只推送事件类型与标识（如 node_online + nodeID、task_updated + nodeID/taskID/状态），前端收到后重新拉取对应 REST 端点；REST 仍是唯一数据源，不在 SSE 中推送完整数据体。
- 部署文档：docs/public-deployment.md 补充反向代理下 SSE 长连接的配置要求（禁用缓冲等）。

## 目录结构

### 新建

- pkg/panel/events.go — 面板内轻量事件总线（发布/订阅）与 SSE 流式 handler（text/event-stream、逐事件 Flush、心跳保活）。
- pkg/panel/events_test.go — 事件总线与 SSE 端点测试（含鉴权、断连清理、心跳）。
- pkg/webadmin/ui/src/composables/useAdminEvents.ts — 前端 EventSource 订阅 composable（自动重连、心跳超时检测、降级轮询开关、登录态失效处理）。

### 修改

- pkg/panel/adminserver.go — 挂载 `/api/admin/events` 路由到现有鉴权 wrap；更新「管理面没有 SSE/流式端点」的注释与相关超时说明（adminserver.go:83-88）。
- pkg/panel/transport.go — 在 Hub.Register（:359）与 disconnect（:406）处发布节点上线/下线事件。
- pkg/panel/tasks.go — 在 Dispatch/expireTask/FailInFlightForConn/markState 等状态流转处发布任务状态事件。
- pkg/panel/transport.go 的 handleTaskResult（:643）— 节点上报任务结果时发布事件。
- cmd/panel/main.go — 创建事件总线并接线到 transport 与 tasks。
- pkg/webadmin/ui/src/views/PanelNodes.vue — 订阅节点事件，自动重载列表；SSE 降级时按轮询兜底。
- pkg/webadmin/ui/src/views/PanelDashboard.vue — 订阅节点事件，自动重载总览；SSE 降级时按轮询兜底。
- pkg/webadmin/ui/src/views/PanelNodeDetail.vue — 订阅在线状态与任务事件。
- pkg/webadmin/ui/src/components/panel/PanelNodeLogsSection.vue — 任务进度由 800ms setTimeout 轮询（:150-174）改为事件驱动（保留一次性兜底）。
- pkg/webadmin/ui/src/api/client.ts — 事件负载类型定义（如需）。
- docs/public-deployment.md — 补充反向代理 SSE 配置（proxy_buffering off / X-Accel-Buffering）。

### 删除

无

### 明确不建

- 浏览器 WebSocket 通道 — SSE 满足服务端单向推送且 EventSource 自带重连，不引入双向通道。
- 对 9443 agent mTLS 监听器的任何改动 — 只复用其事件源（Hub/任务流转），不动节点控制面通道与协议。
- 跨面板实例的事件广播（Redis pub/sub 等） — 面板按单实例部署设计（docker-compose.panel.yml 单服务，Hub 为进程内状态），SSE 订阅者与事件源同进程。
- 配置类数据（buckets/credentials/webhooks/rate-limit/desired-state）的自动刷新 — 用户已确认（Q3）：这些数据变更主要由面板自身操作触发、操作后已有局部刷新，第一版不纳入事件范围。
- 仪表盘历史指标、审计日志的事件化推送 — 第一版不做（Q1 确认范围为节点+任务）。
- 前端通用轮询框架 — 降级轮询仅为 SSE 不可用时的最小兜底，不抽象通用框架。

# 非目标

- 不改变节点控制面（9443）的协议、鉴权与拓扑。
- 不在本 change 内实现浏览器推送完整数据体（SSE 保持通知式，避免与 REST 形成双份数据通路）。
- 不做仪表盘历史指标与审计日志的事件化。
- 不做配置类资源（buckets/credentials/webhooks/rate-limit/desired-state）的实时推送。

# 约束与不变式

- SSE 端点必须挂在现有 admin 监听器与 webadmin 会话 Cookie 鉴权中间件之后，未登录请求返回 401 且不建立事件流。
- admin HTTP server 现有配置：无 WriteTimeout、ReadTimeout 30s、IdleTimeout 120s（adminserver.go:83-88 原注释声明「管理面没有 SSE/流式端点」）；引入 SSE 时必须核对超时语义并更新注释，不得破坏现有端点行为。
- SSE 响应需禁用缓存（Cache-Control: no-store）并设置 X-Accel-Buffering: no 以兼容 nginx 类反向代理；逐事件 Flush。
- 服务端心跳间隔与前端降级阈值需匹配（心跳周期显著小于降级阈值，避免误降级）。
- 事件总线为进程内分发，订阅者（SSE 连接）慢速消费不得阻塞节点控制面路径（transport/tasks 的发布点）；慢订阅者采用丢弃/断开策略而非背压等待。
- 前端 apiFetch 为相对路径 + credentials: 'include'（client.ts:363-387），EventSource 同源使用，Cookie 自动携带；SSE 收到 401 时的登录态失效处理需与现有 markLoggedOut 跳转逻辑一致。
- 单实例面板假设：不做跨实例事件分发。

# 关键决定

- 传输选型 SSE 而非 WebSocket：用户已确认（管理面板只需服务端→浏览器单向推送，EventSource 自带断线重连）。
- 事件范围（Q1 已确认）：节点上线/下线 + 任务状态变更，不含仪表盘指标与审计日志。
- 降级策略（Q2 已确认）：服务端 SSE 心跳 + 客户端心跳超时检测，超时后自动降级慢速轮询（约 30s 量级），SSE 恢复后停止轮询；EventSource 自带重连仍保留。
- 详情页范围（Q3 已确认）：PanelNodeDetail 仅实时化节点在线状态与任务进度，配置类 section 保持手动刷新。
- 通知式事件（只推类型+标识，前端重拉 REST）：REST 保持唯一数据源，避免在 SSE 中重复实现数据序列化，事件负载稳定。
- 事件源挂点：节点事件在 transport 的 Hub.Register/disconnect 处直接发布（比改造 TransportDeps 的单一 OnConnected/OnDisconnected 回调字段为多订阅者更直接，且 OnDisconnected 已被任务失败标记占用）。

# 验证期望

- `go vet ./...` 与 `go test ./...`（至少覆盖 pkg/panel、pkg/webadmin）通过。
- pkg/webadmin/ui 下 `npm run build`（vue-tsc --noEmit && vite build）通过。
- SSE 行为测试参照 adminserver_test.go 的登录 Cookie 范式（pkg/panel/adminserver_test.go:17-108）与 transport_test.go 的 waitFor 轮询断言范式（pkg/panel/transport_test.go:158）。

# 验收示例

- Scenario: 管理员登录面板后停留在 PanelNodes 页面，一个节点进程随后启动并注册成功，页面在线状态在数秒内自动变为在线，全程无手动刷新。
- Scenario: 管理员停留在 PanelDashboard 总览页，节点下线后总览的节点统计自动更新。
- Scenario: 管理员在 PanelNodeDetail 触发导入任务并停留在任务进度区，任务状态（pending/running/success/failed/unknown）变化自动呈现，不再依赖固定 800ms 轮询。
- Scenario: 未携带会话 Cookie 请求 `GET /api/admin/events`，服务端返回 401，不建立事件流。
- Scenario: SSE 连接中断后，前端自动重连并在恢复后补齐期间错过的状态变化（通过重拉 REST 数据）。
- Scenario: SSE 持续不可用（如反代缓冲导致连上但无事件到达）时，前端在心跳超时后自动降级为慢速定时轮询，页面数据仍能自动更新；SSE 恢复后轮询停止。
- Scenario: `go vet ./...` 与 `go test ./...`（至少覆盖 pkg/panel、pkg/webadmin）通过；pkg/webadmin/ui `npm run build`（vue-tsc + vite build）通过。
