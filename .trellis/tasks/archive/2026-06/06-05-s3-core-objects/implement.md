# 子任务 2 执行清单：S3 核心对象操作与 1:1 原生映射

> 按顺序执行，逐步勾选，不改规格。

## 步骤

- [ ] 1. `pkg/storage/path.go`：bucket 名校验、`ResolveBucketPath`、`ResolveObjectPath` + 逃逸检查。先写单元测试覆盖 `..` 逃逸与非法 bucket 名。
- [ ] 2. `pkg/storage/backend.go`：`Backend` 接口、`ObjectInfo`/`ListResult`/`BucketInfo`/`Range` 类型。
- [ ] 3. `pkg/storage/file_backend.go`：`FileBackend` 实现 PutObject（临时文件+rename+MD5 ETag）、GetObject（Range）、HeadObject、DeleteObject。
- [ ] 4. `FileBackend.ListObjects`（prefix/delimiter/maxKeys/token）、`ListBuckets`（排除隐藏项）。
- [ ] 5. `pkg/handlers/common.go`：`WriteS3Error`、XML 编码辅助、标准响应头。
- [ ] 6. `pkg/handlers/object.go`：Put/Get/Head/Delete + ListObjectsV2 的 HTTP 适配。
- [ ] 7. `pkg/handlers/bucket.go`：ListBuckets、HeadBucket。
- [ ] 8. `pkg/server/router.go`：路径解析 + 方法分发 + 中间件链（Auth/Quota 占位）。
- [ ] 9. `pkg/server/server.go`：`http.Server` 装配 + 优雅关闭。
- [ ] 10. 接入 `main.go`：启动 S3 server（替换 S1 的 TODO）。
- [ ] 11. 写 `scripts/smoke-test.sh`（aws-cli 上传/下载/列举/删除）。

## 验证命令

```bash
go build ./... && go vet ./... && go test ./pkg/storage/...
go run ./cmd/natives3bridge --config configs/config.sqlite.yaml &

export AWS_ACCESS_KEY_ID=x AWS_SECRET_ACCESS_KEY=x   # 本期未鉴权，任意值
EP="--endpoint-url http://localhost:9000"
echo "hello native s3" > /tmp/a.txt
aws $EP s3 cp /tmp/a.txt s3://test-bucket/dir/a.txt
ls -l ./data/test-bucket/dir/a.txt          # 原生文件存在
aws $EP s3 cp s3://test-bucket/dir/a.txt /tmp/b.txt && diff /tmp/a.txt /tmp/b.txt
aws $EP s3api head-object --bucket test-bucket --key dir/a.txt
aws $EP s3 ls s3://test-bucket/dir/
aws $EP s3 ls
aws $EP s3 rm s3://test-bucket/dir/a.txt
curl -s -r 0-4 $EP_HOST/test-bucket/dir/a.txt   # Range（若文件仍在）
```

## 完成门
- 命令全过；磁盘原生文件可直接打开；Range 返回 206；删除后 404；`..` 逃逸被拒。
- 对照本任务 `prd.md` Acceptance Criteria 全勾。

## 提交
- `feat(s3): http server, 1:1 path mapping, core object/bucket operations`
