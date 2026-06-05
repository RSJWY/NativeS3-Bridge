# 子任务 4 执行清单：分段上传与元数据 sidecar

> 按顺序执行，逐步勾选，不改规格。

## 步骤

- [x] 1. `pkg/storage/metadata.go`：Sidecar 类型 + 原子读/写/删 + 缺失容错。单测。
- [x] 2. 把 sidecar 接入 S2 的 PutObject / GetObject / HeadObject / DeleteObject（写、回填、删）。
- [x] 3. `pkg/storage/multipart.go`：Create / UploadPart / Complete（流式合并 + multipart ETag）/ Abort / ListParts。单测覆盖合并顺序与 ETag 格式。
- [x] 4. `pkg/handlers/multipart.go`：上述 5 个 multipart HTTP 适配 + XML 请求/响应解析。
- [x] 5. `pkg/handlers/object.go`（或新增）：PutObjectTagging / GetObjectTagging / DeleteObjectTagging。
- [x] 6. 扩展 `pkg/server/router.go`：按 query 参数（`uploads`/`uploadId`/`partNumber`/`tagging`）分发。
- [x] 7. 配额接入：Complete 前 Check（合并后总大小），成功后 Commit(OpPut)。
- [x] 8. GC goroutine（main 装配）+ 新增 config 项；research 记录并请规划者确认。
- [x] 9. 确认 ListObjectsV2 仍排除 `*.s3meta`。
- [x] 10. research：配额结算时机、GC 配置项、multipart ETag 算法验证记录。

## 验证命令

```bash
go build ./... && go vet ./... && go test ./pkg/storage/...
go run ./cmd/natives3bridge --config configs/config.sqlite.yaml &
export AWS_ACCESS_KEY_ID=<ak> AWS_SECRET_ACCESS_KEY=<sk>
EP="--endpoint-url http://localhost:9000"

# 大文件触发 multipart
head -c 120000000 /dev/urandom > /tmp/big.bin
aws $EP s3 cp /tmp/big.bin s3://test-bucket/big.bin
ls -la ./data/test-bucket/big.bin            # 单原生文件
ls ./data/.multipart                          # 完成后无残留
aws $EP s3 cp s3://test-bucket/big.bin /tmp/big2.bin && cmp /tmp/big.bin /tmp/big2.bin

# 元数据
aws $EP s3api put-object --bucket test-bucket --key m.txt --body /tmp/a.txt \
  --metadata author=jdoe,team=infra --content-type text/plain
aws $EP s3api head-object --bucket test-bucket --key m.txt   # 应含 x-amz-meta-author/team

# 标签
aws $EP s3api put-object-tagging --bucket test-bucket --key m.txt \
  --tagging 'TagSet=[{Key=env,Value=prod}]'
aws $EP s3api get-object-tagging --bucket test-bucket --key m.txt

# sidecar 不出现在列举
aws $EP s3 ls s3://test-bucket/    # 不应看到 *.s3meta

# 无 sidecar 容错：手动拷一个文件进去再 GET
cp /tmp/a.txt ./data/test-bucket/external.txt
aws $EP s3 cp s3://test-bucket/external.txt /tmp/ext.txt && diff /tmp/a.txt /tmp/ext.txt

# GC：造一个过期目录验证清理（按实现的 TTL/触发方式）
```

## 完成门
- 大文件落地单原生文件、临时目录清理、ETag 带 -N、元数据/标签可取回、无 sidecar 容错、列举排除 sidecar、GC 生效、配额按合并大小结算。
- 对照 `prd.md` Acceptance Criteria 全勾。

## 提交
- `feat(storage): multipart upload with native merge, metadata & tagging sidecar`
