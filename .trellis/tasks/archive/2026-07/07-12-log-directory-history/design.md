# Design: 日志目录化与历史文件管理

## Architecture

沿用现有 slog + lumberjack + ring 架构，不更换日志后端：

```text
config.LogConfig
  ├─ file (legacy, optional)
  └─ dir  (new, optional; mutually exclusive with file)
          ↓ effectiveLogPath
setupSlog → stdout + lumberjack(active file) + in-memory ring
          ↓ effective file/dir
webadmin Logs API → enumerate allowed files → select/tail/decompress → filter
          ↓
Vue Logs page → file selector + existing level/query/limit controls
```

## Configuration Contract

`LogConfig` 新增 `Dir string`。有效路径规则：

- `dir != "" && file == ""`：`filepath.Join(dir, "natives3bridge.log")`
- `file != "" && dir == ""`：保持原完整路径
- 两者都空：不落盘
- 两者都非空：`Validate` 返回错误

提供集中 helper 计算 effective file，避免 `main` 与 `webadmin` 分别拼路径。目录仍在 `setupSlog` 中创建；不可写或父路径不是目录时保持启动失败。

## File Discovery Contract

从 effective active file 推导日志目录和 active basename，只允许：

- active basename
- lumberjack 对该 basename 生成的轮转文件
- 上述轮转文件的 `.gz` 形式

API 文件项建议包含：

```go
type logFileInfo struct {
    ID         string    `json:"id"`
    Name       string    `json:"name"`
    Size       int64     `json:"size"`
    ModifiedAt time.Time `json:"modified_at"`
    Current    bool      `json:"current"`
    Compressed bool      `json:"compressed"`
}
```

`ID` 可使用 basename，但每次读取必须重新枚举并匹配，不能直接拼接用户输入后打开。只接受普通文件；使用 `Lstat`/`EvalSymlinks` 或等价校验确保目标位于日志目录且不是 symlink 逃逸。轮转清理与选择请求的竞态返回明确错误。

## API Contract

扩展 `GET /api/admin/logs`：

```text
query: limit, level, q, file(optional id)
response: existing fields + files + selected_file
```

- `file` 为空：选择 current active file；读取失败可回退 ring，保持兼容。
- `file` 非空：必须命中 allowed file list；不存在或非法返回 400/404，不回退 ring。
- `.gz` 使用 `gzip.NewReader`，普通文件直接读取；二者进入同一 scanner/parser/filter。
- 文件日志未启用：`files=[]`、`selected_file` 为空，继续返回 ring。

## Frontend Contract

`LogsResponse` 增加 `files` 与 `selected_file`，`adminApi.logs` 增加可选 `file`。`Logs.vue`：

- 文件选择器置于现有 toolbar 首位；当前文件显示“当前日志”，历史文件显示名称、格式化大小及压缩标记。
- 默认使用响应中的 current file；切换选择后调用现有 `load`，保留 level/query/limit。
- 若列表变化导致选择失效，显示服务端错误并允许用户切回当前文件，不自动展示不同文件内容。
- 沿用现有表单、notice、loading/error 样式，不引入新的视觉体系。

## Security

- API 不接收目录或完整路径，只接收服务端返回的 file id。
- 不列出不匹配 active basename 的其他文件，避免暴露 state 目录内容。
- 不跟随目录外 symlink；不提供下载原文件接口。
- 解析日志继续调用 `sensitiveLogKey`，不得因历史文件支持放宽敏感字段过滤。

## Compatibility And Rollback

- `log.file` 保留，现有配置无需迁移；README 推荐新部署使用 `log.dir`。
- API 仅增加字段和可选参数，旧前端/客户端继续读取当前文件。
- 回滚时使用旧 `log.file` 配置即可；目录内现有日志文件不需迁移或删除。

## Tradeoffs

- 继续使用 lumberjack，避免引入按日切割器；“目录化”是配置和管理视角升级，不改变轮转触发机制。
- 历史文件读取沿用顺序扫描，简单且与现有行为一致；超大日志的反向 tail/索引优化留待后续。
