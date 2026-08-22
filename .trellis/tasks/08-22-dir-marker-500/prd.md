# 修复目录占位 marker 导致子级写入 500

## Goal

`PutObject "dir/"`（以 `/` 结尾的空对象，即目录占位 marker）目前会在磁盘上落成**同名普通文件**，导致之后向该前缀写入任何对象（`PutObject "dir/file"`）时 `os.MkdirAll` 撞上同名文件而返回 500 InternalError，该前缀永久写死。修复为目录语义，使占位 marker 与子级写入共存。

## Background / Evidence

2026-08-22 线上排障（OpenList 403 事件，见 journal Session 29）期间本地复现确认：

```
PutObject 软件和插件/            -> OK   （错误地落成 data/library/软件和插件 文件 + .s3meta 边车）
PutObject 软件和插件/插件 A.zip   -> 500  （pkg/storage/file_backend.go:72 MkdirAll 撞上同名文件）
HeadObject 软件和插件/插件 A.zip  -> 500
```

- 触发客户端：AWS CLI `s3 cp/sync` 空目录、s3fs、rclone 等会写 `xxx/` 空对象的客户端。
- OpenList 不受影响（它用 `dir/.openlist` 占位文件，从不写 `dir/` 对象），故优先级不高。
- 现有相关代码：`pkg/storage/file_backend.go` 的 `PutObjectWithOptions`（:63-77，MkdirAll+OpenFile 落文件）、`HeadObject`、`GetObject`、`DeleteObject`、`ListObjects`（`file_backend.go:468` 前后 token 逻辑），路径校验在 `pkg/storage/path.go:37 ResolveObjectPath`。

## Requirements

- `PutObject "bucket/dir/"`（key 以 `/` 结尾）不再落成同名普通文件，按目录语义处理（建目录或等价机制），不再阻塞子级写入。
- `PutObject "dir/"` 后，`PutObject "dir/任意文件"` 成功；HeadObject/GetObject/ListObjects/DeleteObject 对 `dir/` 与 `dir/...` 的行为保持 S3 兼容（marker 在 list 中可见、可被删除；GetObject 一个"目录 marker"的响应语义需与现有 `.s3meta` 体系自洽）。
- 存量数据兼容：历史已落成同名文件的 marker（如用户桶里的 `软件和插件` 文件）升级后不应导致读写 panic 或数据丢失；至少要能正常 list/删除，最好自动或惰性修复为目录。
- 同名冲突的合法场景需定义清楚：真实对象 `dir`（无斜杠）与 marker `dir/` 共存时的 list/head 行为。

## Acceptance Criteria

- [ ] 复现路径修复：`PutObject "dir/"` → `PutObject "dir/a.txt"` → `HeadObject "dir/a.txt"` → `GetObject` 全部 200
- [ ] `ListObjectsV2`（带 delimiter）同时正确返回 marker 与子级对象
- [ ] `DeleteObject "dir/"` 可删除 marker，且不影响子级
- [ ] 存量"marker 落成文件"的数据：list 正常、删除正常，子级写入恢复（或文档化迁移方式）
- [ ] 新增表驱动单测覆盖：trailing-slash put/head/get/delete/list、与无斜杠同名对象共存、存量文件型 marker 兼容
- [ ] `go test ./pkg/storage/...` `./pkg/server/...` 通过，`gofmt`/`go vet` 干净

## Notes

- 排查记录详见 `.trellis/workspace/rsjwy/journal-1.md` Session 29。
- 修复时同步检查 `pkg/handlers/object.go` 的 Put/Head 路径是否有对 trailing-slash key 的假设，以及 reconcile（`storage.ReconcileBucket*`）对 marker 的统计口径。
- 完成后按 trellis 规则更新 `.trellis/spec/backend/storage-guidelines.md`。
