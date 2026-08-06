# Implement — 面板节点配置查看区域

## 前置

排在 `08-06-fix-aws-chunked-upload` 与 `08-06-implement-sigv2-auth` 之后。本任务与它们无代码交集（纯 panel + 前端），顺序只为减少 diff 冲突。

## 落地顺序

### S1 后端脱敏视图

1. `pkg/panel/desired.go`（或新建 `desired_view.go`）新增
   - `PublishedSnapshotView`、`PublishedCredentialView`、`PublishedBucketView`、`PublishedWebhookView`
   - `func (a *DesiredStateAuthority) PublishedView(nodeID uint) (PublishedSnapshotView, error)`
2. **实现要点：从 `persistedDesiredSnapshot` 直接构造，不调用 `decryptSnapshot`。** 明文 secret 全程不进内存
3. Webhook 的 `Events` 从逗号分隔字符串拆成 `[]string`，与草稿 API 形态一致
4. 所有切片初始化为 `[]T{}`，不留 nil
5. `ErrDesiredSnapshotRepublishRequired` 与其他 decode 错误 → `RepublishNeeded=true` + 空内容，不返回 error
6. 门禁：`go build ./pkg/panel/`

### S2 路由与处理器

1. `pkg/panel/adminapi.go:544` `desiredStateRoute` 增加 `len(rest)==0 && GET` 分支
2. 新增 `getDesiredState`：先 `a.loadNode(w, id)`，再 `a.desired.PublishedView(id)`，`writeTransportJSON` 输出
3. **不写审计日志**（与既有 GET 端点一致，理由见 design.md §3.4）
4. 既有 `POST` 与 `POST /push` 分支一字不改
5. 门禁：`go build ./... && go test -count=1 ./pkg/panel/`

### S3 后端测试

1. 有快照 / 无快照 / 旧 schema / 未知 node / 非法方法 五个用例
2. **脱敏断言**：构造含真实 `SecretKeyCipher` 的 `NodeCredential` 并发布，断言响应体字符串不含明文 secret 且不含密文串
3. 确认既有 `adminapi_test.go` / `adminresources_test.go` 的 publish/push 用例零修改通过
4. 门禁：`go test -count=1 ./pkg/panel/`

### S4 前端类型与方法

1. `pkg/webadmin/ui/src/api/client.ts`
   - `PanelPublishedSnapshot`、`PanelPublishedCredential`、`PanelPublishedBucket`、`PanelPublishedWebhook`
   - **不要复用 `PanelBucket` / `PanelWebhook`**：它们含 `id` / `created_at`，快照没有这些字段，`vue-tsc` 会报错
   - `adminApi.getNodeDesiredState(id)`
2. 门禁：`cd pkg/webadmin/ui && npx vue-tsc --noEmit`

### S5 前端区域组件

1. 新建 `pkg/webadmin/ui/src/components/panel/PanelNodeConfigSection.vue`
   - props `{ nodeId: number; disabled: boolean; refreshKey: number }`，无 emits
   - `onMounted` + `watch(() => props.refreshKey)` 加载，参照 `PanelNodeRateLimitSection.vue`
   - 加载中 / 错误 / 未发布 / 需重发布 / 各子项空态，五种状态都要有文案
   - 复用既有 class：`panel panel-detail-section`、`panel-section-heading`、`node-facts`、`panel-section-table`、`table-scroll`、`data-table`、`notice warning-notice panel-inline-notice`、`muted`
   - **凭证表格不得有 Secret 列**
2. `PanelNodeDetail.vue` 在 `PanelNodeImportSection` 之后挂入，传 `:refresh-key="resourceRevision"`
3. **文案红线**：区域说明必须写明"不代表节点当前实际生效的配置"。原文见 design.md §4.2
4. 门禁：`cd pkg/webadmin/ui && npm ci && npm run build`

### S6 spec

1. `.trellis/spec/backend/panel-authoritative-config-guidelines.md`
   - Signatures 增补 `GET /api/admin/nodes/{id}/desired-state` 与 `PublishedView`
   - Contracts 增补：读视图不解密、类型级无 secret 字段、不做 hash 校验且理由、空态用 200
   - 错误矩阵增补四种状态
   - Bad case 增补：直接序列化 `controlproto.DesiredState`（`SecretKey` 无 `omitempty`）会泄露明文
2. `.trellis/spec/frontend/webadmin-ui-guidelines.md`
   - "Panel Node Authoritative Configuration UI" 场景的组件清单加入 `PanelNodeConfigSection.vue`
   - 客户端方法清单加入 `getNodeDesiredState`
   - Contracts 增补："已发布配置"区域只读，且必须声明不代表节点实况
3. 门禁：`go build ./... && go vet ./... && go test -count=1 ./... && git diff --check`

## 不要做

- 不新增草稿聚合端点（草稿已被六个分资源区域覆盖）
- 不做内容级 diff（面板拿不到节点实况，做不出真 diff）
- 不返回原始 `ContentJSON`
- 不直接序列化 `controlproto.DesiredState` 或 `persistedDesiredSnapshot`
- 不解密 secret（`PublishedView` 不该需要 master key）
- 不提交 `pkg/webadmin/ui/dist/`（`.gitignore:33` 已忽略，构建时生成）
- 不改 publish / push 逻辑

## 完成判据

`prd.md` 的 Acceptance Criteria 全部勾选。特别地：脱敏测试通过，UI 文案不声称"节点当前生效配置"，`npm run build` 与 `go test -count=1 ./...` 全绿。
