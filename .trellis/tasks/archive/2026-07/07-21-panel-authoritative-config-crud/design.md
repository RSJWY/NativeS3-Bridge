# Panel 权威配置管理闭环 — 技术设计

## 1. Architecture and State Ownership

本任务把“Panel 表就是已发布状态”的隐式模型拆成三个明确层次：

```text
Panel draft tables
  node_credentials / node_buckets / node_webhooks / node_rate_limits
                         |
                         | explicit Publish
                         v
DesiredConfig vN (exact encrypted snapshot + plaintext-derived content hash)
                         |
                         | mTLS desired_state (manual push or reconnect reconcile)
                         v
Node managed DB + runtime controllers + AgentMeta(applied vN/hash)
```

- Draft：管理员可编辑，允许 node 离线，不影响当前已发布版本。
- Published snapshot：只保留最新版本，内容固定；后续草稿修改不得改变它。
- Applied state：node 最后成功应用的本地副本；Panel 不可用时继续服务。
- Observed data：对象文件、used bytes、request stats 和运行日志继续由 node 权威。

## 2. Panel Draft API Contracts

所有路由继续挂在现有 `/api/admin/nodes/{id}/...` dispatcher 下，并由 `AdminServer` 的 Session middleware 统一保护。

| Resource | Methods | Contract |
|---|---|---|
| `buckets` | `GET`, `POST` | list/create `{name, acl, created_at}`；create 默认 `private` |
| `buckets/{name}` | `DELETE` | 有同节点 credential 绑定时 `409`；只删 Panel 声明 |
| `buckets/{name}/acl` | `PUT` | 只接受 `private` / `public-read` |
| `credentials` | existing `GET`, `POST` | create 校验 bucket；Secret 仅 create 响应一次 |
| `credentials/{ak}` | `PATCH`, `DELETE` | 更新 name/bucket/status/quota 或删除 |
| `credentials/{ak}/rotate` | existing `POST` | Access Key 不变，新 Secret 仅本次响应 |
| `webhooks` | `GET`, `POST` | API 使用 `events: string[]`；持久化为 canonical comma string |
| `webhooks/{id}` | `PATCH`, `DELETE` | node-scoped numeric ID；URL/events/enabled 可更新 |
| `rate-limit` | `GET`, `PUT`, `DELETE` | GET 返回 configured + effective values；PUT upsert；DELETE 恢复默认 |
| `import` | existing `GET`, `POST` | pending summary / request report |
| `import/confirm` | existing `POST` | 原子接管 + baseline publish |
| `import/abort` | existing `POST` | 丢弃内存 pending snapshot |

Validation stays server-owned:

- bucket name 复用 `storage.ValidateBucketName`，ACL 复用 storage 常量。
- credential name 最多 128 rune；quota `>= 0`；status 仅 enabled/disabled；bucket 必须属于同 node。
- webhook URL 仅允许绝对 `http`/`https` URL；events 非空、去重、canonical order，当前只允许 `ObjectCreated`、`ObjectDeleted`。
- rate limit `anonymous_rps > 0`、`anonymous_burst > 0`；`trust_forwarded=true` 仅提示风险，不替管理员推断网络拓扑。
- 所有查改删查询同时带 `node_id`，不存在返回 `404`，冲突返回 `409`。

建议将 draft CRUD/validation 从不断扩大的 `adminapi.go` 拆到 package-local resource/store files；HTTP handler 负责 decode/status/audit，store 负责事务和 node-scoped invariant。

## 3. Exact Published Snapshot

### 3.1 Persisted shape

`DesiredConfig.ContentJSON` 改为 Panel 内部、带 schema version 的持久快照，而不是 masked `controlproto.DesiredState`：

```go
type persistedDesiredSnapshot struct {
    SchemaVersion int                         `json:"schema_version"`
    Credentials   []persistedCredential       `json:"credentials"`
    Buckets       []controlproto.DesiredBucket `json:"buckets"`
    Webhooks      []controlproto.DesiredWebhook `json:"webhooks"`
    RateLimit     *controlproto.DesiredRateLimit `json:"rate_limit,omitempty"`
}

type persistedCredential struct {
    AccessKey       string `json:"access_key"`
    SecretKeyCipher string `json:"secret_key_cipher"`
    Name            string `json:"name,omitempty"`
    Bucket          string `json:"bucket,omitempty"`
    Status          string `json:"status"`
    QuotaBytes      int64  `json:"quota_bytes"`
}
```

- JSON 中只有 AEAD ciphertext；Panel DB 单独泄漏仍不能恢复明文 Secret。
- Publish 从 draft 表构造 canonical persisted snapshot，并临时解密得到 wire `DesiredState` 计算真实 content hash。
- `BuildPushable` 只解码 `DesiredConfig.ContentJSON`，解密其中 ciphertext，重新计算并核对 hash 后发送；它不再查询当前 draft 资源。
- Draft dirty 通过比较 canonical persisted draft 与已发布 snapshot 判断，不需要在节点列表请求中解密 Secret。

### 3.2 Legacy snapshot handling

旧 `ContentJSON` 没有 `schema_version`，且 credential secret 已被置空，无法证明能重建原版本。处理策略：

- 读取时返回 typed `ErrDesiredSnapshotRepublishRequired`。
- 自动 push 和手动 push fail closed，并把安全、可操作的错误写入 node sync state/API。
- UI 显示“旧快照需重新发布”；管理员显式 Publish 后生成新 schema snapshot 和更高版本。
- 不在 migration 中静默用当前 draft 覆盖旧版本，也不在原版本号下改变 hash。

### 3.3 Draft status in node response

`nodeResponse` 增加 additive 字段：

- `draft_dirty: boolean`
- `publish_required: boolean`（legacy/unpushable snapshot）

UI 用它们控制 banner 与 Publish 按钮；`desired_version`/`applied_version` 语义保持不变。

## 4. Publish and Reconnect Flow

### 4.1 Explicit publish

1. CRUD 写 draft 表并审计，不变更 `DesiredConfig`。
2. Publish 在一个 transaction 中读取 canonical draft、生成 snapshot、计算 real hash、upsert `DesiredConfig(version+1)`。
3. 在线 node best-effort push；离线只保存版本。
4. Push current 只发送 `DesiredConfig` 的 exact snapshot。

### 4.2 Reconnect reconcile

握手解析 hello 时把 peer applied version/hash 和 capability 记录在 `AgentConn`。连接进入 Hub 后：

- 若 desired version/hash 已一致，不发消息。
- 若不一致且 peer 支持 `authoritative_config_v1`，立即发送 exact snapshot。
- 若不支持，保持控制连接用于健康/兼容能力，但拒绝 config push，NodeState 记录“agent upgrade required”。

`HelloPayload` 增加 optional `capabilities []string`；旧 Panel 会忽略，新 Panel 能避免旧 node 对部分配置“写 DB 但运行态未生效”后假报 synced。协议版本无需破坏性提升。

## 5. Node Apply and Runtime Controllers

### 5.1 Persistent managed rate limit

在 `pkg/nodeagent` 的 additive migration 中增加单行 managed rate-limit 表（或等价的 Agent-owned additive state），字段保持跨 SQLite/MySQL/Postgres 可移植。无行表示使用统一默认策略。

启动顺序：

1. migrate base DB + agent tables；
2. load persisted managed rate-limit/default；
3. construct dynamic rate-limit controller；
4. construct S3 server；
5. start agent with executor dependencies。

### 5.2 Apply contract

将 executor 接口提升为接收 version/hash/content 的一次操作：

1. canonical validation：payload hash 必须等于 content hash；验证所有资源和 credential bucket references。
2. prepare runtime snapshots：credential invalidation keys、bucket changes、parsed webhook hooks、rate-limit snapshot。
3. 一个 GORM transaction 内 reconcile credentials/buckets/webhooks/managed-rate-limit，并 upsert `AgentMeta(applied_version, content_hash)`。
4. commit 后执行不返回错误的 runtime swaps：cache invalidation、hook snapshot replace、rate-limit controller swap。
5. 回读 local state/hash 做测试与 drift 断言；ack 只在完整成功时为 synced。

Webhook manager 增加“由已校验 config 原子替换内存 hooks”的方法；正常启动的 `Reload()` 仍从 DB 恢复。Rate-limit controller 使用 atomic/current snapshot，更新时替换 limiter set，避免在请求路径访问 DB。

### 5.3 Managed bucket mode

为 `cmd/node` 增加 additive managed-server wiring，现有 standalone constructors/behavior 不变：

- root `ListBuckets` 从 `BucketStore.List()` 读取受管声明，而不是扫描磁盘目录。
- bucket-specific signed/anonymous 请求在进入 handler 前要求存在受管 metadata row。
- node 模式的 S3 CreateBucket/DeleteBucket 返回明确拒绝；生命周期只从 Panel 进入。
- apply 新增 bucket 时确保空目录存在；删除声明只删 DB row并 invalidate cache，不删除目录或对象。
- 若新增声明对应“DB 无 row 但磁盘同名目录非空”，apply 整体失败；错误只报告 bucket 名，不枚举对象。

该模型同时满足：删除不丢对象、删除后不可继续访问、同名重建不意外暴露历史数据。

## 6. Import Hardening

### 6.1 Already-managed check

`Confirm` 在同一 transaction 内检查以下任一存在即返回 `ErrAlreadyManaged`：

- `NodeCredential`
- `NodeBucket`
- `NodeWebhook`
- `NodeRateLimit`
- `DesiredConfig`

### 6.2 Atomic adoption

将 desired authority 的 publish primitive 支持传入 transaction handle：

1. pending snapshot 仍仅存内存，收到报告后立刻加密 secrets。
2. Confirm transaction 插入所有 draft rows。
3. 同一 transaction 内从这些 rows 生成 schema-versioned version=1 snapshot。
4. transaction commit 后删除 pending entry 并写 audit。

任何插入、加密、snapshot encode 或 desired upsert 失败都会回滚全部 Panel business rows；node 从未被写入。

### 6.3 UI state machine

```text
idle/no pending
  -> request (online only)
  -> reviewing summary
       -> confirm -> baseline published
       -> abort   -> idle
```

- 页面加载时 GET pending；404 规范化为 `null`，不是页面错误。
- 请求期间有 30 秒等待态；离线/超时显示针对性文案。
- summary 仅保留 counts、bucket names、access keys、hash。
- confirm 后刷新 node、draft resources 和 desired version；不自动展示或持久化任何 secret。

## 7. Frontend Structure

保留 `/nodes/:id` 路由，把详情页拆为 focused components，避免在现有约 500 行文件继续堆叠：

- `PanelNodeImportSection.vue`
- `PanelNodeBucketsSection.vue`
- `PanelNodeCredentialsSection.vue`
- `PanelNodeWebhooksSection.vue`
- `PanelNodeRateLimitSection.vue`

父页面继续负责 node lifecycle、registration、published/applied 状态和 shared refresh。各 section 只持有自己的 loading/error/form state，并通过事件通知父页面刷新 `draft_dirty`。

`client.ts` 增加 typed resource interfaces/methods，并让 HTTP error 保留 status code，以便将 pending import 404、managed conflict 409、offline 409、timeout 504 映射为明确 UI 状态。

UI 继续遵守现有风格：简单 panel/table/form、复用 `.table-scroll`/`.state-row`/`.status-badge`，不新增状态库，不把 secret/token 放入全局 state 或 storage。

## 8. Compatibility and Rollout

- Panel DB 不新增必须的 desired snapshot 列，复用 `ContentJSON` 并通过 JSON schema version 演进；NodeRateLimit 现有表保持。
- node DB 仅新增 Agent-owned rate-limit state；不改 credentials/buckets/request_stats/hook_configs。
- `HelloPayload.capabilities` 为 optional，旧 peer 忽略。
- 新 Panel + 旧 node：允许连接/观察/import，但拒绝 authoritative push，提示升级 node。
- 旧 Panel + 新 node：旧 Panel 忽略 capabilities，新 node 可完整应用旧格式 desired payload。
- standalone binary 和 UI API 不进入 managed bucket mode，行为不变。

## 9. Failure and Rollback

- Draft CRUD 错误不影响已发布 snapshot 或 node 服务。
- Publish 失败不更新版本；旧 snapshot 仍可推送。
- Push/connection 失败只改变 observed sync state；node 保持最后成功配置。
- Apply transaction 失败不更新 AgentMeta；node 保持旧 DB/runtime。
- Legacy snapshot 无法精确恢复时 fail closed，管理员重新发布；不提供明文 Secret 导出后门。
- 代码回滚不需要删除新增 nodeagent 表；旧二进制忽略它。回滚前按既有运维文档 disable node，避免继续下发。

## 10. Verification Strategy

- Panel unit/API tests：所有 CRUD、node scope、validation、audit、secret redaction、draft dirty、exact snapshot、legacy fail-closed。
- Migration tests：所有 managed resource 检测、confirm transaction rollback、pending/abort、summary redaction。
- Transport tests：capability gate、reconnect auto-push、exact published payload。
- Node executor/server tests：atomic apply、rate-limit persistence/runtime、webhook replace、ACL cache invalidation、managed bucket visibility/direct mutation rejection/retained data guard。
- Frontend：`npm ci`, type-check/Vite build，以及真实 Chrome 对节点详情 CRUD、draft/publish、import UI 状态与无跨 mode API 请求的检查。
- Full gate：`gofmt`, focused Go tests, `go test ./...`, `go vet ./...`, `go build ./...`；最终由 sibling E2E release-gate 任务补齐 Panel→Node→S3 全链路发布门。
