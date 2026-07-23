# Panel 权威配置管理闭环 — 实施计划

## Preconditions

- 用户已审阅并批准本任务最新 `prd.md`、`design.md`、`implement.md`。
- 使用 `task.py start` 激活本 child task 后再修改产品代码。
- 开始实现前加载 `trellis-before-dev`；前端编码同时加载 `uncodixfy`。
- 保留当前 dirty worktree 中与本任务无关的用户改动，不做 reset/restore。

## Implementation Checklist

### 1. Published snapshot correctness (backend foundation)

- [ ] 在 `pkg/panel` 定义 schema-versioned persisted desired snapshot，credential 仅保存 ciphertext。
- [ ] 重构 desired authority：canonical draft build、publish-in-transaction、decode/decrypt exact snapshot、hash verification、legacy republish error。
- [ ] 增加 draft dirty / publish-required 计算并扩展 node response。
- [ ] 修正 transport push，使手动 push 永远读取 exact published snapshot。
- [ ] Tests：发布后修改/删除/轮换 draft 不改变旧 push payload；DB 不含明文；legacy snapshot fail closed；hash 精确匹配。

Rollback point: 此阶段只涉及 Panel snapshot 生成/读取；若失败，回退这些文件即可，node wire schema 尚未变化。

### 2. Panel draft CRUD and validation

- [ ] 增加 node-scoped bucket handlers/store：list/create/ACL/delete，绑定 credential 冲突与审计。
- [ ] 扩展 credential store/API：name/bucket/status/quota update、delete；create/update 复用严格 validation。
- [ ] 增加 webhook handlers/store：URL/events/enabled validation、canonical events、CRUD 与审计。
- [ ] 增加 rate-limit GET/PUT/DELETE：统一默认值、positive validation、upsert/reset 与审计。
- [ ] 将新增 route 加入 `NodeByID` dispatcher，确保 method/404/409/500 错误矩阵稳定。
- [ ] Tests：每个资源的 good/base/bad、跨 node 隔离、unknown fields、secret non-leak、所有写操作 audit。

Rollback point: CRUD 只改 Panel draft，不自动发布；即使 UI 未完成也不会影响现有 node。

### 3. Import atomicity and API behavior

- [ ] 将 already-managed 检查扩展到四类 draft 表 + DesiredConfig。
- [ ] Confirm 使用同一 GORM transaction 完成 adopt + version=1 snapshot；pending 仅在 commit 后清除。
- [ ] 增加并发 request/pending/confirm/abort 的明确错误处理；保持 read-then-confirm 红线。
- [ ] Tests：任一步失败完整 rollback；仅 bucket/webhook/rate-limit/desired 存在也返回 conflict；summary 无 secret。

Rollback point: pending import 保持内存态；失败时 node 与既有 Panel 权威表都不变。

### 4. Node full apply and runtime state

- [ ] 在 `pkg/nodeagent` 增加 additive managed rate-limit persistence/migration/validation。
- [ ] 增加 dynamic rate-limit controller，保留现有 fixed `AnonRateLimit` API 供 standalone 使用。
- [ ] 为 hooks manager 增加 prepared config 的原子 replace；启动 `Reload` 行为不变。
- [ ] 扩展 executor dependencies：credential invalidator、bucket store/runtime、hook replacer、rate-limit updater。
- [ ] 将 payload hash validation、四类资源 reconcile、AgentMeta version/hash 合并到一次 apply transaction。
- [ ] commit 后执行 infallible runtime swap/cache invalidation，并确保 LocalState/ContentHash 包含 rate limit。
- [ ] Tests：rollback、used_bytes preservation、credential cache、ACL cache、hook live set、rate-limit runtime/persistence/hash。

Rollback point: node DB 只增加 Agent-owned schema；旧 node binary 可忽略。

### 5. Managed bucket data-plane boundary

- [ ] 增加 managed-only server/router option；standalone constructors 默认保持现状。
- [ ] managed ListBuckets/Head/request guard 以 bucket metadata 为权威。
- [ ] managed S3 CreateBucket/DeleteBucket 明确拒绝；Panel desired apply 是唯一声明入口。
- [ ] apply create 确保空目录；delete 只删 metadata、保留磁盘对象并 invalidate cache。
- [ ] 新声明同名非空 retained directory 时拒绝 apply，不输出对象名。
- [ ] Tests：删除后目录/对象仍在但 S3 不可列出/访问；ACL 立即变化；standalone 回归不变。

Rollback point: managed behavior 只由 `cmd/node` opt in，不影响 standalone。

### 6. Capability gate and reconnect reconcile

- [ ] 为 `HelloPayload` 增加 optional capabilities，定义 `authoritative_config_v1` 常量。
- [ ] 新 node 在 hello 上报 capability；Panel connection 保存 peer capability。
- [ ] 新 Panel 对旧 node 的 config push 返回可操作升级错误，不发送/不假报 synced。
- [ ] 连接进入 Hub 后按 hello version/hash 自动推送 exact snapshot；失败更新 observed state。
- [ ] Tests：new/new auto sync、new Panel/old node gate、old-style payload decode compatibility、无 desired config 不推送。

Rollback point: optional wire field 可被旧 peer 忽略，无数据库回滚。

### 7. Typed frontend client and error model

- [ ] 扩展 `client.ts`：ApiError(status)、bucket/credential/webhook/rate-limit/import types 与 methods。
- [ ] pending import 404 在 client/component 边界规范化为空状态；401 redirect 行为不变。
- [ ] 保证所有路径参数 encode，Secret 只存在 create/rotate result type。
- [ ] 运行前端 type-check/build。

### 8. Panel node detail UI

- [ ] 拆分 import/buckets/credentials/webhooks/rate-limit 子组件，父页面保留 lifecycle/version/publish。
- [ ] 增加 draft dirty / publish-required banner，明确“发布草稿”与“重推已发布版本”。
- [ ] Bucket：创建、ACL、删除确认与数据保留说明。
- [ ] Credential：bucket select、create/edit、enable/disable、delete、rotate one-time secret。
- [ ] Webhook：URL、事件多选、enabled、edit/delete。
- [ ] Rate limit：effective/default、RPS/burst、trust-forwarded warning、save/reset。
- [ ] Import：request/loading/review/confirm/abort、无 secret 摘要、offline/already-managed/no-pending/timeout 文案。
- [ ] 复用现有 CSS state/table/button patterns，desktop/mobile 均可操作。

Rollback point: UI components 与 client methods 可独立回退；后端 API 保持向后兼容。

### 9. Integrated verification

- [ ] `gofmt` 所有变更 Go 文件。
- [ ] Focused: `go test ./pkg/panel ./pkg/nodeagent ./pkg/server ./pkg/hooks ./pkg/controlproto`。
- [ ] Frontend: `npm ci`（lock 未变可复用已安装依赖）与 `npm run build` in `pkg/webadmin/ui`。
- [ ] Full: `go test ./...`, `go vet ./...`, `go build ./...`。
- [ ] Real browser smoke：Panel login → node detail → draft CRUD → publish；检查无 standalone API、无 >=400 意外请求、secret 关闭后不再出现。
- [ ] Real Panel+node smoke：reconnect auto sync、ACL anonymous behavior、credential disable、webhook delivery、rate-limit SlowDown、bucket logical delete/data preservation。
- [ ] Import browser/control-plane smoke：request → summary → abort；再次 request → confirm；确认前 node/Panel authoritative rows不变。

## Risky Files / Review Focus

- `pkg/panel/desired.go`: plaintext/ciphertext boundary、draft/published separation、legacy rows。
- `pkg/panel/migration.go`: transaction boundary 与 pending lifecycle。
- `pkg/nodeagent/executor.go`: DB transaction、AgentMeta、runtime swap consistency。
- `pkg/server/router.go` / `pkg/storage/bucketmeta.go`: managed vs standalone 行为隔离、对象保留边界。
- `pkg/webadmin/ui/src/api/client.ts`: 401 redirect 与 ApiError status 不冲突。
- `pkg/webadmin/ui/src/views/PanelNodeDetail.vue` and new sections: one-time secret/token lifecycle、并发 loading state。

## Final Review Gates

- [ ] 每条 PRD acceptance criterion 都有自动测试或明确 browser/E2E evidence。
- [ ] `git diff` 不包含 `dist/assets/*`、临时 DB、证书、token、secret 或浏览器输出。
- [ ] No plaintext Secret in Panel persistence/log/audit fixtures。
- [ ] No unrelated dirty-worktree changes overwritten。
- [ ] 执行 `trellis-check`，处理所有 verified critical/warning findings。
- [ ] 完成后运行 `trellis-update-spec`，更新 backend/frontend contracts，再按 Trellis finish/commit 流程收尾。
