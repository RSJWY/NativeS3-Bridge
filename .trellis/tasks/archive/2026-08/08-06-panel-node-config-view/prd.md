# 面板新增节点配置查看区域

来源：GitHub issue #1 — "增加面板直接查看节点配置的区域"（enhancement）

## 背景

issue 正文只有一句话，需求需要基于现状推断并明确边界。

现状：节点详情页（`pkg/webadmin/ui/src/views/PanelNodeDetail.vue`）已有六个分资源区域（导入、桶、凭证、Webhook、限流、日志），管理员只能逐块查看，**没有任何一处能看到该节点配置的整体面貌**。

后端更是完全没有读端点：
- `nodeResponse`（`pkg/panel/adminapi.go:64-77`）只有 `applied_version` / `desired_version` / `sync_state` / `draft_dirty` 等元数据，无配置内容。
- `desiredStateRoute`（`pkg/panel/adminapi.go:544`）只接受 `POST`（发布）与 `POST /push`（重推），没有 `GET`。
- `DesiredConfig.ContentJSON` 从未被序列化进任何 HTTP 响应。

## 关键语义约束（必须在 UI 上如实呈现）

**面板只知道自己发布了什么，不知道节点上真正在跑什么。** 节点的 apply ACK 只回传 version 与 content hash，不回传配置内容本身（见 `.trellis/spec/backend/panel-authoritative-config-guidelines.md` 的 "Delivery, reconnect, and observed state" 段）。

因此本区域**只能**标注为"已发布配置快照"与"当前草稿"，**不得**声称是"节点当前生效配置"。若把已发布快照说成节点实况，在 `sync_state` 为 `waiting` / `failed` / `drift` 时会直接误导排障。

## Requirements

### R1 新增只读快照查询端点

- `GET /api/admin/nodes/{id}/desired-state` 返回**已发布**快照的脱敏视图。
- 复用现有 `desiredStateRoute` 分派（`pkg/panel/adminapi.go:544`），新增 `GET` 分支，不改动既有 `POST` 行为。
- 无已发布快照（`gorm.ErrRecordNotFound`）→ 明确的空态响应，不是 500。
- 旧格式快照（`ErrDesiredSnapshotRepublishRequired`）→ 明确标记"需重新发布"，不是 500，且不得尝试用草稿回填。
- 响应包含 `version`、`content_hash`、以及桶/凭证/Webhook/限流的脱敏内容。

### R2 secret 零暴露（红线）

- 响应中**不得**出现 `secret_key`、`secret_key_cipher`，也不得返回原始 `ContentJSON`。
- 不得直接把 `controlproto.DesiredState` 当响应类型序列化——它的 `SecretKey` 字段没有 `omitempty`（`pkg/controlproto/desiredstate.go:34` 附近），会把明文 secret 写进 HTTP 响应。必须定义独立的脱敏响应结构体。
- 现有契约：`secret_key` 仅由创建与轮转返回（spec `panel-authoritative-config-guidelines.md` Contracts）。本任务不得新增任何暴露面。
- 凭证条目只返回 `access_key`、`name`、`bucket`、`status`、`quota_bytes`。

### R3 草稿与已发布的差异可见

- 区域需同时呈现"已发布版本"与"草稿是否有未发布变更"，复用已有的 `draft_dirty` / `publish_required`（`nodeResponse` 已提供，无需新增计算）。
- 明确提示：草稿变更需发布后才会下发到节点。

### R4 前端区域

- 新增 `pkg/webadmin/ui/src/components/panel/PanelNodeConfigSection.vue`，挂入 `PanelNodeDetail.vue`。
- 遵循既有 section 组件契约：props `{ nodeId, disabled, refreshKey }`，通过 `adminApi` 调用，`watch(() => props.refreshKey)` 触发重载（参照 `PanelNodeRateLimitSection.vue`）。
- 本区域是只读的，不需要 `changed` 事件。
- `pkg/webadmin/ui/src/api/client.ts` 新增类型与 `getNodeDesiredState(id)` 方法，命名与既有 `PanelXxx` 风格一致。
- 文案与既有区域一致：中文、`muted` 提示语、加载中与错误态。

### R5 可读呈现

- 配置以结构化表格/描述列表呈现，而非裸 JSON 倾泻。
- 空态（无桶/无凭证/无 Webhook/限流未配置）需有明确文案，不是空白。
- 长值（access key、content hash）用 `<code>` 并考虑换行，参照证书表格对 `fingerprint-code` 的处理。

### R6 权限与审计

- 端点走与其他 admin 端点相同的鉴权包装（`Routes` 的 `wrap`，`pkg/panel/adminapi.go:56-59`）。
- 只读查询是否写审计日志：由 design.md 决定并说明理由（倾向不写，避免审计表被读操作淹没；但需确认既有其他 GET 端点的一致做法）。

## Acceptance Criteria

- [ ] `GET /api/admin/nodes/{id}/desired-state` 返回已发布快照的脱敏视图，含 version 与 content_hash
- [ ] 响应 JSON 中 grep 不到 `secret_key`、`secret_key_cipher`，也没有任何明文 secret
- [ ] 有专门的测试断言脱敏（构造含真实密文的快照，断言响应体不含明文与密文）
- [ ] 无已发布快照时返回明确空态（非 500）
- [ ] 旧格式快照返回"需重新发布"标记（非 500），且不回填草稿
- [ ] 未知 node id → 404
- [ ] 非 GET/POST 方法 → 405 或既有的 404 约定，与同文件其他路由一致
- [ ] 既有 `POST /desired-state` 与 `POST /desired-state/push` 行为零变化，现有测试全部通过
- [ ] 前端新区域在节点详情页正常渲染，含加载中、错误、各类空态
- [ ] UI 文案明确写的是"已发布配置"，没有任何地方声称是"节点当前生效配置"
- [ ] `npm ci && npm run build`（含 `vue-tsc --noEmit`）通过
- [ ] `go build ./...`、`go vet ./...`、`go test -count=1 ./...` 全绿
- [ ] `git diff --check` 无输出
- [ ] `.trellis/spec/backend/panel-authoritative-config-guidelines.md` 与 `.trellis/spec/frontend/webadmin-ui-guidelines.md` 增补该端点与区域的契约（含脱敏红线与"不代表节点实况"的语义）

## Notes

- 实现代码由其他 AI 落盘。
- 排在 #2、#3 之后，因为涉及前端构建产物 `pkg/webadmin/ui/dist/`（被 Go embed），最后做可减少与其他任务的 diff 冲突。
- 需确认 `dist/` 是否纳入版本控制；若是，构建产物需一并提交，否则 Go embed 会拿到旧资源。
