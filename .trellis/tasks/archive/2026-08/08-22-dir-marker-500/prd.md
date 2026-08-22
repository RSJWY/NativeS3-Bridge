# 修复目录占位 marker 导致子级写入 500

## Goal

修复 S3 客户端创建空目录后无法向该目录上传子对象的问题。`PutObject "dir/"` 必须建立目录语义，使后续 `PutObject "dir/file"` 成功；普通对象 `dir` 与目录前缀 `dir/` 在本任务中互斥，冲突必须返回明确的 S3 冲突错误而不是 500。

## Background / Evidence

当前 `pkg/storage/file_backend.go:63-136` 通过 `ResolveObjectPath` 清理 trailing slash，再把 marker 写成同名普通文件。随后 `PutObject "dir/file"` 在 `:72` 调用 `os.MkdirAll` 时撞上该文件并返回内部错误。`HeadObject`、`GetObject`、`DeleteObject` 和 `ListObjects` 也只识别普通文件，无法表达目录 marker。

触发客户端包括 AWS CLI `s3 cp/sync` 空目录、s3fs、rclone 和 S3 文件管理器的新建文件夹操作。OpenList 使用 `dir/.openlist`，通常不触发此问题。

## Requirements

### R1. 目录 marker 写入与子对象

- `PutObject "bucket/dir/"` 创建真实目录并记录显式 marker 元数据，不创建名为 `dir` 的普通文件。
- trailing-slash marker 仅支持零字节正文；非空正文必须返回明确的参数错误，不得静默丢弃。
- `PutObject "dir/"` 后，`PutObject "dir/file"`、`HeadObject`、`GetObject` 均成功。
- marker 的 `HeadObject`/`GetObject` 返回零字节对象语义；带 Range 的零字节对象沿用现有无效 Range 错误。

### R2. Marker 生命周期与列举

- `ListObjects`/`ListObjectsV2` 能返回显式 marker，支持 prefix、delimiter、分页 token；子对象仍按现有规则返回 CommonPrefixes/Contents。
- `DeleteObject "dir/"` 只删除 marker 元数据；有子对象时保留真实目录和子对象，无子对象时可删除空目录。
- marker 的 sidecar、目录和普通对象 sidecar 不得被错误列举为对象或被 reconcile 识别为 orphan sidecar。

### R3. 同名冲突

- 普通对象 `dir` 已存在时，创建 `dir/` 或 `dir/file` 返回明确的冲突错误（HTTP 409），不得返回 500。
- 目录 `dir/` 已存在时，写入普通对象 `dir` 返回明确的冲突错误（HTTP 409）；子级对象仍可正常写入。
- 本任务不引入隐藏编码布局，不支持普通对象 `dir` 与目录前缀 `dir/` 在 S3 逻辑上同时存在。

### R4. 存量兼容

- 历史文件型 marker 不得导致 list、head、delete 或 reconcile panic/500；现有普通文件必须保持可读、可删除。
- 由于旧 sidecar 没有可靠字段区分“零字节普通对象”和“错误落盘的 marker”，不得静默猜测并转换所有零字节文件。提供文档化迁移步骤：删除旧文件型 marker 后重新 PUT trailing-slash marker；迁移期间子级写入返回明确冲突错误。

### R5. 测试与质量

- 新增表驱动 storage 单测覆盖 trailing-slash put/head/get/delete/list、marker 与子对象、普通对象/目录冲突、历史文件型 marker 兼容和分页/delimiter 行为。
- `go test ./pkg/storage/... ./pkg/server/...`、`gofmt`、`go vet` 通过。
- 完成后更新 `.trellis/spec/backend/storage-guidelines.md`，记录 marker 表示、冲突和迁移契约。

## Out of Scope

- 不设计普通对象与目录 marker 的隐藏双重存储或全量磁盘迁移工具。
- 不改变 multipart 临时布局；multipart 目标写入必须复用相同的目录/普通对象冲突检查。
- 不改变 OpenList 的 `.openlist` 目录占位方式。
- 不支持以 `/` 结尾且携带非空正文的普通 S3 对象。

## Key Decisions

- 采用互斥模型：`dir/` 与 `dir/...` 共存；普通对象 `dir` 与目录前缀互斥。
- 目录 marker 使用真实目录加目录路径旁的 marker sidecar（默认 `<dir>.s3meta`，sidecar 增加向后兼容的 directory 标志），保持对象正文仍为原生文件字节。
- 冲突新增 storage sentinel，并映射为 S3 `Conflict`/HTTP 409；不能把 `os.MkdirAll` 的路径冲突暴露为 InternalError。

## Acceptance Criteria

- [ ] `PutObject "dir/"` → `PutObject "dir/a.txt"` → `HeadObject`/`GetObject` 全部成功。
- [ ] 带 delimiter 的 List 同时正确返回显式 marker 与子级对象/前缀，分页 token 稳定。
- [ ] `DeleteObject "dir/"` 删除 marker 但不影响子级；空目录可清理。
- [ ] 普通对象 `dir` 与目录 marker/子级写入的冲突返回 409 对应错误。
- [ ] 历史文件型 marker 可正常 list/head/delete，不产生 500；迁移步骤已文档化。
- [ ] storage/server 测试、gofmt、go vet 全部通过。
