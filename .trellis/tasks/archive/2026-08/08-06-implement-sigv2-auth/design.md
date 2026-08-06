# Design — SigV2 验签

## 1. 现状

`pkg/auth` 只有一条认证路径：

```
Auth middleware → authenticator.Verify(r)
                      ├─ HasPresignQuery(r) → verifyPresigned  (v4 query)
                      └─ ParseAuthorization(header)             (v4 header) ← 硬要求 AWS4-HMAC-SHA256 前缀
```

`Authenticator` 是单方法接口（`pkg/auth/identity.go:18`），`server.Auth` 只依赖它（`pkg/server/router.go:309`）。这给了一个干净的扩展点：**不改中间件，只改 Verify 的分派。**

## 2. 方案选择

| 方案 | 做法 | 评价 |
|---|---|---|
| A | 在 `LocalSigV4Authenticator.Verify` 内加 v2 分支 | 命名与职责错位（名字叫 SigV4 却处理 v2），且两套算法挤在一个类型里 |
| B | 新 `LocalSigV2Authenticator` + 组合式 `MultiSchemeAuthenticator` 按签名形态分派 | **采用。** 各算法独立可测，v4 类型零改动，符合 R6 的隔离要求 |
| C | 在 `server.Auth` 中间件里判定 | 认证算法泄漏进 HTTP 层，且 `Authenticator` 接口形同虚设 |

选 B。新结构：

```
Auth middleware → MultiSchemeAuthenticator.Verify(r)
                      ├─ 形态判定 ─┬─ v4 → LocalSigV4Authenticator (原样，零改动)
                                   └─ v2 → LocalSigV2Authenticator (新增)
```

### 形态判定优先级（必须严格按此顺序，对应 R6）

1. `Authorization` 以 `AWS4-HMAC-SHA256 ` 开头 → v4
2. `HasPresignQuery(r)` 为真 → v4 预签名
3. `Authorization` 以 `AWS ` 开头（且不是 `AWS4-`）→ v2
4. 查询串同时含 `AWSAccessKeyId`、`Expires`、`Signature` → v2 预签名
5. 其余 → 交由现有匿名/拒绝逻辑

第 3 条的前缀检查需注意 `AWS4-HMAC-SHA256` 本身也以 `AWS` 开头，判定必须用 `strings.HasPrefix(h, "AWS ")`（带空格）并已被第 1 条拦截在前。

## 3. 新增组件

### 3.1 `pkg/auth/sigv2.go`

```go
const AlgorithmV2 = "AWS"

// ParsedV2Authorization 是 "AWS <AccessKey>:<Base64Signature>" 的解析结果。
type ParsedV2Authorization struct {
	AccessKey string
	Signature string
}

func ParseV2Authorization(header string) (ParsedV2Authorization, error)

// StringToSignV2 按 S3 legacy 规范拼装待签字符串。
// expires 非空时用于预签名 URL，替换 Date 位置。
func StringToSignV2(r *http.Request, expires string) string

func CanonicalizedAmzHeadersV2(h http.Header) string
func CanonicalizedResourceV2(r *http.Request) string
func SignStringV2(secret, stringToSign string) string   // Base64(HMAC-SHA1)
```

**`CanonicalizedResourceV2` 是本任务的技术核心**（R2）。实现要点：

```go
func CanonicalizedResourceV2(r *http.Request) string {
	path := r.URL.EscapedPath()   // 原始转义形态，绝不用 r.URL.Path
	if path == "" {
		path = "/"
	}
	// 追加白名单子资源，按名字典序；有值的写 name=value，无值的只写 name
}
```

为什么必须用 `EscapedPath()`：客户端签名时用的是它自己发出的转义路径。`r.URL.Path` 是 Go 解码后的结果，对 `屏幕截图(10).png` 这类 key，解码再重编码不保证字节一致（Go 与 botocore 对 `(` `)` 等 sub-delims 的转义策略不同），签名必然不匹配。这正是 issue 标题所指的编码问题——只是它在 v2 完全缺失的前提下从未被触发过。

注意 `EscapedPath()` 的行为：当 `RawPath` 为空（即转义形态与 `Path` 的默认编码一致）时它返回 `Path` 的重新编码结果。为确保拿到客户端字面发送的字节，需要优先读 `r.URL.RawPath`，为空时回落 `EscapedPath()`。这一点要在测试中用带中文的 target 显式验证。

子资源白名单常量化，避免把 `?x-custom=1` 之类无关参数带进签名（R2）。

### 3.2 `pkg/auth/authenticator_v2.go`

```go
type LocalSigV2Authenticator struct {
	store     *CredentialStore
	now       func() time.Time
	clockSkew time.Duration
}

func NewLocalSigV2Authenticator(store *CredentialStore) *LocalSigV2Authenticator
func (a *LocalSigV2Authenticator) Verify(r *http.Request) (*Identity, error)
```

流程与 v4 对齐（R6）：解析 → 时间校验 → `store.Get(accessKey)` → 状态检查 → 计算期望签名 → 恒定时间比较 → 返回同构 `Identity`。

v2 无 region/service scope，所以不做 region 校验。

时间处理（R4）：`x-amz-date` 优先，存在时 StringToSign 的 Date 行置空（该时间已经作为 `x-amz-date` 进入 CanonicalizedAmzHeaders）；否则用 `Date` 头。两者都支持 RFC1123 与 ISO8601 解析。

### 3.3 `pkg/auth/multischeme.go`

```go
type MultiSchemeAuthenticator struct {
	v4 Authenticator
	v2 Authenticator   // nil 表示 v2 已禁用
}

func NewMultiSchemeAuthenticator(v4, v2 Authenticator) *MultiSchemeAuthenticator
func (a *MultiSchemeAuthenticator) Verify(r *http.Request) (*Identity, error)
```

`v2 == nil` 且请求是 v2 形态时（R7）返回 `NewError(CodeInvalidRequest)`，需在 `pkg/auth/errors.go` 新增 `CodeInvalidRequest = "InvalidRequest"`，并在 `pkg/handlers/common.go` 的消息表加入可读消息（如 "Signature version 2 is not supported. Use AWS Signature Version 4."）。用明确错误码而非 `SignatureDoesNotMatch`，避免运维排查时把"被禁用"误认为"签名算错"。

## 4. 开关与默认值

配置项：`auth.allow_sigv2`（沿用 `pkg/config` 现有风格，具体字段名以该包实际约定为准）。

**默认关闭。** 理由：v2 用 SHA1、不覆盖请求体、无 scope 绑定，安全性明显弱于 v4；AWS 自身已在多数区域停止支持。仓库既有安全基线（TLS、匿名限流、登录节流）都倾向保守默认。需要 v2 的用户显式开启，并在文档中标注风险。

装配点：`cmd/natives3bridge/main.go` 与 `cmd/node/main.go` 构造 authenticator 处，用 `NewMultiSchemeAuthenticator` 包一层；开关关闭时第二个参数传 nil。

## 5. 与 #2 的关系

两任务都触及请求处理链：#2 在 `Auth` 之后插入 `AwsChunked` 中间件；本任务只改 `Verify` 内部分派，不动中间件链。因此**无直接冲突**，但仍要求 #2 先落地，原因是 v2 + aws-chunked 组合的测试需要 #2 的解码器就位才有意义。

v2 场景下 aws-chunked 的 payload 完整性：v2 本身不签 body，`x-amz-content-sha256` 也不参与 v2 验签，因此 v2 + STREAMING-* 的组合完全依赖 #2 的解码器做长度与 trailer 校验和检查。这一限制要写入 spec。

## 6. 测试计划

`pkg/auth/sigv2_test.go`（单元）：
- AWS 官方文档 v2 签名向量至少一组，逐字段断言 StringToSign
- `CanonicalizedResourceV2`：纯 ASCII、中文、括号、空格、`+`、`%` 字面量、`&`；带/不带子资源；子资源排序；非白名单参数被排除
- `CanonicalizedAmzHeadersV2`：大小写混合、同名多值合并、排序、空白折叠、`x-amz-checksum-crc32` 与 `x-amz-sdk-checksum-algorithm` 纳入
- `x-amz-date` 优先且 Date 行置空的变体
- `ParseV2Authorization`：合法、缺冒号、空 access key、空签名、`AWS4-` 前缀不得被误认为 v2

`pkg/auth/authenticator_v2_test.go`：
- 正确签名通过并返回正确 `Identity`
- 错误 secret / 未知 key / 禁用 credential / 时钟偏移 的错误码矩阵
- v2 预签名有效与过期
- **中文 key 端到端验签通过**（issue 原始用例，最关键的一条）

`pkg/auth/multischeme_test.go`：
- 五条形态判定分支各一例
- `AWS4-HMAC-SHA256` 不被误判为 v2
- v2 禁用时的错误码
- v4 请求在 MultiScheme 下行为与直接用 v4 authenticator 完全一致

`pkg/server/router_test.go`：
- 一条 v2 签名的 PUT 端到端走完中间件链并落盘

## 7. 风险

- **`EscapedPath()` 与 `RawPath` 的取值差异**是本任务最可能出错的点。必须用真实含中文的 target 构造 `httptest.NewRequest` 并断言拿到的是字面转义形态，而不是想当然。
- **子资源白名单遗漏**会让某些合法请求验签失败。白名单需对照 AWS 文档完整列出。
- **误把 v4 判成 v2**会破坏现有部署。第 1、3 条判定的前缀检查必须有专门测试。
- v2 默认关闭意味着 issue 报告者升级后仍需显式开启才能用 v2。这一点必须在关闭 issue 的评论中写清楚，否则会被认为"没修"。
