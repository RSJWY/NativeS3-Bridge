# 子任务 5 执行清单：预签名 URL 与事件钩子

> 按顺序执行，逐步勾选，不改规格。

## 步骤

- [ ] 1. `pkg/handlers/presigned.go`：query SigV4 解析 + 过期校验 + 复用 S3 签名函数比对。单测覆盖过期/篡改。
- [ ] 2. 改 Auth 中间件：检测预签名 query → 走 presign.Verify，否则 header 鉴权；通过后注入 Identity 复用 object handler。
- [ ] 3. （可选）`GeneratePresignedURL` 生成函数。
- [ ] 4. `pkg/hooks/event.go`：Event 类型。
- [ ] 5. `pkg/hooks/webhook.go`：WebhookHook（Match/Deliver，超时）。
- [ ] 6. `pkg/hooks/manager.go`：队列 + worker + 重试 + Reload + 非阻塞 Emit。单测覆盖重试与 enabled 过滤。
- [ ] 7. 触发点接入：PutObject / CompleteMultipart / DeleteObject 成功后 Emit。
- [ ] 8. main 装配 Manager.Start()，注入依赖；暴露 Reload 给 webadmin。
- [ ] 9. research：队列/重试/超时常量、预签名 payload hash 约定。

## 验证命令

```bash
go build ./... && go vet ./... && go test ./pkg/hooks/...
go run ./cmd/natives3bridge --config configs/config.sqlite.yaml &
export AWS_ACCESS_KEY_ID=<ak> AWS_SECRET_ACCESS_KEY=<sk>
EP="--endpoint-url http://localhost:9000"

# 预签名 GET
aws $EP s3 cp /tmp/a.txt s3://test-bucket/p.txt
URL=$(aws $EP s3 presign s3://test-bucket/p.txt --expires-in 60)
curl -s "$URL" -o /tmp/p.txt && diff /tmp/a.txt /tmp/p.txt    # 成功
# 过期（expires-in 1，等 2s）
URL2=$(aws $EP s3 presign s3://test-bucket/p.txt --expires-in 1); sleep 2
curl -s -o /dev/null -w "%{http_code}" "$URL2"   # 403
# 篡改
curl -s -o /dev/null -w "%{http_code}" "${URL}TAMPER"   # 403

# 事件钩子：起一个接收端
# 在 hook_configs 表插入 {url: http://localhost:8888/hook, events: "ObjectCreated,ObjectDeleted", enabled:1}
python3 -m http.server 8888 &   # 或用能打印 body 的接收器
aws $EP s3 cp /tmp/a.txt s3://test-bucket/h.txt      # 应触发 ObjectCreated POST
aws $EP s3 rm s3://test-bucket/h.txt                  # 应触发 ObjectDeleted POST
# 接收端宕机时上传仍成功
```

## 完成门
- 预签名有效期内可用、过期/篡改被拒、受配额限制；三类事件回调送达、宕机不阻塞、enabled=false 不触发。
- 对照 `prd.md` Acceptance Criteria 全勾。

## 提交
- `feat(ext): presigned URL verification and async webhook event hooks`
