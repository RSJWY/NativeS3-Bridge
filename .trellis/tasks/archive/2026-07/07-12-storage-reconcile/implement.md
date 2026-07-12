# Implement: 管理端存储对账

> 用户要求：本阶段**只规划、不执行**。下列清单供后续 `task.py start` 后按序实施。

## 前置

- [ ] 用户确认 prd/design 无阻塞分歧
- [ ] `python ./.trellis/scripts/task.py start`（**届时再做**；现在不要 start）
- [ ] 实现时再读最新：`pkg/webadmin/api.go`、`pkg/storage/file_backend.go`、绑桶字段

## 执行清单

### 1. 存储扫盘核心

- [ ] 在 `pkg/storage`（推荐）实现 `ReconcileBucket` / scan 报告结构
  - 路径解析复用 `ResolveBucketPath`
  - 对象过滤与 `ListObjects` 一致
  - 输出：object_count、scanned_bytes、orphan 相对路径列表
- [ ] 单测：正常对象、sidecar 不计对象、孤儿检测、非法桶名、空桶

### 2. 管理 API

- [ ] 扩展 `webadmin.API` 依赖（root/suffix 或 reconciler + invalidator）
- [ ] `POST /api/admin/buckets/{name}/reconcile` body `{"apply":bool}`
- [ ] dry-run：只报告
- [ ] apply：重扫 → 删孤儿 sidecar → 更新绑定密钥 used_bytes → Invalidate
- [ ] 路由挂入 admin mux + Auth.Middleware
- [ ] API 测试：401 / 404 / dry-run 不变 / apply 副作用

### 3. 前端

- [ ] `client.ts`：`reconcileBucket(name, apply)`
- [ ] `Buckets.vue`：对账按钮、报告展示、二次确认 apply
- [ ] 中文风险文案：不恢复已删文件；会改 used_bytes；会删孤儿 sidecar

### 4. 接线

- [ ] `webadmin.NewServer` / main 注入 data_root 与 metadata_suffix
- [ ] 确认 CredentialStore invalidator 在 used_bytes 回写后被调用

### 5. 文档与 spec

- [ ] README 管理后台小节
- [ ] `webadmin-guidelines.md`、`auth-quota-guidelines.md` 契约补丁
- [ ] 必要时 `storage-guidelines.md` 孤儿定义

### 6. 验证命令（实现后）

```bash
go test ./pkg/storage/ ./pkg/webadmin/ -count=1
# 手动：管理端登录 → 桶页 dry-run → apply → 查密钥 used_bytes
```

## 回滚点

- API 未发布前：整特性可回退提交
- 若 apply 已在生产误用：used_bytes 可再次对账校正；误删的仅为孤儿 sidecar（对象本体设计上不删）

## 分期建议（若时间紧）

1. **MVP**：storage + API + 测试（无 UI，curl 可用）  
2. **完整**：+ UI + README  

prd 默认含 UI；若砍范围须先改 prd 验收项再实现。

## 完成定义

- prd Acceptance Criteria 全部勾选  
- 测试通过  
- spec 已更新  
- **再**走 Phase 3 提交（实现阶段，非现在）
