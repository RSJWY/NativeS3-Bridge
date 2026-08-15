# 节点遥测基础

## Goal

让 Panel 仪表盘能展示各节点最近一次可靠上报的实际对象存储使用量、对象数和观测时间，并明确标识缺少遥测或遥测已过期的节点。旧版 Node 继续可连接、可服务；缺少遥测字段时必须显示“未上报/不可用”，不能把缺失解释成零。

## Background / Confirmed Facts

- 当前心跳协议已预留 `used_bytes_total` 和 `object_count`，但字段是非指针 `int64`，Node 心跳只发送 `applied_version`，Panel 只持久化版本和心跳时间，见 [pkg/controlproto/payloads.go:51](/opt/NativeS3-Bridge/pkg/controlproto/payloads.go:51)、[pkg/nodeagent/client.go:384](/opt/NativeS3-Bridge/pkg/nodeagent/client.go:384)、[pkg/panel/transport.go:366](/opt/NativeS3-Bridge/pkg/panel/transport.go:366)。
- Panel `NodeState` 是节点观测状态的唯一持久化行，当前包含在线、同步、区域和最近心跳，但没有容量、对象数或遥测观测时间，见 [pkg/panel/models.go:88](/opt/NativeS3-Bridge/pkg/panel/models.go:88)。
- Panel 健康仪表盘已经通过单一汇总接口聚合 `NodeState`/Hub/DesiredConfig；其当前设计明确排除了容量和对象数，见 [pkg/panel/dashboard.go:69](/opt/NativeS3-Bridge/pkg/panel/dashboard.go:69) 和归档任务 `08-15-panel-node-health-dashboard/design.md`。
- Node 本地数据库的 `Credential.UsedBytes` 是按凭据的配额记账，不是可靠的节点总量：跨凭据覆盖/删除可能使总和失真；`RequestStat` 的 PUT/DELETE 计数也不能推出当前对象数，见 [pkg/db/models.go:3](/opt/NativeS3-Bridge/pkg/db/models.go:3)、[pkg/quota/quota.go:60](/opt/NativeS3-Bridge/pkg/quota/quota.go:60)。
- 全盘统计目前由 `storage.ReconcileBucket` 的 `WalkDir` 完成，已有手动 storage scan/reconcile 任务；它会跳过 `.multipart`、元数据 sidecar 和数据库文件，见 [pkg/storage/reconcile.go:21](/opt/NativeS3-Bridge/pkg/storage/reconcile.go:21) 和 [pkg/nodeagent/tasks.go:188](/opt/NativeS3-Bridge/pkg/nodeagent/tasks.go:188)。
- S3 PUT、Copy、Delete、批量 Delete、Multipart Complete 都已在成功路径拿到对象大小和覆盖/存在信息，适合作为节点级增量计数器的更新点，见 [pkg/handlers/object.go:55](/opt/NativeS3-Bridge/pkg/handlers/object.go:55)、[pkg/handlers/object_ops.go:35](/opt/NativeS3-Bridge/pkg/handlers/object_ops.go:35)、[pkg/handlers/multipart.go:74](/opt/NativeS3-Bridge/pkg/handlers/multipart.go:74)。
- Panel 的心跳间隔默认 15 秒、离线倍数默认 3，见 [pkg/config/panel.go:117](/opt/NativeS3-Bridge/pkg/config/panel.go:117)；现有离线 sweeper 已按该口径标记 NodeState，见 [cmd/panel/main.go:141](/opt/NativeS3-Bridge/cmd/panel/main.go:141)。

## Requirements

### R1. Node telemetry contract

- Node 每次心跳在可用时上报：实际对象字节总量、当前对象数、RFC3339 UTC 观测时间。
- 新字段必须可选并保持协议版本 1 的向后兼容：旧 Node 不发送字段时，Panel 能区分“缺少字段”和合法的零字节/零对象。
- 观测时间是 Node 统计数据的时间，不使用 Panel 收到心跳的时间替代。
- 计数器更新必须覆盖普通 PUT（含覆盖）、服务端 Copy、单对象 Delete、批量 Delete 和 Multipart Complete；失败操作不能改变计数。
- Node 进程重启后从本地持久化状态恢复最新计数；无法证明计数可靠时应上报缺失/不可用，而不是伪造 0。

### R2. Node low-cost accounting and baseline

- 心跳读取必须是常量级数据库读取，不得每 15 秒执行全盘 `WalkDir`。
- 节点级计数器独立于凭据配额记账，不能通过 `SUM(credentials.used_bytes)` 或 `RequestStat` 推算。
- 现有 storage scan/reconcile 作为初始基线和显式修复入口；基线统计遵循现有 sidecar、`.multipart`、数据库文件排除规则。
- 外部直接改动 native 文件、历史数据升级和计数器不一致时，系统必须有明确的“遥测不可用/需要重建”状态，不能静默误报为 0。

### R3. Panel persistence and compatibility

- Panel 在 `NodeState` 持久化最新容量、对象数、观测时间以及“是否有可靠遥测”的状态；旧数据库通过 additive migration 升级。
- 每次心跳只更新存在的遥测字段；旧 Node 缺少字段时保留“未上报”语义，不覆盖成 0，也不把旧值伪装成当前观测。
- Panel 保存最新一份观测，不做历史趋势、时序表或聚合历史。

### R4. Panel summary API

- 现有 `GET /api/admin/dashboard/summary` 增加节点遥测汇总：总使用字节、总对象数、具备有效遥测的节点数、缺少遥测节点数、遥测过期节点数，以及按节点返回最近遥测摘要。
- 汇总只累加“有效且未过期”的节点遥测；缺失或过期节点不能贡献 0，也不能进入总量。
- 遥测过期判断使用现有 Panel 心跳配置口径：默认阈值为 `heartbeat_interval * offline_multiplier`，由 Panel 配置注入 API，不写死第二套阈值。
- 接口继续受现有管理认证保护、只允许 GET、一次响应满足首屏需要，不产生前端 N+1 请求。

### R5. Panel dashboard UX

- 仪表盘增加总使用容量、对象总数、有效遥测节点数和遥测问题节点数等指标，并显示数据生成时间。
- 节点明细显示遥测值、观测时间和状态：有效、未上报、已过期；缺失字段显示“未上报/不可用”，不显示 0。
- 页面保留现有健康指标、关注列表、加载/空/错误/刷新状态；刷新失败时保留上一份成功数据。
- 颜色必须配合文字表达状态；不新增历史图表、趋势、容量告警或轮询刷新。

## Compatibility and Safety Constraints

- 旧版 Node 与新版 Panel：心跳仍能解码，节点保持在线/同步功能；遥测字段为空且不计入 Panel 总量。
- 新版 Node 与旧版 Panel：新增 JSON 字段被旧 Panel 忽略，协议版本不变。
- Node 本地遥测表属于 node-agent additive state，不修改 standalone 旧版数据库迁移路径；Panel 遥测列属于 Panel `NodeState` additive migration。
- 遥测不包含密钥、对象 key、日志内容或高基数标签。
- 第一版不做历史趋势、时序数据库、容量告警、跨节点请求流量统计或 Panel 主动拉取节点。

## Acceptance Criteria

- [x] 新版 Node 的心跳在本地统计有效时包含 `used_bytes_total`、`object_count`、`observed_at`；合法的 0 字节/0 对象可被表达并与字段缺失区分。
- [x] 普通 PUT/覆盖、Copy、单删、批量删、Multipart Complete 的成功/失败测试证明节点级字节和对象数增量正确，零大小对象也正确计数。
- [x] 心跳路径只读取本地计数器；测试或代码检查证明心跳周期不调用全盘 `WalkDir`。
- [x] Node 重启后计数器和可靠性状态可恢复；基线未建立或被标记失效时，心跳省略遥测字段而非发送 0。
- [x] Panel 旧库迁移成功；新心跳持久化实际值和观测时间，旧心跳/缺少字段不会把列写成 0。
- [x] `GET /api/admin/dashboard/summary` 返回总使用字节、总对象数、有效/缺失/过期节点数及每节点遥测状态；缺失/过期节点不计入总量。
- [x] 遥测过期阈值与 `heartbeat_interval * offline_multiplier` 一致，并覆盖自定义配置测试。
- [x] 未认证返回 401，非 GET 返回 405，响应不泄露敏感配置。
- [x] Panel 仪表盘正确显示容量、对象数、观测时间和未上报/过期状态；加载、空数据、接口失败、刷新保留旧数据均可见。
- [x] standalone 仪表盘、导航和既有容量/请求图表行为不变。
- [x] 完整 Go 测试、前端构建及现有 Panel/ChromeDriver E2E 通过。

## Key Decisions

- 初始基线采用同步全量扫描：Node 首次启动在开始接受 S3 流量前扫描一次数据目录并写入节点级计数器。这样已有 native 文件能得到真实初值，且不会与在线写入产生扫描竞态；代价是首次启动时间随数据量增加。
- 后续心跳只读取持久化计数器；显式 storage reconcile 负责修复外部直接改盘或计数器失效，不引入每 15 秒全盘扫描。
- 在线 rebuild 与对象变更复用同一 recorder：对象变更持共享门闩直到文件提交和计数落库完成，rebuild 持独占门闩覆盖扫描与基线持久化；不使用无法覆盖交接窗口的 Epoch 检测。
- 失效状态同时使用内存 latch、数据库 `Valid=false` 和数据根 `.natives3-telemetry-invalid` 标记；重建成功落库后才清除标记，保证 DB 写失败或重建中崩溃后仍 fail-closed。
- 遥测阈值复用 Panel 的 `heartbeat_interval * offline_multiplier`，不新增第二套默认配置。
