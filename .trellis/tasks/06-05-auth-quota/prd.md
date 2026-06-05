# 子任务 3：SigV4 鉴权与按密钥配额

> 父任务：`06-05-natives3-bridge`。把 S2 的占位中间件替换为真实的签名校验、配额检查与用量统计。

## ⛔ 执行者硬约束
需求、接口、错误码、配额语义为**冻结规格**。不得修改/删减/替换。问题写 `research/change-request.md` 上报。详见父任务 `prd.md`。

---

## Goal

实现标准 AWS Signature V4 校验（密钥来自数据库），实现"按访问密钥限总容量"的配额检查，以及按密钥按天的请求/流量统计累加。把 S2 预留的 Auth/Quota 占位中间件替换为真实实现，签名/接口形态不变。

## 依赖
- 子任务 1（db、config、模型）。
- 子任务 2（中间件链、handler、backend）。

## Requirements

1. `pkg/auth`：
   - `Identity` 与 `Authenticator` 接口（与父 `design.md` 第 4 节一致）。
   - `LocalSigV4Authenticator`：实现 `Verify(r) (*Identity, error)`，解析 `Authorization: AWS4-HMAC-SHA256 ...` 头，按 SigV4 规范重算签名并比对；校验 `X-Amz-Date` 时钟偏移（默认 ±15 分钟）。
   - `credential_store.go`：按 AccessKey 从 DB 查 `Credential`，带短 TTL 内存缓存（默认 60s）和失效；`status=disabled` 视为无效密钥。
   - 支持 header 签名；预签名 URL 的 query 签名校验放在 S5，但本任务的 SigV4 工具函数要可复用。
2. `pkg/quota`：
   - `Check(id *Identity, incomingSize int64) error`：`QuotaBytes>0 && UsedBytes+incomingSize > QuotaBytes` → 返回配额超限错误。
   - `Commit(credID uint, deltaBytes int64, op string)`：在**单事务**内原子更新 `credentials.used_bytes`（自增/自减，下限 0）并 upsert `request_stats`（按 UTC 日期，put/get/delete 计数 + bytes_in/out）。
   - 用 `UPDATE ... SET used_bytes = used_bytes + ?` 原子语句，避免读改写竞态。
3. 中间件接入（替换 S2 占位，签名不变）：
   - Auth 中间件：调用 `Verify`，失败→标准 S3 403（`SignatureDoesNotMatch` / `InvalidAccessKeyId` / `AccessDenied`），成功把 `Identity` 放入 request context。
   - Quota 中间件：仅对写操作（PUT Object / 后续 Complete Multipart）做 `Check`；对象大小取自 `Content-Length`。
   - 用量提交：在 handler 成功完成对象写入/删除/读取后调用 `quota.Commit`（PUT→used+size、put_count、bytes_in；DELETE→used-size、delete_count；GET→get_count、bytes_out）。
4. 错误信息不泄露内部细节；返回标准 S3 XML。

## 非目标
- 预签名 URL 校验（S5）。
- 多租户 bucket 隔离、ACL（超出范围）。
- 外部鉴权中心实现（仅保持接口可扩展）。

## Acceptance Criteria

- [ ] `go build`/`go vet`/`go test ./pkg/auth/...` 通过。
- [ ] 用正确密钥（DB 中存在、enabled）经 aws-cli 上传/下载成功。
- [ ] 用错误 secret 签名 → 403 `SignatureDoesNotMatch`（标准 XML）。
- [ ] 用不存在的 AccessKey → 403 `InvalidAccessKeyId`。
- [ ] `status=disabled` 的密钥 → 403。
- [ ] 时钟偏移超过 ±15 分钟的请求被拒（`RequestTimeTooSkewed`）。
- [ ] 设某密钥 `quota_bytes` 小于待传文件 → 上传被拒（配额超限标准错误），且 `used_bytes` 未被错误增加。
- [ ] 成功上传后 `credentials.used_bytes` 增加且 `request_stats` 当日 put_count/bytes_in 增加。
- [ ] 删除对象后 `used_bytes` 相应减少（不为负）。
- [ ] 并发上传同一密钥多文件，`used_bytes` 最终值正确（无竞态丢失），用脚本并发验证。
- [ ] SigV4 与官方 aws-cli/SDK 互通（真实客户端能连通，非仅自测）。

## Notes
- SigV4 可手写或复用 `aws-sdk-go-v2` 的 signer 做校验侧；选择记入 research，保持单文件构建。
- SecretKey 存储形态（明文/加密）记入 research；若加密需提供解密以重算签名。第一版可明文但在 research 标注风险。
