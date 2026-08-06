# Implement — SigV2 验签

## 前置

**必须等 `08-06-fix-aws-chunked-upload` 合并后再开始。** 原因见 design.md §5：v2 + aws-chunked 的组合测试需要解码器就位。

## 落地顺序

### S1 v2 签名原语（纯函数，无依赖）

1. 新建 `pkg/auth/sigv2.go`
   - `AlgorithmV2 = "AWS"`
   - `ParsedV2Authorization`、`ParseV2Authorization`
   - `StringToSignV2(r *http.Request, expires string) string`
   - `CanonicalizedAmzHeadersV2(h http.Header) string`
   - `CanonicalizedResourceV2(r *http.Request) string`
   - `SignStringV2(secret, stringToSign string) string`
   - 子资源白名单常量（对照 AWS 文档完整列出）
2. **路径取值优先级**：先 `r.URL.RawPath`，为空回落 `r.URL.EscapedPath()`。这是 issue 的技术核心，不能想当然，用带中文的 target 写测试断言实际取到的字节。
3. 新建 `pkg/auth/sigv2_test.go`，覆盖 design.md §6 的单元清单，含至少一组 AWS 官方文档向量
4. 门禁：`go test -count=1 ./pkg/auth/`

复用现有 `normalizeHeaderValue`（`pkg/auth/sigv4.go:202`）做空白折叠，保持两版语义一致。

### S2 v2 认证器

1. 新建 `pkg/auth/authenticator_v2.go`：`LocalSigV2Authenticator` + `Verify`
   - 头部签名与查询串预签名两条路径
   - 时间：`x-amz-date` 优先（此时 Date 行置空），否则 `Date` 头；RFC1123 与 ISO8601 都要能解析
   - 时钟偏移复用 `DefaultClockSkew`（`pkg/auth/authenticator.go:11`）
   - credential 查找/状态检查/Identity 构造与 v4 逐字段对齐
2. 新建 `pkg/auth/authenticator_v2_test.go`，含**中文 key 端到端验签通过**这条关键用例
3. 门禁：`go test -count=1 ./pkg/auth/`

### S3 分派器与错误码

1. `pkg/auth/errors.go` 新增 `CodeInvalidRequest = "InvalidRequest"`
2. `pkg/handlers/common.go:93` 消息表新增 `InvalidRequest` 的可读消息
3. 新建 `pkg/auth/multischeme.go`：`MultiSchemeAuthenticator`
   - 判定顺序严格按 design.md §2 的五条
   - `AWS4-` 前缀绝不能落入 v2 分支
   - v2 为 nil 时返回 `CodeInvalidRequest`
4. 新建 `pkg/auth/multischeme_test.go`
5. 门禁：`go test -count=1 ./pkg/auth/ ./pkg/handlers/`

### S4 配置与装配

1. `pkg/config/config.go` 的 `Config` 增加认证相关配置项（沿用 `yaml:"..."` 风格；若无 `auth` 段则新建 `AuthConfig`），字段 `AllowSigV2 bool`，**默认 false**
2. `configs/` 下示例配置补注释说明该项与其安全含义
3. `cmd/natives3bridge/main.go` 与 `cmd/node/main.go` 构造 authenticator 处改为 `NewMultiSchemeAuthenticator(v4, v2OrNil)`
4. 门禁：`go build ./... && go test -count=1 ./pkg/config/`

### S5 端到端与 spec

1. `pkg/server/router_test.go` 追加一条 v2 签名 PUT 走完整中间件链的用例
2. `.trellis/spec/backend/auth-quota-guidelines.md`
   - 修订 Contracts 第一条（当前写的是"Only header-based `Authorization: AWS4-HMAC-SHA256 ...` is accepted in this layer"，需改为反映双方案与分派顺序）
   - 新增 v2 的 StringToSign / CanonicalizedResource（含 RawPath 优先规则）/ CanonicalizedAmzHeaders 契约
   - 错误矩阵新增 v2 分支与 v2 禁用时的 `InvalidRequest`
   - 明确记录：v2 默认关闭；v2 不签 body，v2 + aws-chunked 的完整性完全依赖解码器校验
3. `README.md` 补一句签名版本支持说明与开关方式
4. 门禁：`go build ./... && go vet ./... && go test -count=1 ./... && git diff --check`

## 不要做

- 不修改 `LocalSigV4Authenticator`、`ParseAuthorization`、`CanonicalRequest`、`StringToSign` 等任何 v4 现有函数（R6）
- 不修改任何既有 v4 测试
- 不改 `server.Auth` 中间件（分派在 Verify 内部完成）
- 不动 aws-chunked 解码器（那是 #2 的产物，本任务只消费）
- 不实现 v2 的 POST 表单上传签名（`policy` base64 形式），范围外

## 完成判据

`prd.md` 的 Acceptance Criteria 全部勾选，且 `go test -count=1 ./...` 全绿。特别地：含中文与括号的 key 在 v2 下验签通过（issue 原始用例），且所有 v4 测试零修改通过。
