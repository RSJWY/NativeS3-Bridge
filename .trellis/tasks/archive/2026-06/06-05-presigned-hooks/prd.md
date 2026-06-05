# 子任务 5：预签名 URL 与事件钩子

> 父任务：`06-05-natives3-bridge`。实现 S3 预签名 URL 的校验，以及上传/删除事件的下游业务回调（项目相对 Rclone 的核心差异化扩展点）。

## ⛔ 执行者硬约束
需求、接口、事件载荷为**冻结规格**。不得修改/删减/替换。问题写 `research/change-request.md` 上报。详见父任务 `prd.md`。

---

## Goal

支持 S3 预签名 URL（query 形式 SigV4）的校验，使普通 HTTP 客户端可在有效期内直接 PUT/GET，无需带 Authorization 头。实现事件钩子系统：对象创建（含 multipart 完成）、对象删除后，异步触发已配置的 Webhook 回调，载荷包含 bucket/key/size/metadata 等。

## 依赖
- 子任务 3（SigV4 工具函数复用）。
- 子任务 4（对象/multipart 完成事件源；DB 的 HookConfig 模型来自 S1）。

## Requirements

### A. 预签名 URL
1. `pkg/handlers/presigned.go`：
   - 识别 query 形式 SigV4 请求：含 `X-Amz-Algorithm=AWS4-HMAC-SHA256`、`X-Amz-Credential`、`X-Amz-Date`、`X-Amz-Expires`、`X-Amz-SignedHeaders`、`X-Amz-Signature`。
   - 复用 S3 的 CanonicalRequest/signing-key 函数，按 query 形式重算签名比对。
   - 校验过期：`X-Amz-Date + X-Amz-Expires` 与当前时间比较，过期→403 `AccessDenied`（或 `ExpiredToken`）。
   - 校验通过后，与普通鉴权一样把 Identity 注入 context，复用既有 object handler（GET/PUT）。
   - 配额检查同样适用于预签名 PUT。
2. 在 Auth 中间件里区分：请求带 query 签名参数 → 走预签名校验分支；否则走 header SigV4。
3. （可选生成侧）提供一个内部函数 `GeneratePresignedURL(cred, method, bucket, key, expires)`，供 webadmin 或工具生成 URL；至少保证校验侧完整可用。

### B. 事件钩子
4. `pkg/hooks`：
   - `event.go`：`Event{Type, Bucket, Key, Size, ETag, Metadata, CredentialID, Timestamp}`；Type ∈ `ObjectCreated` / `ObjectDeleted`。
   - `manager.go`：从 DB `hook_configs`（enabled）加载钩子；`Emit(event)` 异步分发到匹配 event 类型的钩子；带 worker 队列（缓冲 channel），失败重试（指数退避，最多 N 次，默认 3），不阻塞主请求路径。
   - `webhook.go`：POST JSON 到配置的 URL，超时（默认 5s），记录响应状态。
   - 钩子配置变更后可重载（提供 `Reload()`，供 webadmin 改钩子后调用）。
5. 触发点接入：
   - PutObject 成功 → `Emit(ObjectCreated)`
   - CompleteMultipartUpload 成功 → `Emit(ObjectCreated)`
   - DeleteObject 成功 → `Emit(ObjectDeleted)`
6. Emit 必须**异步、非阻塞**：钩子失败不影响 S3 操作返回结果，仅记日志。

## 非目标
- 不做 SQS/SNS 风格的完整 S3 事件通知协议（仅简化 Webhook）。
- 不做插件式本地脚本执行（第一版仅 Webhook；接口预留可扩展）。
- 不做前端钩子配置界面（webadmin 子任务如纳入则在那边，本任务提供 API/Reload 钩子）。

## Acceptance Criteria

- [ ] `go build`/`go vet`/`go test ./pkg/hooks/...` 通过。
- [ ] 生成一个预签名 GET URL（aws-cli `s3 presign` 或内部函数），用 `curl` 在有效期内可直接下载成功（200/206）。
- [ ] 预签名 PUT URL 在有效期内可 `curl -T` 上传成功，文件原生落地。
- [ ] 过期的预签名 URL 返回 403。
- [ ] 篡改预签名 URL 的签名或路径 → 403。
- [ ] 预签名 PUT 同样受配额限制。
- [ ] 配置一个本地接收端（如 `nc`/简单 HTTP 服务），上传对象后收到 `ObjectCreated` POST，JSON 含 bucket/key/size。
- [ ] 删除对象后收到 `ObjectDeleted` 回调。
- [ ] Multipart 完成后收到 `ObjectCreated` 回调。
- [ ] Webhook 接收端宕机时，S3 上传仍正常返回（钩子失败不阻塞），且有重试与失败日志。
- [ ] 钩子配置 `enabled=false` 时不触发。

## Notes
- 预签名校验必须复用 S3 的签名核心函数，禁止复制一份签名逻辑导致不一致。
- 事件队列容量、重试次数、超时写入 config 或常量，记入 research。
