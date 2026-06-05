# 子任务 3 执行清单：SigV4 鉴权与按密钥配额

> 按顺序执行，逐步勾选，不改规格。

## 步骤

- [ ] 1. `pkg/auth/identity.go`：`Identity`、`Authenticator` 接口、context key。
- [ ] 2. `pkg/auth/sigv4.go`：CanonicalRequest / StringToSign / signing key 派生 / 签名比对，做成可复用纯函数。写单元测试用 AWS 官方测试向量验证。
- [ ] 3. `pkg/auth/credential_store.go`：DB 查询 + TTL 缓存 + Invalidate。
- [ ] 4. `pkg/auth/authenticator.go`：`LocalSigV4Authenticator.Verify`（含时钟偏移、disabled 处理）。
- [ ] 5. `pkg/quota/quota.go`：`Check` + `Commit`（原子 used_bytes 更新 + request_stats upsert，用 clause.OnConflict）。写测试覆盖 put/get/delete 与下限 0。
- [ ] 6. 替换 `pkg/server/router.go` 的 Auth/Quota 占位为真实中间件；handler 成功后调用 Commit。
- [ ] 7. `main.go` 装配 authenticator + credentialStore + quota（注入 db）。
- [ ] 8. 临时种子：提供一个 CLI/启动选项或测试夹具插入一个测试密钥（正式创建走 webadmin 子任务）。记录于 research。
- [ ] 9. research：SecretKey 存储形态、used_bytes 跨方言原子更新方案、软/硬配额选择。

## 验证命令

```bash
go build ./... && go vet ./... && go test ./pkg/auth/... ./pkg/quota/...
go run ./cmd/natives3bridge --config configs/config.sqlite.yaml &

# 用 DB 中真实密钥
export AWS_ACCESS_KEY_ID=<seeded-ak> AWS_SECRET_ACCESS_KEY=<seeded-sk>
EP="--endpoint-url http://localhost:9000"
aws $EP s3 cp /tmp/a.txt s3://test-bucket/a.txt        # 成功
# 错误 secret
AWS_SECRET_ACCESS_KEY=wrong aws $EP s3 cp /tmp/a.txt s3://test-bucket/b.txt   # 403
# 不存在的 AK
AWS_ACCESS_KEY_ID=NOPE aws $EP s3 ls                    # 403 InvalidAccessKeyId
# 配额：先把该 key 的 quota_bytes 调到很小，再传大文件 → 403
# 用量核对
sqlite3 ./natives3.db "select access_key,used_bytes from credentials;"
sqlite3 ./natives3.db "select * from request_stats;"
# 并发用量正确性
seq 1 20 | xargs -P8 -I{} aws $EP s3 cp /tmp/a.txt s3://test-bucket/c{}.txt
# 核对 used_bytes == 20 * filesize + 之前的量
```

## 完成门
- 鉴权正/负用例全过；配额拦截生效且不误增 used_bytes；统计累加正确；并发无丢失；与 aws-cli 互通。
- 对照 `prd.md` Acceptance Criteria 全勾。

## 提交
- `feat(auth): SigV4 verification, per-key quota and usage accounting`
