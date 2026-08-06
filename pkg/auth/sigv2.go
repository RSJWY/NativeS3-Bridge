package auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"sort"
	"strings"
)

// AlgorithmV2 是 S3 Signature Version 2 的 Authorization 头前缀。
// v2 头部签名形如 "AWS <AccessKey>:<Base64Signature>"。
const AlgorithmV2 = "AWS"

// v2SubResources 是参与 CanonicalizedResource 的 S3 legacy 子资源白名单
// (AWS S3 开发者文档)。非白名单查询参数不进入 v2 签名。
var v2SubResources = map[string]bool{
	"acl":            true,
	"lifecycle":      true,
	"location":       true,
	"logging":        true,
	"notification":   true,
	"partNumber":     true,
	"policy":         true,
	"requestPayment": true,
	"torrent":        true,
	"uploadId":       true,
	"uploads":        true,
	"versionId":      true,
	"versioning":     true,
	"versions":       true,
	"website":        true,
	"cors":           true,
	"delete":         true,
	"tagging":        true,
}

// v2ResponseOverridePrefixes 是 response-* 覆盖响应头的查询参数前缀,
// 它们按规范也参与 CanonicalizedResource。
var v2ResponseOverridePrefixes = []string{"response-"}

// ParsedV2Authorization 是 "AWS <AccessKey>:<Base64Signature>" 的解析结果。
type ParsedV2Authorization struct {
	AccessKey string
	Signature string
}

// ParseV2Authorization 解析 v2 头部签名。
// 格式: "AWS <AccessKey>:<Base64Signature>"。
// 缺前缀/缺冒号/空 access key/空签名 -> CodeSignatureDoesNotMatch。
// 注意: "AWS4-HMAC-SHA256 ..." 不应被误判为 v2; 调用方需先用 v4 前缀拦截,
// 这里也用带空格的 "AWS " 前缀做双保险 (AWS4-HMAC-SHA256 不含空格紧跟 AWS)。
func ParseV2Authorization(header string) (ParsedV2Authorization, error) {
	if header == "" {
		return ParsedV2Authorization{}, NewError(CodeAccessDenied)
	}
	// 必须是 "AWS " 带空格; "AWS4-HMAC-SHA256" 不含此空格前缀, 不会误匹配。
	if !strings.HasPrefix(header, AlgorithmV2+" ") {
		return ParsedV2Authorization{}, NewError(CodeSignatureDoesNotMatch)
	}
	rest := strings.TrimPrefix(header, AlgorithmV2+" ")
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return ParsedV2Authorization{}, NewError(CodeSignatureDoesNotMatch)
	}
	accessKey := rest[:colon]
	signature := rest[colon+1:]
	if accessKey == "" || signature == "" {
		return ParsedV2Authorization{}, NewError(CodeSignatureDoesNotMatch)
	}
	return ParsedV2Authorization{AccessKey: accessKey, Signature: signature}, nil
}

// StringToSignV2 按 S3 legacy 规范拼装 v2 待签字符串:
//
//	HTTP-Verb + "\n" +
//	Content-MD5 + "\n" +
//	Content-Type + "\n" +
//	Date + "\n" +
//	CanonicalizedAmzHeaders +
//	CanonicalizedResource
//
// expires 非空时用于预签名 URL, 替换 Date 位置为 Expires 的值。
// 当请求带 x-amz-date 头时, Date 行必须置空 (规范要求), 因为该时间已进入
// CanonicalizedAmzHeaders。
func StringToSignV2(r *http.Request, expires string) string {
	dateLine := ""
	if expires != "" {
		dateLine = expires
	} else if r.Header.Get("x-amz-date") == "" {
		dateLine = r.Header.Get("Date")
	}
	return strings.Join([]string{
		r.Method,
		r.Header.Get("Content-MD5"),
		r.Header.Get("Content-Type"),
		dateLine,
		CanonicalizedAmzHeadersV2(r.Header),
		CanonicalizedResourceV2(r),
	}, "\n")
}

// CanonicalizedAmzHeadersV2 收集所有 x-amz-* 头:
//   - 名称转小写, 按名称字典序排序
//   - 同名多值以逗号合并
//   - 值用 normalizeHeaderValue (复用 sigv4.go) 折叠内部空白
//   - 每条以 "\n" 结尾
//
// 未知 x-amz-* 头 (例如 boto3>=1.36 的 x-amz-checksum-crc32,
// x-amz-sdk-checksum-algorithm) 按规范纳入签名, 不拒绝。
// x-amz-date 本身也是 x-amz-* 头, 按规范纳入 CanonicalizedAmzHeaders;
// 当它存在时, StringToSign 的 Date 行置空 (在 StringToSignV2 中处理),
// 二者是独立的规则。
func CanonicalizedAmzHeadersV2(h http.Header) string {
	keys := make([]string, 0, len(h))
	for name := range h {
		if !strings.HasPrefix(strings.ToLower(name), "x-amz-") {
			continue
		}
		keys = append(keys, strings.ToLower(name))
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, name := range keys {
		values := h.Values(name)
		trimmed := make([]string, 0, len(values))
		for _, v := range values {
			trimmed = append(trimmed, normalizeHeaderValue(v))
		}
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(strings.Join(trimmed, ","))
		b.WriteByte('\n')
	}
	return b.String()
}

// CanonicalizedResourceV2 构造 v2 签名的 CanonicalizedResource:
//   - 路径取值优先 r.URL.RawPath, 为空回落 r.URL.EscapedPath() (绝不用已解码的 r.URL.Path)
//   - 空路径用 "/"
//   - 追加白名单子资源, 按名字字典序; 有值写 "name=value", 无值只写 "name"
//
// 必须用原始转义形态, 否则含中文/括号/空格的 key 会因编码往返不一致而验签失败
// (GitHub issue #3 的核心关切)。RawPath 优先确保拿到客户端字面发送的字节,
// 因为 EscapedPath() 在 RawPath 为空时会回落到 Path 的重新编码, 与客户端
// 发出的转义可能不一致 (Go 与 botocore 对 sub-delims 的转义策略不同)。
func CanonicalizedResourceV2(r *http.Request) string {
	path := r.URL.RawPath
	if path == "" {
		path = r.URL.EscapedPath()
	}
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	var sub []string
	query := r.URL.Query()
	for name := range query {
		if isV2SubResource(name) {
			for _, val := range query[name] {
				if val == "" {
					sub = append(sub, name)
				} else {
					sub = append(sub, name+"="+val)
				}
			}
		}
	}
	sort.Strings(sub)
	if len(sub) == 0 {
		return path
	}
	return path + "?" + strings.Join(sub, "&")
}

// isV2SubResource 判断查询参数是否参与 v2 CanonicalizedResource。
// response-* 前缀 (response-content-type 等) 也算。
func isV2SubResource(name string) bool {
	if v2SubResources[name] {
		return true
	}
	for _, prefix := range v2ResponseOverridePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// SignStringV2 计算 v2 签名: Base64(HMAC-SHA1(SecretKey, StringToSign))。
func SignStringV2(secret, stringToSign string) string {
	h := hmac.New(sha1.New, []byte(secret))
	_, _ = h.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// constantTimeBase64Equal 恒定时间比较两个 base64 签名。
// 解码失败返回 false (与 v4 的 ConstantTimeSignatureEqual 同等强度)。
func constantTimeBase64Equal(expected, actual string) bool {
	expectedBytes, err := base64.StdEncoding.DecodeString(expected)
	if err != nil {
		return false
	}
	actualBytes, err := base64.StdEncoding.DecodeString(actual)
	if err != nil {
		return false
	}
	return hmac.Equal(expectedBytes, actualBytes)
}
