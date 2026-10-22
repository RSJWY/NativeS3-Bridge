# panel-live-events 完整目标规格

管理面板的实时事件能力：面板进程在现有 admin 监听器上向已登录的管理员浏览器推送服务端事件（SSE），前端据此自动刷新页面数据。本规格描述该 capability 归档后的完整行为。

## 端点与鉴权

- `GET /api/admin/events`，挂载于 panel admin 服务器（admin_addr 监听器），位于现有 webadmin 会话 Cookie 鉴权中间件之后，与 `/api/admin/nodes` 等 REST 端点同一鉴权口径。
- 未携带有效会话 Cookie 的请求返回 401，不建立事件流。
- 响应头：`Content-Type: text/event-stream`、`Cache-Control: no-store`、`X-Accel-Buffering: no`。每条事件写入后立即 Flush。

## 事件类型与负载

事件为「通知式」：只携带事件类型与标识，不携带完整业务数据体；前端收到事件后重新拉取对应 REST 端点，REST 是唯一数据源。

| 事件 | 触发点 | data 字段（JSON） |
| ---- | ---- | ---- |
| `node_online` | 节点 WebSocket 注册成功（transport 的 Hub.Register 调用处） | `{"node_id": <id>}` |
| `node_offline` | 节点连接断开（transport 的 disconnect 调用处） | `{"node_id": <id>}` |
| `task_updated` | 任务状态流转：创建（pending）、派发（running）、结果上报（success/failed）、超时（failed）、连接断开标记（unknown） | `{"node_id": <id>, "task_id": "<id>", "state": "pending|running|success|failed|unknown"}` |

- 事件流格式遵循 SSE 标准（`event:`/`data:` 行）；事件带单调递增 `id:` 行，便于断线重连语义（客户端仍以重拉 REST 补齐状态，不做服务端回放）。

## 心跳与保活

- 服务端按固定间隔（心跳周期，量级 15s，具体值实现时定，须显著小于前端降级阈值）发送 SSE 心跳（comment 帧或专用 event），用于客户端判活与中间设备保活。
- 心跳只用于判活，不携带业务数据。

## 事件总线语义

- 进程内发布/订阅：发布点位于 transport（节点注册/断开）与 tasks（状态流转）的既有代码路径。
- 分发不阻塞发布者：订阅者（SSE 连接）消费过慢时对该订阅者丢弃积压事件或断开其连接，绝不背压阻塞节点控制面或任务编排路径。
- 订阅者在连接断开时清理，无泄漏。
- 面板单实例部署，不做跨实例广播。

## 前端订阅行为

- pkg/webadmin/ui 提供 EventSource 订阅 composable（useAdminEvents）：登录后建立连接，页面卸载/登出时关闭。
- 自动重连：依赖 EventSource 内建重连；重连成功后通过重拉 REST 数据补齐断线期间错过的状态变化。
- 降级轮询：客户端超过阈值（显著大于心跳周期，量级 45–60s）未收到任何事件或心跳时，判定 SSE 不可用，自动启动慢速定时轮询（量级 30s）刷新当前页面数据；SSE 恢复收到事件/心跳后停止轮询。
- 登录态失效：SSE 请求返回 401 时，按现有 markLoggedOut 逻辑处理（与 apiFetch 的 401 行为一致），不无限重连。
- 接入页面：PanelNodes（节点上下线→重载列表）、PanelDashboard（节点上下线→重载总览）、PanelNodeDetail（在线状态、任务事件）；PanelNodeLogsSection 的任务进度由 800ms 固定轮询改为事件驱动，保留一次性兜底拉取。
- 配置类 section（buckets/credentials/webhooks/rate-limit/desired-state）不接入事件，保持手动刷新与操作后局部刷新。

## 部署要求

- 反向代理场景必须允许 SSE 长连接透传：禁用响应缓冲（nginx `proxy_buffering off`，响应头已带 `X-Accel-Buffering: no`）、不设过短的读超时；相关要求写入 docs/public-deployment.md。
- 面板 admin 监听器原「管理面没有 SSE/流式端点」注释与超时配置说明同步更新；现有 REST 端点行为不变。

## 场景

- Scenario: 管理员登录后停留在 PanelNodes 页面，节点随后启动并注册成功，页面在数秒内自动显示该节点在线，全程无手动刷新。验收：A1
- Scenario: 管理员停留在 PanelDashboard 总览页，节点下线后总览的节点统计自动更新。验收：A2
- Scenario: 管理员在 PanelNodeDetail 触发导入任务并停留在任务进度区，任务状态变化（pending/running/success/failed/unknown）自动呈现，不依赖固定 800ms 轮询。验收：A3
- Scenario: 未携带会话 Cookie 请求 `GET /api/admin/events`，服务端返回 401，不建立事件流。验收：A4
- Scenario: SSE 连接中断后，前端自动重连，并在恢复后通过重拉 REST 数据补齐期间错过的状态变化。验收：A5
- Scenario: SSE 持续不可用（如反代缓冲导致连接建立但无事件到达）时，前端在心跳超时后自动降级为慢速定时轮询，页面数据仍自动更新；SSE 恢复后轮询停止。验收：A6
- Scenario: `go vet ./...` 与 `go test ./...`（至少覆盖 pkg/panel、pkg/webadmin）通过；pkg/webadmin/ui `npm run build` 通过。验收：A7
