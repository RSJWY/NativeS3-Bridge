# Implement: 日志落盘与管理端日志查看

> 用户要求：本阶段**只规划、不执行**。下列清单供后续 `task.py start` 后使用。

## 前置

- [ ] 用户确认 prd/design
- [ ] `python ./.trellis/scripts/task.py start`（**现在不要**）
- [ ] 与 `07-12-storage-reconcile` 分开实现/提交

## 执行清单

### 1. 配置

- [ ] `pkg/config`：`LogConfig` + 默认值 + Validate
- [ ] 更新 `configs/config.example.yaml`、`config.docker.example.yaml` 等示例
- [ ] config 单测

### 2. logging 包

- [ ] `pkg/logging/ring.go`：Ring + Entry + Snapshot + 敏感 key 过滤
- [ ] `pkg/logging/handler.go`：Tee 到底层 handler + ring
- [ ] ring 单测（并发、limit、过滤）

### 3. 落盘接线

- [ ] `go get gopkg.in/natefinch/lumberjack.v2`
- [ ] 改造 `setupSlog`：MultiWriter(stdout, lumberjack?) + ring handler
- [ ] `log.file` 打开/建目录失败 → 返回 error → main 退出
- [ ] 可选：小 MaxSize 轮转集成测（TempDir）

### 4. 管理 API

- [ ] API 注入 ring + logFile
- [ ] `GET /api/admin/logs` + 路由注册（Auth.Middleware）
- [ ] tail 文件辅助函数（只读、限行）
- [ ] API 测试 401 / 200 / limit

### 5. 前端

- [ ] `client.ts` logs API
- [ ] `Logs.vue` + router + App 侧栏
- [ ] 中文空态与 file_enabled 提示
- [ ] `npm run build`（实现阶段）

### 6. 文档与 spec

- [ ] README 日志章节
- [ ] `webadmin-guidelines.md` 补契约
- [ ] Docker 说明：state 卷日志路径

### 7. 验证（实现后）

```bash
go test ./pkg/config/ ./pkg/logging/ ./pkg/webadmin/ -count=1
go build -o natives3bridge ./cmd/natives3bridge
# 配置 log.file 到临时路径，登录后 GET /api/admin/logs
```

## 回滚

- `log.file=""` 即关闭落盘；整特性可 revert。
- 落盘不影响对象数据。

## 完成定义

- prd Acceptance Criteria 全勾  
- 测试通过  
- spec/README 更新  
- 再 Phase 3 提交（非现在）
