# 日志目录化与历史文件管理

## Goal

将现有“配置单个日志文件、管理页只读当前文件”的能力升级为目录化日志管理：服务可通过 `log.dir` 管理当前及轮转历史日志，管理后台能列出并安全选择查看历史文件。

## Background

- 当前 `log.file` 指向一个完整路径；lumberjack 实际会在父目录生成历史备份，但目录和历史文件没有成为显式管理对象。
- `/api/admin/logs` 只 tail 当前 `log.file`，无法枚举或选择 lumberjack 生成的历史 `.log` / `.log.gz` 文件。
- 日志页只有级别、搜索和条数过滤器，没有文件选择器。

## Requirements

1. **目录化配置**
   - 新增 `log.dir`，启用时当前日志固定写入 `<dir>/natives3bridge.log`。
   - 保留现有 `log.file` 兼容已有部署；`log.dir` 与 `log.file` 同时设置时配置校验失败，避免路径优先级歧义。
   - 无 `log.dir`/`log.file` 时继续只写 stdout + 内存 ring。
   - 现有 `max_size_mb`、`max_backups`、`max_age_days`、`compress` 继续由 lumberjack 管理。

2. **安全的日志文件枚举与读取**
   - 管理 API 仅列出有效日志目录中、属于当前日志 basename 的当前/轮转文件，包括可选 `.gz` 历史文件。
   - 文件选择只能使用服务端枚举出的 basename/opaque id；拒绝绝对路径、`..`、路径分隔符、符号链接逃逸和不匹配文件，禁止任意文件读取。
   - 历史 gzip 文件由服务端透明解压后复用现有 level/query/limit 过滤。
   - 当前文件不可读时保留现有 ring fallback；显式选择历史文件失败时返回明确 4xx/5xx，不静默改读其他文件。

3. **向后兼容的管理 API**
   - 扩展 `GET /api/admin/logs`：可选 `file` 参数选择日志文件；响应新增文件列表和当前选择信息。
   - 未传 `file` 时行为保持为读取当前日志，原有 `source`、`file_enabled`、`limit`、`entries`、`warning` 字段继续可用。
   - 文件列表至少返回稳定 id/name、大小、修改时间、是否当前文件、是否压缩；按当前文件优先、历史文件由新到旧排序。

4. **管理后台历史文件选择**
   - 日志页新增文件选择器，显示“当前日志”及历史文件的时间、大小和压缩状态。
   - 切换文件后使用同一套级别、搜索和条数过滤器重新加载，不清空用户已选过滤条件。
   - 加载失败、文件已被轮转清理、未启用文件日志时显示明确状态；页面仍可查看 ring fallback。

5. **文档与测试**
   - README 配置示例改用 `log.dir`，说明 `log.file` 兼容策略、互斥规则、轮转文件与管理页历史查看能力。
   - 增加配置、日志初始化、文件枚举/路径安全、gzip 读取、API 兼容、前端类型检查和构建验证。

## Confirmed Facts

- `setupSlog` 已使用 lumberjack，并在启动时创建 `log.file` 的父目录；轮转参数无需重做。
- `pkg/webadmin/logs.go` 已有文本日志解析、敏感字段过滤、level/query/limit 逻辑，可复用于选定文件。
- 当前管理 API 通过 session middleware 保护；新增文件列表和选择继续使用同一 `/api/admin/logs` 鉴权边界。
- 前端为 Vue 3 + TypeScript + Vite，日志页位于 `pkg/webadmin/ui/src/views/Logs.vue`，API 类型集中在 `src/api/client.ts`。

## Out of Scope

- 修改 S3 access/auth 日志字段（由 `.trellis/tasks/07-12-s3-auth-access-logging` 负责）
- 在线删除、下载、重命名日志文件
- 修改 lumberjack 为按日切割器或引入新的日志数据库/索引服务
- 跨机器聚合、全文索引、ELK/Loki 接入

## Acceptance Criteria

- [ ] `log.dir: /state/logs` 创建并写入 `/state/logs/natives3bridge.log`，轮转参数保持有效
- [ ] 仅配置旧 `log.file` 的部署继续正常写入，并同样可浏览其匹配的历史轮转文件
- [ ] 同时配置 `log.dir` 与 `log.file` 时启动前校验失败并给出明确错误
- [ ] `/api/admin/logs` 不带 `file` 时保持现有响应语义，并额外返回可选日志文件列表
- [ ] 选择普通历史 `.log` 或压缩 `.log.gz` 时可正确应用 level/query/limit 过滤
- [ ] 路径穿越、绝对路径、非匹配文件和 symlink 逃逸均不能被 API 读取
- [ ] 日志页可选择当前/历史文件，切换后保留过滤条件并显示所选文件的日志
- [ ] 历史文件被轮转删除或读取失败时页面显示明确错误，不误显示其他文件内容
- [ ] `go test` 相关包通过，前端 `npm run build` 通过且嵌入产物按项目流程更新

## Notes

- 本任务与 S3 auth/access logging 无代码依赖，可在前一任务完成后独立实施和验收。
