# Panel 节点健康仪表盘执行与验收计划

本文件只描述后续实现顺序和验收逻辑；当前任务不执行产品代码修改。

## Implementation Checklist

1. 按 PRD 已锁定的接口统计口径建立测试 fixture：退役节点单独计数，在线/离线排除退役节点，attention 返回全部。
2. 在 Panel 后端增加 summary response 类型、聚合函数和 `GET /api/admin/dashboard/summary` 路由。
3. 复用现有 `nodeToResponse` 的 online、sync、desired/applied version 语义，避免重复实现。
4. 为后端聚合逻辑补充表驱动单元测试：全健康、离线、失败/漂移、待发布、无 NodeState、退役节点、并列排序和空库。
5. 在前端 API client 增加响应类型和请求方法。
6. 增加 Panel Dashboard 页面、加载/错误/空状态、概览卡、健康分布和需要关注列表。
7. 更新 Panel 模式导航和默认首页分流；验证 standalone 模式不受影响。
8. 增加前端组件测试或浏览器验收：路由入口、四张指标卡、排序、缺失字段、刷新防重复和 API 失败保留旧数据。
9. 运行后端测试、前端构建和现有 Panel/SPA 集成检查。

## Acceptance Logic

### Summary consistency

- 从 fixture 节点逐条计算 `nonRetiredOnline`、`nonRetiredOffline`、`retired`、`attention`，逐字段对比 API totals。
- `attention` 不能仅由 `sync_state` 推导；必须覆盖 `draft_dirty` 和 `publish_required`。
- 同一节点只能在 totals 的一个连接分类中出现，在 health 分布中只能出现一个同步分类。

### Severity and ordering

- 构造至少四个节点：failed、drift、offline、waiting/publish_required；响应顺序必须严格符合约定。
- 两个节点无心跳时，按 node ID 升序；有心跳时旧心跳优先，验证排序稳定且不依赖数据库返回顺序。

### Lifecycle semantics

- retired 节点不计入 online/offline/health，但计入总节点和 retired；即使 Hub 模拟在线，也不得成为健康在线节点。
- disabled 节点仍按实际连接状态计入 online/offline，但不因生命周期 disabled 自动被标记为同步异常。

### Missing and failure states

- 没有 NodeState 的节点返回 `unknown`，`last_heartbeat` 和 `region` 使用空值，前端展示占位符。
- API 500 或网络错误时，首次加载显示错误状态；已有数据刷新失败时保留旧数据并显示错误提示。
- 空数据库返回 0 统计和明确空状态，不出现 NaN、负数或异常图表。

### Routing and compatibility

- Panel `auth-settings.service_mode = panel`：登录后默认进入 Panel Dashboard，导航包含“仪表盘”。
- Standalone：默认仍进入原 `/dashboard`，导航仍包含密钥管理、桶管理和日志；不请求 Panel summary endpoint。
- 未认证 summary 请求为 401，POST/PUT 等非 GET 为 405。

## Validation Commands

后续实现完成后至少运行：

```bash
go test ./pkg/panel ./pkg/controlproto ./pkg/nodeagent
npm --prefix pkg/webadmin/ui run build
go test ./...
```

若仓库已有浏览器脚本，应补充 Panel/standalone 两种 service mode 的页面验收；若没有，则使用现有 `scripts/internal/e2e-browser.py` 或等价浏览器检查覆盖上述路由和状态矩阵。

## Risk / Rollback Points

- 风险：直接复制 `nodeToResponse` 逻辑会造成列表页和仪表盘状态不一致；验收必须以共享规则或同一组 fixture 对比。
- 风险：把 heartbeat 协议预留字段误当作可用容量指标；接口契约和 UI 不得出现这些字段。
- 回滚：后端路由、前端 Panel 入口和页面可独立回退，不涉及数据库迁移或节点配置格式变更。
