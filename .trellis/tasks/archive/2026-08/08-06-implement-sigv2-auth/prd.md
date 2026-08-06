# 实现 S3 Signature Version 2 验签

来源：GitHub issue #3 — "S3 v2 签名下含非 ASCII 字符的 key 验签失败（SignatureDoesNotMatch）"

## 背景与真实结论

issue 标题指向"非 ASCII key 的 v2 编码 bug"，**但实测证明 SigV2 从未被实现，与 key 是否含非 ASCII 无关。**

唯一的认证入口 `LocalSigV4Authenticator.Verify`（`pkg/auth/authenticator.go:27`）直接调用 `ParseAuthorization`，后者在 `pkg/auth/sigv4.go:34` 硬性要求：

```go
if !strings.HasPrefix(header, Algorithm+" ") {   // Algorithm = "AWS4-HMAC-SHA256"
    return ParsedAuthorization{}, NewError(CodeSignatureDoesNotMatch)
}
```

任何 `Authorization: AWS <AccessKey>:<Signature>` 都在此处被拒。规划期实测：

```
v2 ascii      -> SignatureDoesNotMatch
v2 non-ascii  -> SignatureDoesNotMatch
```

因此报告者"v2 + 纯 ASCII key ✅ 正常"的观察在代码层不成立——其客户端在该场景很可能实际发出了 v4 请求（多数 SDK 在 v2 不被接受时会回落）。这一点需在关闭 issue 时说明。

用户已确认选择**完整实现 SigV2 验签**，因此本任务是新增功能，而非修 bug。

## Requirements

### R1 v2 头部签名验签

- 接受 `Authorization: AWS <AccessKey>:<Base64Signature>`。
- `StringToSign` 按 S3 legacy 规范拼装：

  ```
  HTTP-Verb + "\n" +
  Content-MD5 + "\n" +
  Content-Type + "\n" +
  Date + "\n" +
  CanonicalizedAmzHeaders +
  CanonicalizedResource
  ```
- 签名算法：`Base64(HMAC-SHA1(SecretKey, StringToSign))`。
- 比较必须恒定时间，与 v4 现有 `ConstantTimeSignatureEqual` 同等强度。

### R2 CanonicalizedResource 的路径编码（issue 的核心关切）

- 必须使用**原始未解码**的请求路径（`r.URL.EscapedPath()` 语义），不得用已解码的 `r.URL.Path`，否则含 `%` 转义、中文、括号、空格的 key 会因编码往返不一致而验签失败。
- 子资源按字典序追加，仅限 S3 规定的子资源白名单（`acl`、`location`、`logging`、`notification`、`partNumber`、`policy`、`requestPayment`、`torrent`、`uploadId`、`uploads`、`versionId`、`versioning`、`versions`、`website`、`tagging`、`lifecycle`、`cors`、`delete`、`response-*` 等）。非白名单查询参数不参与签名。
- 必须与 `pkg/server/router.go:237` 的 `parseS3Path` 对同一路径的理解保持一致，避免"验签用 A 路径、落盘用 B 路径"。

### R3 CanonicalizedAmzHeaders

- 收集所有 `x-amz-*` 头，名称转小写，按名称字典序排序。
- 同名多值以逗号合并。
- 值需折叠内部空白（与 v4 的 `normalizeHeaderValue` 同语义）。
- 每条以 `\n` 结尾。
- **必须容忍 boto3 ≥ 1.36 新增的 `x-amz-checksum-crc32` / `x-amz-sdk-checksum-algorithm` 头**——它们本就是 `x-amz-*`，按规范纳入签名即可，不得因未知头名而拒绝（issue 的附加建议）。

### R4 时间与重放

- 支持 `Date` 头与 `x-amz-date` 头；`x-amz-date` 存在时优先，且此时 `StringToSign` 的 Date 位置必须为空字符串（规范要求）。
- 时钟偏移限制与 v4 一致：±15 分钟，超出返回 `RequestTimeTooSkewed`。
- 缺少可用时间戳 → `AccessDenied` 或 `SignatureDoesNotMatch`（择一并在 spec 固定）。

### R5 v2 预签名 URL（查询串形式）

- 接受 `AWSAccessKeyId` + `Expires` + `Signature` 三参数齐备的查询串形式。
- `StringToSign` 的 Date 位置替换为 `Expires` 的值。
- 过期 → `AccessDenied`。
- 判定必须与 v4 的 `HasPresignQuery`（`pkg/auth/authenticator.go:76`）互斥，不得互相误判。

### R6 与 v4 严格隔离

- v4 的判定优先：`Authorization` 以 `AWS4-HMAC-SHA256 ` 开头或命中 `HasPresignQuery` 时，一律走 v4，行为零变化。
- v2 与 v4 的 credential 查找、禁用检查、bucket 绑定检查、错误码映射保持一致。
- `pkg/auth` 现有测试全部保持通过，一个不改。

### R7 可开关

- v2 支持需要可通过配置关闭（默认值在 design.md 决定并说明理由）。v2 使用 SHA1 且无 payload 完整性保证，安全性弱于 v4，运维需要能显式禁用。
- 关闭时 v2 请求返回明确错误码，不得是模糊的 `SignatureDoesNotMatch`。

## Acceptance Criteria

- [ ] 正确签名的 v2 PUT/GET/HEAD/DELETE 通过验签并返回正常业务响应
- [ ] **key 含中文与括号**（如 `img/2026/08/05/屏幕截图(10)_20260805084300287924.png`）的 v2 请求验签通过 —— 这是 issue 的原始失败用例
- [ ] key 含空格、`+`、`%` 字面量、`&` 的 v2 请求验签通过
- [ ] 错误 secret → `SignatureDoesNotMatch`；未知 access key → `InvalidAccessKeyId`；禁用 credential → `AccessDenied`
- [ ] 时钟偏移超 15 分钟 → `RequestTimeTooSkewed`
- [ ] 携带 `x-amz-checksum-crc32` 与 `x-amz-sdk-checksum-algorithm` 的 v2 请求验签通过（boto3 ≥ 1.36 兼容）
- [ ] 子资源请求（`?acl`、`?uploads`、`?uploadId=`、`?tagging`）验签通过；非白名单参数（如 `?x-custom=1`）不影响签名结果
- [ ] `x-amz-date` 存在时 Date 位置置空的变体验签通过
- [ ] v2 预签名 URL 有效期内通过、过期返回 `AccessDenied`
- [ ] v2 关闭时 v2 请求返回约定的明确错误码
- [ ] 所有既有 v4 测试（`pkg/auth/*_test.go`、`pkg/server/router_test.go`）零修改全部通过
- [ ] 单元测试包含至少一组来自 AWS 官方文档的 v2 签名向量
- [ ] `go build ./...`、`go vet ./...`、`go test -count=1 ./...` 全绿
- [ ] `git diff --check` 无输出
- [ ] `.trellis/spec/backend/auth-quota-guidelines.md` 增补 v2 契约，并明确记录 v2 的安全弱点与默认开关状态

## Notes

- 实现代码由其他 AI 落盘。
- 与 #2 的交集：aws-chunked 请求也可能用 v2 签名。两任务都改认证/请求体路径，#2 先落地，本任务需在 #2 合并后再动，避免中间件顺序冲突。
- issue 的附加建议（boto3 checksum 头兼容）已并入 R3，不单独立项。
