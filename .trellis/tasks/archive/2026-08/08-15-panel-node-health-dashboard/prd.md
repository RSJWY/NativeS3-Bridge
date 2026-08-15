# Panel 节点健康仪表盘规划

## Goal

为集中管理模式增加一个 Panel 首页仪表盘，让管理员打开后能在一个页面内判断节点是否在线、配置是否同步，以及哪些节点需要处理。

用户价值：第一版使用 Panel 已经可靠采集的控制面状态，避免展示没有数据支撑的容量或流量指标。

## Background / Confirmed Facts

- Panel 当前导航只有“节点管理”和“Panel 日志”；独立节点模式才显示现有 `/dashboard`，见 `pkg/webadmin/ui/src/App.vue:27-37`。
- Panel 的节点列表响应已经包含 `online`、`status`、`applied_version`、`desired_version`、`sync_state`、`last_error`、`draft_dirty`、`publish_required`、`region`、`last_heartbeat`，见 `pkg/panel/adminapi.go:64-80`。
- 上述字段由 `nodeToResponse` 从 Hub、`NodeState` 和 `DesiredConfig` 组合得到，见 `pkg/panel/adminapi.go:848-875`。
- Panel 的 `NodeState` 已持久化在线状态、同步状态、最近错误、区域和最近心跳，见 `pkg/panel/models.go:88-103`。
- 心跳协议预留了 `used_bytes_total` 和 `object_count`，但当前 node agent 发送心跳时只填充 `applied_version`，Panel 处理心跳时也只落库版本和心跳时间，见 `pkg/controlproto/payloads.go:51-57`、`pkg/nodeagent/client.go:398-400`、`pkg/panel/transport.go:366-374`、`pkg/panel/transport.go:567-575`。
- Panel 数据库拥有受管 Bucket、密钥配额等声明数据，但没有节点实际使用量和历史请求统计，见 `pkg/panel/models.go:105-121`、`pkg/panel/models.go:152-162`。

## Requirements

### R1. Panel 首页入口

- 在 `service_mode = panel` 时增加“仪表盘”入口，并将 Panel 登录后的默认首页指向该页面。
- 独立节点模式的现有仪表盘行为保持不变。

### R2. 概览指标

页面顶部显示以下可由现有节点列表聚合得到的指标：

- 节点总数：包含 active、disabled、retired，并在标签或辅助信息中明确统计口径。
- 在线节点数：非 retired 且 `online = true` 的节点数。
- 离线节点数：非 retired 且 `online = false` 的节点数。
- 已退役节点数：`status = retired` 的节点数，作为辅助信息或健康分布中的独立项展示。
- 需要处理数：满足任一条件的非退役节点数：`sync_state = failed`、`sync_state = drift`、非退役离线（`online = false`）、`sync_state = waiting`、`draft_dirty = true`、`publish_required = true`。严重性阶梯固定为 同步失败/漂移 > 离线 > 待同步/待发布（与 design §3 统一，修订于 2026-08-15 验收）。

指标卡点击后应能跳转到节点管理，并带上对应筛选条件（若筛选能力未在第一版实现，则点击不做伪交互，使用普通非交互展示）。

### R3. 需要关注列表

按严重性和最近心跳排序显示需要处理的节点，至少包含：

- 节点名称和 ID。
- 在线/离线状态。
- 同步状态及当前版本 / 目标版本。
- 区域；未上报时显示“未上报”，不能解释为无区域。
- 最近心跳时间。
- 最近错误；无错误时显示占位符。
- 进入节点详情的操作。

严重性顺序固定为：同步失败/漂移 > 离线 > 待同步/待发布 > 其他。

没有异常节点时显示明确的空状态，而不是空白表格。

### R4. 节点健康分布

- 显示在线、离线、同步正常、待同步、同步失败/漂移的数量分布。
- 分布图或列表必须使用与状态含义一致的颜色和文字，并提供无节点、无状态数据的空状态。
- 区域只作为辅助分组信息，不作为健康结论。

### R5. 数据刷新与错误处理

- 页面首次进入加载一次数据。
- 提供手动刷新按钮，刷新期间禁止重复请求并展示进行中状态。
- API 失败时显示可读错误，保留上一份已成功加载的数据（若存在）。
- 不把缺失的 `last_heartbeat`、`region`、`last_error` 渲染成异常字符串或当前时间。

### R6. 后端汇总接口

- 新增一个受现有 Panel 管理认证保护的只读汇总接口，推荐路径：`GET /api/admin/dashboard/summary`。
- 接口一次返回概览指标、状态分布和需要关注的节点，避免前端对每个节点发起 N+1 请求。
- 返回值必须来自 Panel 当前数据库 / Hub 观测值；不得推算实际容量、对象数、请求量或错误率。

## Out Of Scope

- 实际已用容量、对象数量、上传/下载流量、PUT/GET/DELETE 趋势、请求成功率。
- 从日志文本反推统计指标。
- 节点心跳协议扩展、节点采集逻辑、历史指标表或时序存储。
- Bucket / 密钥配额汇总卡；可作为后续版本单独增加，不阻塞第一版节点健康页面。
- 自动轮询、告警通知、导出报表、跨时间范围筛选。

## Acceptance Criteria

- [ ] Panel 模式登录后可从导航进入仪表盘，且默认首页为仪表盘；standalone 模式导航和默认首页不变。
- [ ] 页面只展示由 `GET /api/admin/dashboard/summary` 返回的数据，不能在浏览器端对节点逐个请求再拼装主指标。
- [ ] 总节点、在线、离线、需要处理四个指标与节点列表逐条聚合结果一致。
- [ ] `failed` / `drift` 节点在需要关注列表中排在离线、待同步/待发布节点之前；同级按最近心跳从旧到新优先，缺失心跳排在最后。
- [ ] `draft_dirty`、`publish_required`、非退役离线或 `waiting` 的节点会进入需要关注列表，即使其 `sync_state` 为空或为 `synced`；关注计数与列表长度一致。
- [ ] 节点处于 `retired` 时计入总节点和已退役数，但不计入在线、离线或健康分布；即使 Hub 残留在线连接也不能被误判为健康在线节点。
- [ ] `region`、`last_heartbeat`、`last_error` 缺失时分别显示“未上报”、时间占位符和“—”，不抛出渲染错误。
- [ ] 无节点、无异常节点、接口加载中、接口失败、手动刷新中均有可见状态。
- [ ] API 未认证返回 401；非 GET 返回 405；正常响应使用稳定 JSON 字段，不泄露密钥或敏感配置。
- [ ] 后端单元测试覆盖统计口径、严重性排序、缺失字段和空数据；前端测试或浏览器验收覆盖 Panel/standalone 路由分流和主要状态。

## Decisions

- 节点总数包含所有生命周期节点；在线、离线和健康分布排除 retired，retired 单独计数。
- 第一版 `attention_nodes` 返回全部异常节点，不引入分页或数量上限。
- 第一版概览卡为非交互展示；节点处理入口统一位于需要关注列表。
- 无阻塞开放问题。
