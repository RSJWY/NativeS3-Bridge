# Panel 权威配置管理闭环

## Goal

让 Panel 成为 node 业务配置的真正唯一权威：管理员可以在节点详情页完整管理 bucket/ACL、credential、webhook、匿名限流和原地迁移导入；配置先作为草稿保存在 Panel，只有显式发布后才形成可重连、可校验、可安全应用的版本化期望状态。

## User Value

- 管理员不再需要手工 curl 或直接修改 node 数据库即可完成配置生命周期。
- Panel 不会因为“无法表达某类配置”而在全量下发时误删 node 上的有效业务配置。
- 离线 node 可以先保存草稿并发布最新版本，上线后自动对账；Panel 暂时不可用时 node 继续使用最后成功版本提供 S3。
- Secret Key、对象数据和迁移过程保持现有安全边界。

## Confirmed Facts

- Panel schema 和 `controlproto.DesiredState` 已包含 node-scoped bucket、webhook 和 rate-limit 模型，但管理路由目前只覆盖 credential 列表/创建/轮换，没有 bucket、webhook、rate-limit 或 credential 更新/删除路由（`pkg/panel/models.go:143`, `pkg/panel/adminapi.go:122`, `pkg/panel/adminapi.go:368`）。
- 当前显式发布语义不成立：`DesiredConfig` 保存已发布版本/hash，但 `BuildPushable` 会重新读取当前 Panel 表，因此未发布的修改可能被旧版本号推送，或与旧 hash 不一致（`pkg/panel/desired.go:94`, `pkg/panel/desired.go:152`）。
- hello 握手会返回 `needs_sync`，但连接注册后没有内建的自动 desired-state push；`cmd/panel` 也未配置 `OnConnected` 对账钩子（`pkg/panel/transport.go:194`, `pkg/panel/transport.go:267`, `cmd/panel/main.go:100`）。
- node executor 当前只写 credentials/buckets/webhooks 数据表：匿名限流未落库或应用，webhook manager 不会在 apply 后 reload，bucket ACL cache 不会失效（`pkg/nodeagent/executor.go:35`, `pkg/nodeagent/executor.go:85`, `cmd/node/main.go:121`）。
- node 当前仍以空 `RateLimitConfig` 启动固定中间件；desired-state 中的 `RateLimit` 不会改变实际请求限流（`cmd/node/main.go:121`, `pkg/server/router.go:58`）。
- import 后端 API 已存在，但前端 client/detail 页没有封装或入口（`pkg/panel/adminapi.go:579`, `pkg/webadmin/ui/src/api/client.ts:275`, `pkg/webadmin/ui/src/views/PanelNodeDetail.vue:90`）。
- import confirm 只用 credential 行判断“已托管”，且先提交权威表、再单独发布 baseline；发布失败可能留下半完成接管（`pkg/panel/migration.go:205`）。
- 既有架构约束明确：Panel 权威管理业务配置；对象文件和运行统计由 node 权威，控制面不得删除或搬迁对象数据（`.trellis/tasks/archive/2026-07/07-13-multi-node-mtls-control-plane/design.md`）。

## Requirements

### R1. Node-scoped draft CRUD

- 在 `/api/admin/nodes/{id}` 下增加：
  - bucket list/create/delete 与 ACL update；
  - credential update/enable/disable/delete，保留 create/rotate；
  - webhook list/create/update/delete；
  - anonymous rate-limit get/upsert/reset。
- 所有写操作继续位于管理员 Session 鉴权之后，并写入不含 Secret/Token 的审计记录。
- 所有资源严格按 `node_id` 隔离；跨节点 ID/access key/name 不得误操作。
- credential 的非空 bucket 绑定必须引用同一 node 的现有 bucket；删除 bucket 时若仍有绑定 credential，返回冲突。
- Secret Key 只在 credential create/rotate 响应中返回一次，列表、更新、删除、审计、期望快照持久化均不得出现明文。

### R2. Draft 与 published snapshot 严格分离

- `node_credentials/node_buckets/node_webhooks/node_rate_limits` 表示可编辑草稿。
- `desired_configs` 表示最后一次显式发布的完整、不可被后续草稿修改影响的快照。
- 已发布快照必须持久化 credential ciphertext（不是明文，也不是依赖当前草稿重新拼装的空 Secret）；push 时仅从该快照解密并构造 wire payload。
- `Push current version`、自动重连对账和离线恢复只能发送最后已发布快照，绝不能夹带未发布草稿。
- 节点 API/UI 暴露“有未发布变更”和“旧快照需要重新发布”状态；无草稿变化时不误导管理员。
- 旧格式 `desired_configs.content_json` 无法安全恢复精确快照时必须 fail closed，并提示管理员重新发布；不得用当前草稿冒充旧版本。

### R3. 完整且立即生效的 node apply

- node 在写入前校验 payload hash、bucket/credential 引用、ACL、webhook、rate-limit 等完整约束；无效快照不得部分应用。
- credentials、buckets、webhooks、rate-limit 与 `AgentMeta` 的 applied version/hash 在同一数据库事务内提交；失败保留此前可用配置。
- 事务成功后立即、无失败窗口地：
  - 失效受影响 credential cache；
  - 失效 bucket ACL/existence cache；
  - 替换 webhook manager 的运行时 hook 集合；
  - 替换匿名 rate-limit 运行时策略。
- node 重启后从持久化的 managed rate-limit 状态恢复相同策略；未配置策略继续使用现有默认值。
- 新 Panel 配置能力必须有向后兼容的 capability 标识；新 Panel 不向不支持完整权威 apply 的旧 node 推送并假报同步，旧 Panel 可忽略新 node 的可选 capability 字段。

### R4. Bucket 与对象数据安全边界

- Panel 删除 bucket 是删除“受管声明”，不得通过 desired-state 删除或搬迁磁盘对象。
- managed node 的 S3 视图以受管 bucket 声明为准：未声明但磁盘仍存在的目录不得出现在 ListBuckets，也不得被 signed/anonymous 请求继续访问。
- managed node 不接受绕过 Panel 的 S3 CreateBucket/DeleteBucket；standalone 模式保持现有行为。
- 新声明 bucket 可安全创建空目录；若同名未受管目录包含保留对象，apply 必须失败并给出可定位错误，避免重新声明时意外暴露旧数据。
- ACL 更新在 apply 成功后立即生效，不等待缓存 TTL。

### R5. 发布、推送与重连

- 配置 CRUD 只修改草稿，不自动生成版本；管理员显式发布后才创建单调递增版本。
- 发布在线 node 时可 best-effort 立即推送；离线 node 保存最新发布版本。
- node hello 报告的版本/hash 与已发布快照不一致时，Panel 在连接注册后自动推送精确快照。
- push/apply 失败必须落入可观察的 waiting/failed/drift 状态和脱敏错误；不得把不完整运行态报告为 synced。

### R6. In-place import UI 与原子确认

- 在节点详情页封装并展示：
  - `POST /nodes/{id}/import` 请求只读报告；
  - `GET /nodes/{id}/import` 恢复 pending 摘要；
  - `POST /nodes/{id}/import/confirm` 确认接管；
  - `POST /nodes/{id}/import/abort` 中止。
- UI 流程为“请求导入 → 审阅摘要 → 确认接管/中止”，摘要只显示 bucket 名、access key 与数量/hash，绝不显示 secret。
- 请求导入仅允许在线 node；离线、超时、无 pending、已托管等错误给出明确提示。
- 已托管判断覆盖 credential、bucket、webhook、rate-limit 和 existing desired config，而不是只检查 credential。
- confirm 必须在一个数据库事务内完成权威表接管和 version=1 published snapshot；失败不得留下半完成状态。
- confirm 前 Panel 权威表和 node 业务配置均零写入；confirm 本身仍只写 Panel，不主动改写 node。

### R7. Panel UI 完整生命周期

- 节点详情页提供独立的迁移、bucket、credential、webhook、rate-limit 配置区块；必要时拆分 Vue 子组件，避免继续扩大单文件。
- credential bucket 使用同节点 bucket select；支持编辑名称、bucket、配额、启停和删除。
- bucket 支持创建、ACL 切换和带安全说明的删除确认。
- webhook 使用受支持事件的显式选择，不要求管理员手写逗号字符串。
- rate-limit 表单显示 effective/default 状态，并对 `trust_forwarded` 给出可信代理安全警告。
- 页面明确区分“未发布草稿”“已发布版本”“已应用版本”；push 按钮只表示重推已发布版本。
- 所有一次性 token/secret 仅保存在组件本地并在关闭结果框时清除；错误不得形成未处理 Promise rejection。

### R8. Validation and compatibility

- bucket 名/ACL、credential name/status/quota/bucket、webhook URL/events、rate-limit 数值均在 API 边界校验，错误不泄漏 SQL/内部细节。
- GORM schema 继续兼容 SQLite、MySQL、PostgreSQL；node 新持久状态只增表/增列，不修改或删除既有业务表。
- 现有 standalone WebAdmin 与 S3 bucket CRUD 行为保持不变；managed-only 规则只由 `cmd/node` 启用。
- 控制协议新增字段必须 optional；未知字段可被旧 peer 忽略。

## Acceptance Criteria

- [ ] Panel UI 可完成 node-scoped bucket/ACL、credential、webhook、rate-limit 的创建、查看、更新/启停、删除/重置，并显示未发布状态。
- [ ] credential 不可绑定不存在的同节点 bucket；有绑定 credential 的 bucket 不可删除。
- [ ] 草稿修改后重推旧版本或 node 重连只得到旧 published snapshot；显式发布后才得到新版本和新内容。
- [ ] `desired_configs`、API list/detail、日志与审计均不含明文 Secret；create/rotate 仍只返回一次。
- [ ] 在线 node 应用后 credential、ACL、webhook 和 anonymous rate-limit 的实际 S3 运行行为立即变化，且 hash/version 报告为 synced。
- [ ] 离线 node 可保存并发布配置；上线后自动收到最新发布版本，Panel 暂停期间继续用最后成功版本服务。
- [ ] managed node 无法通过 S3 直接创建/删除 bucket；删除 Panel bucket 后磁盘对象仍保留，但 bucket 不再列出或可访问。
- [ ] 同名保留数据阻止无意重新声明；错误清楚且不会暴露对象名列表。
- [ ] 管理员可在节点详情页完成 import 请求、摘要审阅、确认/中止；摘要不泄露 secret，confirm 前无业务写入，confirm 失败无半完成接管。
- [ ] 旧 node 不支持完整 authoritative apply 时，新 Panel 拒绝推送并给出升级提示，而不是报告 synced。
- [ ] standalone 模式回归测试、Panel API 测试、node executor/runtime 测试、前端 type-check/build 与真实浏览器验收全部通过。

## Out of Scope

- 通过 Panel 永久清除、迁移、恢复或下载 node 磁盘对象；本任务只隐藏已删除声明并保留数据安全边界。
- 跨节点批量配置、模板、复制或故障转移。
- 多管理员、RBAC、OIDC。
- 自动导入旧 YAML 中按既有安全网 B 被忽略的业务字段；原地 import 继续以 node 可报告的持久业务状态为准。
- 大规模性能测试和长时间 webhook/rate-limit 压测；由后续 E2E release-gate 子任务覆盖完整发布门。

## Open Questions

无阻塞产品问题；等待用户审阅最终 PRD、技术设计和实施计划后批准进入实现阶段。
