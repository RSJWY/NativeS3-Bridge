# Panel 节点健康仪表盘技术设计

## 1. Architecture

页面复用现有 Vue SPA、认证状态和 Panel 路由；后端在 `pkg/panel/adminapi.go` 增加只读 dashboard handler，并在 `AdminAPI.Routes` 注册。认证由现有 `NewAdminServer` 中的 middleware 统一负责，不新增认证机制。

推荐数据流：

```text
Panel Dashboard.vue
    -> adminApi.dashboardSummary()
    -> GET /api/admin/dashboard/summary
    -> AdminAPI dashboard handler
       -> Node + NodeState + DesiredConfig 查询
       -> Hub.IsOnline / nodeToResponse 语义
    <- summary cards + state distribution + attention nodes
```

一次接口响应应完成前端首屏所需的数据，避免页面对 `/api/admin/nodes/{id}`、credentials、buckets 发起 N+1 请求。

## 2. Response Contract

建议响应结构（字段名可按现有 TypeScript 命名风格落地）：

```json
{
  "totals": {
    "nodes": 3,
    "online": 1,
    "offline": 1,
    "retired": 1,
    "attention": 1
  },
  "health": {
    "synced": 1,
    "waiting": 0,
    "failed": 1,
    "drift": 0,
    "unknown": 0
  },
  "attention_nodes": [
    {
      "id": 2,
      "display_name": "上海-01",
      "status": "active",
      "online": false,
      "applied_version": 4,
      "desired_version": 5,
      "sync_state": "waiting",
      "last_error": "",
      "draft_dirty": false,
      "publish_required": true,
      "region": "cn-shanghai",
      "last_heartbeat": "2026-08-15T08:00:00Z",
      "severity": "offline"
    }
  ],
  "generated_at": "2026-08-15T08:01:00Z"
}
```

### Contract decisions

- `online` 由当前 Hub 连接状态决定，与持久化 `NodeState.Online` 保持现有 `nodeToResponse` 语义一致。
- `attention` 使用后端统一规则计算，不让前端复制业务判断。
- `severity` 是展示排序所需的后端派生字段，枚举建议：`sync_failed`、`drift`、`offline`、`pending`。
- `generated_at` 仅表示本次汇总生成时间，不能当作节点心跳时间。
- `attention_nodes` 第一版返回全部异常节点，节点数量增长后再增加 `limit` / 分页。

## 3. Aggregation Semantics

固定以下口径：

- 生命周期节点：`nodes` 包含 active、disabled、retired；`retired` 单独计数。
- 连接指标：`online` / `offline` 只统计非退役节点；退役节点不应因遗留 Hub 状态被算入在线。
- 健康分布：只统计非退役节点；没有 `NodeState` 的节点进入 `unknown`。
- 需要处理：`failed` / `drift` 最高；其次非退役离线；其次 `waiting`、`draft_dirty`、`publish_required`。
- 同步状态为空但存在已发布目标版本且应用版本不一致时，沿用现有 `nodeToResponse` 的 waiting 语义。
- 排序键：严重性降序；同级按 `last_heartbeat` 升序，空值最后；最后按 node ID 升序保证稳定结果。

实现时应优先复用 `nodeToResponse` 或抽取共享的纯聚合函数，避免 dashboard 与节点列表各自演化出不同的状态判定。

## 4. Frontend Shape

- 新增 Panel Dashboard 视图，沿用 standalone `Dashboard.vue` 的页面容器和状态处理模式，但不复用容量/请求图表逻辑。
- `runtimeState.serviceMode === 'panel'` 时在 `App.vue` 显示仪表盘链接，并把 `serviceHomePath()` 指向该路由。
- 需要关注列表中的“管理”按钮跳转 `/nodes/:id`。
- 第一版不实现复杂筛选；概览卡可以作为静态指标展示，避免点击后产生无效交互。
- 视觉状态必须同时依赖文字和颜色，不能只靠颜色区分健康状态。

## 5. Compatibility / Security

- 新增接口是只读接口，不改变节点协议、数据库迁移或 standalone API。
- 复用现有认证 middleware；响应不得包含 NodeCredential.SecretKeyCipher、访问密钥 secret 或 DesiredConfig.ContentJSON。
- 旧节点没有区域、心跳或 NodeState 时接口仍返回合法空值。
- 当 Hub 不可用时以数据库状态作为基础，但 `online` 应遵守现有实时连接判定，不能伪造在线。

## 6. Deferred Metrics

容量、对象数、请求量和流量需要单独的采集设计：node agent 填充心跳字段、Panel 持久化最新观测值、按时间窗口保存历史统计，并为缺失/旧节点定义兼容语义。本任务不改变这些边界。
