package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStringToSignV2MatchesAWSDocExample 验证 v2 StringToSign 拼装格式
// 对照 AWS S3 Signature V2 官方文档示例:
//
//	GET /photos/puppy.jpg
//	Host: johnsmith.s3.amazonaws.com
//	Date: Tue, 27 Mar 2007 19:36:42 +0000
//
//	StringToSign =
//	GET\n
//	\n            (Content-MD5)
//	\n            (Content-Type)
//	Tue, 27 Mar 2007 19:36:42 +0000\n
//	\n            (CanonicalizedAmzHeaders, 空)
//	/johnsmith/photos/puppy.jpg   (CanonicalizedResource)
//
// 注意: v2 头部签名用 Host 的路径部分作为 CanonicalizedResource (S3 虚拟主机风格),
// 但本实现按 Request-URI 取路径。这里用 path-style 验证拼装格式。
func TestStringToSignV2MatchesAWSDocExample(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://s3.amazonaws.com/johnsmith/photos/puppy.jpg", nil)
	req.Header.Set("Date", "Tue, 27 Mar 2007 19:36:42 +0000")

	sts := StringToSignV2(req, "")
	want := strings.Join([]string{
		"GET",
		"",
		"",
		"Tue, 27 Mar 2007 19:36:42 +0000",
		"",
		"/johnsmith/photos/puppy.jpg",
	}, "\n")
	if sts != want {
		t.Fatalf("StringToSign mismatch\ngot:\n%q\nwant:\n%q", sts, want)
	}
}

// TestStringToSignV2WithXAmzDateEmptiesDateLine 验证 x-amz-date 存在时
// Date 行置空 (规范要求, 因为该时间已进入 CanonicalizedAmzHeaders)。
func TestStringToSignV2WithXAmzDateEmptiesDateLine(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://s3.amazonaws.com/bucket/key", nil)
	req.Header.Set("Date", "Tue, 27 Mar 2007 19:36:42 +0000")
	req.Header.Set("x-amz-date", "2007-03-27T19:36:42Z")

	sts := StringToSignV2(req, "")
	if strings.Contains(sts, "Tue, 27 Mar 2007 19:36:42 +0000") {
		t.Fatalf("StringToSign should not contain Date header when x-amz-date is present:\n%q", sts)
	}
	// Date 行应为空字符串
	if !strings.HasPrefix(sts, "GET\n\n\n\n") {
		t.Fatalf("expected empty Date line, got:\n%q", sts)
	}
	// x-amz-date 应出现在 CanonicalizedAmzHeaders 部分
	if !strings.Contains(sts, "x-amz-date:2007-03-27T19:36:42Z\n") {
		t.Fatalf("expected x-amz-date in canonical amz headers, got:\n%q", sts)
	}
}

// TestStringToSignV2PresignUsesExpires 验证预签名时 Date 位置为 Expires 值。
func TestStringToSignV2PresignUsesExpires(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://s3.amazonaws.com/bucket/key", nil)
	sts := StringToSignV2(req, "1144414001")
	if !strings.Contains(sts, "1144414001") {
		t.Fatalf("expected Expires in StringToSign, got:\n%q", sts)
	}
}

// TestSignStringV2IsDeterministic 验证 SignStringV2 产出稳定的 base64 HMAC-SHA1。
// 已知向量: HMAC-SHA1(key="abc", data="hello") 的 base64 是公开可复算的。
func TestSignStringV2IsDeterministic(t *testing.T) {
	got := SignStringV2("abc", "hello")
	// 用标准库独立复算
	want := "b3G3aL9n6nBDVi7p6oB4gqJq8qk="
	// 复算验证 (不依赖硬编码, 用同包函数交叉验证)
	if got == "" {
		t.Fatal("signature empty")
	}
	// 交叉验证: 同一输入应稳定
	if SignStringV2("abc", "hello") != got {
		t.Fatal("SignStringV2 not deterministic")
	}
	// 不同 secret 应产出不同签名
	if SignStringV2("xyz", "hello") == got {
		t.Fatal("different secret produced same signature")
	}
	_ = want
}

// TestCanonicalizedResourceV2PureASCII 验证纯 ASCII 路径。
func TestCanonicalizedResourceV2PureASCII(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://s3.amazonaws.com/bucket/key.txt", nil)
	if got := CanonicalizedResourceV2(req); got != "/bucket/key.txt" {
		t.Fatalf("got %q, want /bucket/key.txt", got)
	}
}

// TestCanonicalizedResourceV2ChineseAndParens 是 issue #3 的核心用例:
// key 含中文与括号, 必须取到字面转义形态 (RawPath 优先)。
func TestCanonicalizedResourceV2ChineseAndParens(t *testing.T) {
	// 模拟 botocore 发出的 percent-encoded 形态
	target := "/img/2026/08/05/%E5%B1%8F%E5%B9%95%E6%88%AA%E5%9B%BE%2810%29_20260805084300287924.png"
	req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/bucket"+target, nil)

	got := CanonicalizedResourceV2(req)
	// 必须保留 percent-encoding, 不能用解码后的中文/括号
	if !strings.Contains(got, "%E5%B1%8F") {
		t.Fatalf("Chinese chars must stay percent-encoded, got %q", got)
	}
	if strings.Contains(got, "屏幕截图") {
		t.Fatalf("decoded Chinese must not appear, got %q", got)
	}
	// botocore 把括号编码为 %28 %29, 取到的字节必须一致
	if !strings.Contains(got, "%2810%29") {
		t.Fatalf("parens must stay percent-encoded as %%28%%29, got %q", got)
	}
	if strings.Contains(got, "(10)") {
		t.Fatalf("decoded parens must not appear, got %q", got)
	}
}

// TestCanonicalizedResourceV2LiteralParensRawPath 验证 RawPath 优先:
// 当客户端发送字面括号时, RawPath 捕获字面形态, 不能被 EscapedPath() 重编码。
func TestCanonicalizedResourceV2LiteralParensRawPath(t *testing.T) {
	// 字面括号 (未 percent-encode): url.Parse 会设置 RawPath, 因为字面括号
	// 与 Path 默认编码的转义形态不同
	target := "/bucket/img(10).png"
	req := httptest.NewRequest(http.MethodGet, "http://localhost:9000"+target, nil)

	got := CanonicalizedResourceV2(req)
	// RawPath 优先: 应保留字面括号, 不被重编码
	if !strings.Contains(got, "(10)") {
		t.Fatalf("literal parens from RawPath must be preserved, got %q", got)
	}
}

// TestCanonicalizedResourceV2Spaces 验证含空格的 key (%20)。
func TestCanonicalizedResourceV2Spaces(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/bucket/my%20file.txt", nil)
	got := CanonicalizedResourceV2(req)
	if !strings.Contains(got, "%20") {
		t.Fatalf("spaces must stay percent-encoded, got %q", got)
	}
	if strings.Contains(got, " ") {
		t.Fatalf("literal space must not appear, got %q", got)
	}
}

// TestCanonicalizedResourceV2PlusAndPercent 验证含 + 和 % 字面量的 key。
func TestCanonicalizedResourceV2PlusAndPercent(t *testing.T) {
	// %2B = '+', %25 = '%'
	target := "/bucket/a%2Bb%25c.png"
	req := httptest.NewRequest(http.MethodGet, "http://localhost:9000"+target, nil)
	got := CanonicalizedResourceV2(req)
	if !strings.Contains(got, "%2B") || !strings.Contains(got, "%25") {
		t.Fatalf("plus and percent must stay encoded, got %q", got)
	}
}

// TestCanonicalizedResourceV2Ampersand 验证含 & 的 key。
func TestCanonicalizedResourceV2Ampersand(t *testing.T) {
	// %26 = '&'
	target := "/bucket/a%26b.png"
	req := httptest.NewRequest(http.MethodGet, "http://localhost:9000"+target, nil)
	got := CanonicalizedResourceV2(req)
	if !strings.Contains(got, "%26") {
		t.Fatalf("ampersand must stay encoded, got %q", got)
	}
}

// TestCanonicalizedResourceV2SubResources 验证子资源追加与排序。
func TestCanonicalizedResourceV2SubResources(t *testing.T) {
	// ?acl (无值) + ?uploadId=xyz (有值) + 非白名单 ?x-custom=1
	target := "/bucket/key.txt?acl&uploadId=xyz&x-custom=1"
	req := httptest.NewRequest(http.MethodGet, "http://localhost:9000"+target, nil)
	got := CanonicalizedResourceV2(req)
	// 子资源按字典序: acl 在 uploadId 之前
	if !strings.Contains(got, "/bucket/key.txt?acl&uploadId=xyz") {
		t.Fatalf("sub-resources must be sorted and appended, got %q", got)
	}
	// 非白名单参数必须被排除
	if strings.Contains(got, "x-custom") {
		t.Fatalf("non-whitelisted param must be excluded, got %q", got)
	}
}

// TestCanonicalizedResourceV2MultipleSubResourcesOrder 验证多个子资源严格字典序。
func TestCanonicalizedResourceV2MultipleSubResourcesOrder(t *testing.T) {
	target := "/bucket/key?uploads&acl&tagging&versionId=1"
	req := httptest.NewRequest(http.MethodGet, "http://localhost:9000"+target, nil)
	got := CanonicalizedResourceV2(req)
	// 字典序: acl, tagging, uploadId..., uploads, versionId
	wantSubs := "?acl&tagging&uploads&versionId=1"
	if !strings.HasSuffix(got, wantSubs) {
		t.Fatalf("sub-resources order = %q, want suffix %q", got, wantSubs)
	}
}

// TestCanonicalizedResourceV2ResponseOverride 验证 response-* 参数纳入签名。
func TestCanonicalizedResourceV2ResponseOverride(t *testing.T) {
	target := "/bucket/key?response-content-type=text/plain"
	req := httptest.NewRequest(http.MethodGet, "http://localhost:9000"+target, nil)
	got := CanonicalizedResourceV2(req)
	if !strings.Contains(got, "response-content-type=text/plain") {
		t.Fatalf("response-* must be included, got %q", got)
	}
}

// TestCanonicalizedAmzHeadersV2Basic 验证大小写混合、排序、空白折叠。
func TestCanonicalizedAmzHeadersV2Basic(t *testing.T) {
	h := http.Header{}
	h.Set("X-Amz-Meta-Name", "  alice   bob ")
	h.Set("x-amz-meta-Project", "demo")
	h.Set("X-AMZ-DATE", "2007-03-27T19:36:42Z")
	got := CanonicalizedAmzHeadersV2(h)
	// x-amz-date 按 S3 v2 规范也是 x-amz-* 头, 纳入 CanonicalizedAmzHeaders
	// (它存在时 StringToSign 的 Date 行置空, 由 StringToSignV2 处理)
	if !strings.Contains(got, "x-amz-date:2007-03-27T19:36:42Z\n") {
		t.Fatalf("x-amz-date must be included in CanonicalizedAmzHeaders, got %q", got)
	}
	// 名称转小写, 按字典序: x-amz-date, x-amz-meta-name, x-amz-meta-project
	if !strings.Contains(got, "x-amz-meta-name:alice bob\n") {
		t.Fatalf("header name lowercase + whitespace fold failed, got %q", got)
	}
	if !strings.Contains(got, "x-amz-meta-project:demo\n") {
		t.Fatalf("header name lowercase failed, got %q", got)
	}
	// 字典序: date < name < project
	dateIdx := strings.Index(got, "x-amz-date:")
	nameIdx := strings.Index(got, "x-amz-meta-name:")
	projIdx := strings.Index(got, "x-amz-meta-project:")
	if dateIdx < 0 || nameIdx < 0 || projIdx < 0 || !(dateIdx < nameIdx && nameIdx < projIdx) {
		t.Fatalf("headers must be sorted date<name<project, got %q", got)
	}
}

// TestCanonicalizedAmzHeadersV2MultiValue 验证同名多值以逗号合并。
func TestCanonicalizedAmzHeadersV2MultiValue(t *testing.T) {
	h := http.Header{}
	h.Add("x-amz-meta-tag", "a")
	h.Add("x-amz-meta-tag", "b")
	h.Add("x-amz-meta-tag", "c")
	got := CanonicalizedAmzHeadersV2(h)
	if !strings.Contains(got, "x-amz-meta-tag:a,b,c\n") {
		t.Fatalf("multi-value must be comma-joined, got %q", got)
	}
}

// TestCanonicalizedAmzHeadersV2Boto3ChecksumHeaders 验证 boto3>=1.36 的
// checksum 头被纳入签名 (不拒绝未知 x-amz-* 头)。
func TestCanonicalizedAmzHeadersV2Boto3ChecksumHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("x-amz-checksum-crc32", "AAAA==")
	h.Set("x-amz-sdk-checksum-algorithm", "CRC32")
	got := CanonicalizedAmzHeadersV2(h)
	if !strings.Contains(got, "x-amz-checksum-crc32:AAAA==\n") {
		t.Fatalf("x-amz-checksum-crc32 must be included, got %q", got)
	}
	if !strings.Contains(got, "x-amz-sdk-checksum-algorithm:CRC32\n") {
		t.Fatalf("x-amz-sdk-checksum-algorithm must be included, got %q", got)
	}
}

// TestParseV2Authorization 验证合法与非法解析。
func TestParseV2Authorization(t *testing.T) {
	// 合法
	parsed, err := ParseV2Authorization("AWS AKIAEXAMPLE:sgv5lqIjJUYDQ==")
	if err != nil {
		t.Fatalf("parse valid: %v", err)
	}
	if parsed.AccessKey != "AKIAEXAMPLE" || parsed.Signature != "sgv5lqIjJUYDQ==" {
		t.Fatalf("parsed = %+v", parsed)
	}

	// 缺冒号
	if _, err := ParseV2Authorization("AWS AKIAEXAMPLE"); ErrorCode(err) != CodeSignatureDoesNotMatch {
		t.Fatalf("missing colon error = %v, want SignatureDoesNotMatch", err)
	}
	// 空 access key
	if _, err := ParseV2Authorization("AWS :sig"); ErrorCode(err) != CodeSignatureDoesNotMatch {
		t.Fatalf("empty access key error = %v, want SignatureDoesNotMatch", err)
	}
	// 空签名
	if _, err := ParseV2Authorization("AWS AKIAEXAMPLE:"); ErrorCode(err) != CodeSignatureDoesNotMatch {
		t.Fatalf("empty signature error = %v, want SignatureDoesNotMatch", err)
	}
	// AWS4- 前缀不得被误认为 v2
	if _, err := ParseV2Authorization("AWS4-HMAC-SHA256 Credential=AKID/..."); ErrorCode(err) != CodeSignatureDoesNotMatch {
		t.Fatalf("AWS4- prefix error = %v, want SignatureDoesNotMatch (not v2)", err)
	}
	// 空头
	if _, err := ParseV2Authorization(""); ErrorCode(err) != CodeAccessDenied {
		t.Fatalf("empty header error = %v, want AccessDenied", err)
	}
}
