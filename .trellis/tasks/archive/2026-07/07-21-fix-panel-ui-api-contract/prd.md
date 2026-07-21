# 修复 Panel 管理界面 API 契约错配

## Goal

Panel 部署后必须展示与集中控制面相匹配的管理界面，不再加载只适用于旧单体 WebAdmin 的 dashboard、bucket、credential 和日志接口。管理员应能从浏览器完成最核心的节点接入与配置下发流程，同时保留旧单体 WebAdmin 构建/运行时的现有界面和 API 行为。

## Confirmed Facts

- `cmd/panel` 通过 `pkg/panel.AdminServer` 仅注册登录接口和 `/api/admin/nodes*` 节点作用域接口。
- Panel 当前嵌入 `pkg/webadmin/ui/dist`，该 SPA 登录后固定请求 `/api/admin/dashboard/*`、`/api/admin/buckets`、`/api/admin/credentials` 和 `/api/admin/logs`。
- Panel 的 SPA fallback 会对这些不存在的 `/api/*` 路径返回 `404 {"error":"not found"}`；问题可由仓库代码直接复现，与反向代理路径无关。
- 同一个 `pkg/webadmin/ui` 仍被旧单体 `pkg/webadmin.Server` 使用，因此修复不能把 standalone WebAdmin 的页面和接口硬替换掉。
- Panel API 已支持节点列表/创建/状态变更/退役、注册令牌、节点凭证创建与轮换、期望状态发布/推送、证书查看/撤销、任务和迁移接口。

## Requirements

- `/api/admin/auth-settings` 必须返回明确的服务运行模式，前端不得通过触发一组预期 404 来猜测模式。
- standalone 模式继续显示现有仪表盘、密钥、桶和日志页面，现有 API 请求路径与登录行为保持兼容。
- panel 模式只显示 Panel 可提供的数据和操作，首屏不得调用 standalone 专属 API。
- Panel 管理界面至少支持：
  - 列出、创建和选择节点；显示在线状态、生命周期状态、同步状态、期望/已应用版本和最近心跳。
  - 启用/停用节点；永久退役必须二次确认并明确不可逆。
  - 为节点签发一次性注册令牌，并只在当前结果区域显示一次明文与过期时间。
  - 列出、创建和轮换节点 S3 credential；新 secret 只在创建/轮换结果中显示一次，不写入持久状态。
  - 发布新的期望状态；节点在线时允许重推当前期望状态，并展示结果/错误。
- panel 模式中的 API 错误必须在页面可见；`401` 继续清理本地登录态并跳转登录页。
- 桌面与窄屏下主要操作均可达；视觉样式沿用现有克制的管理后台语言。
- 添加后端和前端回归验证，覆盖运行模式契约以及 Panel 首屏不会请求不存在的 standalone API。

## Acceptance Criteria

- [x] Panel 的 `GET /api/admin/auth-settings` 返回 `service_mode: "panel"`；standalone 返回 `service_mode: "standalone"`。
- [x] 登录 Panel 后进入节点管理首屏，浏览器网络请求中不出现 `/api/admin/dashboard/*`、`/api/admin/buckets`、顶层 `/api/admin/credentials` 或 `/api/admin/logs` 的 404。
- [x] Panel UI 能创建节点、签发注册令牌，并能看到一次性 token 与有效期。
- [x] Panel UI 能创建/列出/轮换节点 credential；列表响应和列表 UI 均不包含 secret，创建/轮换结果仅临时显示一次。
- [x] Panel UI 能切换 active/disabled、发布期望状态，并在在线节点上执行 push；失败状态有可读提示。
- [x] Panel UI 的退役操作需要明确确认，成功后节点显示 retired 且不再提供危险操作。
- [x] standalone WebAdmin 登录后仍进入 `/dashboard`，原有 dashboard/credentials/buckets/logs 导航和 API 行为不变。
- [x] `go test ./pkg/webadmin ./pkg/panel`、前端类型检查/生产构建、Panel 二进制构建通过。
- [x] 至少一个服务级测试验证 Panel 管理服务器对运行模式和节点 API 的路由契约，避免只验证 SPA 返回 HTTP 200 的弱测试再次漏过问题。

## Out of Scope

- 本任务不新增 Panel bucket/webhook/rate-limit CRUD API；这些权威模型虽存在，但当前后端没有对应管理路由。
- 本任务不重构 Panel/node 控制协议，也不改变数据库 schema。
- 本任务不改变反向代理、端口或 Docker Compose 拓扑。
