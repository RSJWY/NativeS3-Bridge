# Design — 面板节点配置查看区域

## 1. 现状与可行边界

三层配置状态彼此分离（spec `panel-authoritative-config-guidelines.md` §3）：

```
草稿 (node_credentials / node_buckets / node_webhooks / node_rate_limits)
  │  显式 publish
  ▼
已发布快照 (desired_configs：version + content_json + content_hash，仅保留最新一版)
  │  push
  ▼
节点已应用 (AgentMeta：只有 AppliedVersion + ContentHash)
```

**第三层不含配置内容。** 节点的 hello / heartbeat / ack 三种消息都只回传 version 与 hash（`pkg/controlproto/payloads.go`）。节点确实能产出完整生效配置（`nodeagent.Executor.LocalState()`），但唯一的传输通道是 `import_request` → `import_report`，而它被 `nodeAlreadyManaged()` 挡住，对已纳管节点不可用。任务类型也是封闭的三种（`log_query` / `storage_scan` / `storage_reconcile_apply`），无一返回配置。

结论：**本区域能诚实展示的是已发布快照 + 草稿状态，做不到"节点实况"。** 想要实况需要新增控制面消息类型，不在本任务范围。

`transport.go` 的 `handleAck` 已经在 hash 不一致时记为 `drift`，所以"是否存在分歧"面板已知，只是"分歧在哪"不知。UI 可以据此提示，但不能伪装成内容级 diff。

## 2. 方案选择

### 2.1 展示什么

| 方案 | 内容 | 评价 |
|---|---|---|
| A | 只展示已发布快照 | **采用。** 语义最干净，是"面板作为权威源发布了什么"的唯一真相，且它就是会被推给节点的字节 |
| B | 只展示草稿聚合 | 草稿已被六个分资源区域完整覆盖，重复且无新信息 |
| C | 已发布 + 草稿双栏 diff | 信息量最大，但需要新增草稿聚合端点与前端 diff 渲染，复杂度远超一句话的 issue 需求 |

选 A，并用已有的 `draft_dirty` / `publish_required` 字段（`nodeResponse` 已提供）在区域内提示"草稿有未发布变更"，无需新端点即可覆盖 C 的主要价值。

### 2.2 数据来源

`DesiredStateAuthority` 已有三个入口：

- `Build(nodeID)` — 读草稿，返回含**明文 secret** 的 `controlproto.DesiredState`
- `BuildPushable(nodeID)` — 只读已发布快照，同样返回含**明文 secret** 的结构
- `DraftStatus(nodeID)` — 只返回两个 bool

三者都不适合直接喂给 HTTP 响应。**采用新增方法**：

```go
// PublishedView 返回已发布快照的脱敏视图，不解密任何 secret。
func (a *DesiredStateAuthority) PublishedView(nodeID uint) (PublishedSnapshotView, error)
```

关键设计：**直接从 `persistedDesiredSnapshot` 构造，跳过 `decryptSnapshot`。** 好处有三：

1. 明文 secret 从不进入内存，脱敏是结构性保证而非"记得删字段"
2. 不需要 master key，`ErrMasterKeyMissing` 场景下依然可读（对排障有价值）
3. 不触碰 `BuildPushable` 的哈希校验语义，避免读操作产生 push 路径的副作用

代价：无法在读路径上做 hash 完整性校验。这是可接受的——读视图不承担完整性职责，push 路径的校验（`BuildPushable` 的 hash 比对）保持原样。但响应中要带上存储的 `content_hash`，让管理员能与节点上报的 hash 人工对照。

## 3. 后端

### 3.1 响应结构（`pkg/panel/desired.go` 或新文件）

```go
type PublishedSnapshotView struct {
	Published       bool                     `json:"published"`
	Version         int64                    `json:"version"`
	ContentHash     string                   `json:"content_hash"`
	SchemaVersion   int                      `json:"schema_version"`
	RepublishNeeded bool                     `json:"republish_needed"`
	UpdatedBy       string                   `json:"updated_by,omitempty"`
	UpdatedAt       *time.Time               `json:"updated_at,omitempty"`
	Credentials     []PublishedCredentialView `json:"credentials"`
	Buckets         []controlproto.DesiredBucket  `json:"buckets"`
	Webhooks        []controlproto.DesiredWebhook `json:"webhooks"`
	RateLimit       *controlproto.DesiredRateLimit `json:"rate_limit,omitempty"`
}

// PublishedCredentialView 刻意不含 SecretKey 与 SecretKeyCipher 字段。
// 该结构体是脱敏红线的类型级保证：字段不存在，就无法被误序列化。
type PublishedCredentialView struct {
	AccessKey  string `json:"access_key"`
	Name       string `json:"name,omitempty"`
	Bucket     string `json:"bucket,omitempty"`
	Status     string `json:"status"`
	QuotaBytes int64  `json:"quota_bytes"`
}
```

`Buckets` / `Webhooks` / `RateLimit` 直接复用 `controlproto` 类型 —— 已确认这三个结构体不含任何 secret 字段（`pkg/controlproto/desiredstate.go:38-55`）。只有 `DesiredCredential` 有 `SecretKey`（`:30`，无 `omitempty`），所以只有它需要独立的 view 类型。

Webhook URL 按现状原样返回：`PanelNodeWebhooksSection` 已经在明文显示 URL，本任务不引入新暴露面，收敛 URL 中可能内嵌 token 的问题超出范围（如需，另立任务）。

### 3.2 状态机

| 情况 | 响应 | HTTP |
|---|---|---|
| 有已发布快照且 schema 版本受支持 | `published=true`，完整内容 | 200 |
| 无 `desired_configs` 行 | `published=false`，空切片（非 null） | 200 |
| `ErrDesiredSnapshotRepublishRequired` | `published=true, republish_needed=true`，内容为空切片 | 200 |
| 其他 decode 错误 | 同上（fail closed，与 `DraftStatus` 一致的保守处理） | 200 |
| 未知 node id | `not found` | 404 |
| DB 错误 | 泛化错误消息 | 500 |

空态用 200 而非 404：这是"节点存在但尚未发布"的正常业务态，前端不该按错误处理。前端契约里"pending import 404 归一化为 null"（frontend spec）那种反模式不必在这里重演。

切片必须是 `[]T{}` 而非 nil，避免 JSON 里出现 `null` 让前端多写守卫。

### 3.3 路由

`pkg/panel/adminapi.go:544` 的 `desiredStateRoute` 加 GET 分支：

```go
func (a *AdminAPI) desiredStateRoute(w http.ResponseWriter, r *http.Request, id uint, rest []string) {
	if len(rest) == 0 && r.Method == http.MethodGet {
		a.getDesiredState(w, r, id)
		return
	}
	if len(rest) == 0 && r.Method == http.MethodPost { ... }   // 不变
	if len(rest) == 1 && rest[0] == "push" && ... { ... }      // 不变
	writeTransportError(w, http.StatusNotFound, "not found")
}
```

`getDesiredState` 先 `a.loadNode(w, id)` 校验节点存在（与 `publishDesiredState` 一致），再调 `PublishedView`。

### 3.4 审计

**不写审计日志。** 理由：`AuditLog`（`pkg/panel/models.go:139`）是"每个管理动作"的追加记录，现有 GET 端点（`listCredentials`、`listNodes`、`getNode` 等）均不写审计；只读查询写审计会让审计表被读操作淹没，反而降低可审计性。保持与既有 GET 端点一致。

## 4. 前端

### 4.1 `client.ts`

```ts
export interface PanelPublishedCredential {
  access_key: string
  name?: string
  bucket?: string
  status: 'enabled' | 'disabled'
  quota_bytes: number
}

export interface PanelPublishedSnapshot {
  published: boolean
  version: number
  content_hash: string
  schema_version: number
  republish_needed: boolean
  updated_by?: string
  updated_at?: string
  credentials: PanelPublishedCredential[]
  buckets: PanelBucket[]          // 若字段不完全一致则另定类型
  webhooks: PanelWebhook[]        // 同上
  rate_limit?: PanelRateLimitValues
}

// adminApi 新增：
getNodeDesiredState(id: number) {
  return apiFetch<PanelPublishedSnapshot>(`/api/admin/nodes/${id}/desired-state`)
}
```

注意 `PanelBucket` 含 `created_at`、`PanelWebhook` 含 `id` / `node_id` / `created_at`，而快照里没有这些字段。需要为快照单独定义 `PanelPublishedBucket` / `PanelPublishedWebhook`，不要硬套草稿类型——`vue-tsc` 会在这里报错，正好作为类型正确性的门禁。

另外 `controlproto.DesiredWebhook.Events` 是**逗号分隔字符串**（`pkg/controlproto/desiredstate.go:46`），而草稿侧 `PanelWebhook.events` 是 `PanelWebhookEvent[]`。快照视图需要在后端就拆成数组，或在前端拆分并在类型上标明。倾向**后端拆成数组**，与草稿 API 的表现形态一致，减少前端特殊处理。

### 4.2 `PanelNodeConfigSection.vue`

```
props: { nodeId: number; disabled: boolean; refreshKey: number }
无 emits（只读区域）
```

结构（复用既有 class：`panel-detail-section` / `panel-section-heading` / `node-facts` / `panel-effective-state` / `panel-section-table`）：

```
<h2>已发布配置</h2>
<p class="muted">面板最后一次显式发布的权威快照，也是节点重连时会收到的内容。
   不代表节点当前实际生效的配置——面板只能通过版本号与内容哈希判断是否一致。</p>

[未发布空态] → "尚未发布任何配置。资源编辑只保存草稿，需点击「发布草稿」。"
[republish_needed] → warning-notice "快照为旧格式，需重新发布后才能查看与推送。"
[已发布] →
  概览 dl: 版本 / 内容哈希(<code>) / 发布者 / 发布时间
  桶表格: 名称 / ACL           （空态："本次发布未声明任何桶。"）
  凭证表格: Access Key / 名称 / 绑定桶 / 状态 / 配额   （空态文案；表头不出现 Secret 列）
  Webhook 表格: URL / 事件 / 启用   （空态文案）
  限流 dl: RPS / Burst / 转发头信任  （未配置时："未配置，节点使用内置默认值。"）
```

第二段 `muted` 文案是本任务最重要的一行 UI 文本——它是 §1 语义约束的落地。

挂载点：`PanelNodeDetail.vue:74-79` 的 section 列表中，放在 `PanelNodeImportSection` 之后、`PanelNodeBucketsSection` 之前（先看"已发布的是什么"，再看"草稿里在改什么"，符合阅读顺序）。

`refreshKey` 用父组件已有的 `resourceRevision`，发布后自动刷新。

## 5. 测试计划

`pkg/panel/adminapi_test.go` 或新 `desired_view_test.go`：
- 有已发布快照 → 200，字段齐全，version/hash 正确
- **脱敏断言：构造含真实 `SecretKeyCipher` 的快照，断言响应体字符串既不含明文 secret，也不含该密文串**（这是红线的自动化守卫）
- 无发布行 → 200 `published=false`，切片为 `[]` 非 `null`
- 旧 schema 快照 → 200 `republish_needed=true`
- 未知 node id → 404
- PUT/DELETE 等方法 → 与既有约定一致的 404/405
- 既有 publish/push 测试零修改通过

前端：`npm run build`（`vue-tsc --noEmit` + vite build）。

## 6. 风险

- **最大风险是脱敏被绕过。** 防线是类型级的：`PublishedCredentialView` 没有 secret 字段。绝不能为了"省事"直接序列化 `controlproto.DesiredState` 或 `persistedDesiredSnapshot`。测试里的字符串断言是第二道防线。
- **语义误导。** 若 UI 文案说成"节点当前配置"，在 `drift` 状态下会把排障带偏。文案必须按 §4.2 的措辞。
- **`dist/` 未纳入版本控制。** `.gitignore:33` 忽略 `pkg/webadmin/ui/dist/*`（只保留 `.gitkeep`），说明构建产物不入库，由构建流程生成。因此**不需要**提交 `dist/`，但发布流程必须先 `npm run build` 再 `go build`，否则 embed 会拿到空目录。这一点 `webadmin-guidelines.md:86` 已有记载，遵守即可。
