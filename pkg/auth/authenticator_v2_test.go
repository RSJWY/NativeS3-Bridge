package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
)

// ---- v2 签名请求构造辅助 ----

// signV2Request 用 secret 对请求做 v2 头部签名并设置 Authorization 头。
// 调用方需先设置好 method/target/host/headers,再调用本函数完成签名。
func signV2Request(t *testing.T, req *http.Request, accessKey, secretKey string) *http.Request {
	t.Helper()
	sts := StringToSignV2(req, "")
	signature := SignStringV2(secretKey, sts)
	req.Header.Set("Authorization", "AWS "+accessKey+":"+signature)
	return req
}

// newV2TestCredential 建库 + credential + v2 authenticator(fixed now)。
func newV2TestAuthenticator(t *testing.T, accessKey, secretKey string, at time.Time) (*LocalSigV2Authenticator, db.Credential) {
	t.Helper()
	gdb := testDB(t)
	cred := db.Credential{AccessKey: accessKey, SecretKey: secretKey, Status: "enabled"}
	if err := gdb.Create(&cred).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	authenticator := NewLocalSigV2Authenticator(NewCredentialStore(gdb, time.Second))
	authenticator.now = func() time.Time { return at }
	return authenticator, cred
}

// v2ISO8601 返回 v2 规范要求的 x-amz-date ISO8601 形态。
func v2ISO8601(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// ---- 正确签名 / 错误码矩阵 ----

func TestLocalSigV2AuthenticatorVerify(t *testing.T) {
	issuedAt := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	authenticator, cred := newV2TestAuthenticator(t, "V2ACCESS", "V2SECRET", issuedAt)

	// 正确签名
	req := httptest.NewRequest(http.MethodPut, "http://localhost:9000/test-bucket/v2.txt", strings.NewReader("hello"))
	req.Header.Set("x-amz-date", v2ISO8601(issuedAt))
	req = signV2Request(t, req, cred.AccessKey, "V2SECRET")
	id, err := authenticator.Verify(req)
	if err != nil {
		t.Fatalf("verify v2 signed request: %v", err)
	}
	if id.CredentialID != cred.ID || id.AccessKey != cred.AccessKey {
		t.Fatalf("identity = %+v, want credential values", id)
	}

	// 错误 secret
	bad := httptest.NewRequest(http.MethodPut, "http://localhost:9000/test-bucket/v2.txt", strings.NewReader("hello"))
	bad.Header.Set("x-amz-date", v2ISO8601(issuedAt))
	bad = signV2Request(t, bad, cred.AccessKey, "WRONG")
	if _, err := authenticator.Verify(bad); ErrorCode(err) != CodeSignatureDoesNotMatch {
		t.Fatalf("wrong secret error = %v, want SignatureDoesNotMatch", err)
	}

	// 未知 access key
	unknown := httptest.NewRequest(http.MethodPut, "http://localhost:9000/test-bucket/v2.txt", strings.NewReader("hello"))
	unknown.Header.Set("x-amz-date", v2ISO8601(issuedAt))
	unknown = signV2Request(t, unknown, "NOPE", "V2SECRET")
	if _, err := authenticator.Verify(unknown); ErrorCode(err) != CodeInvalidAccessKeyID {
		t.Fatalf("unknown access key error = %v, want InvalidAccessKeyId", err)
	}

	// 时钟偏移超 15 分钟
	old := httptest.NewRequest(http.MethodPut, "http://localhost:9000/test-bucket/v2.txt", strings.NewReader("hello"))
	old.Header.Set("x-amz-date", v2ISO8601(issuedAt.Add(-16*time.Minute)))
	old = signV2Request(t, old, cred.AccessKey, "V2SECRET")
	if _, err := authenticator.Verify(old); ErrorCode(err) != CodeRequestTimeTooSkewed {
		t.Fatalf("skewed request error = %v, want RequestTimeTooSkewed", err)
	}

	// 禁用 credential
	gdb := authenticator.store.db
	if err := gdb.Model(&db.Credential{}).Where("id = ?", cred.ID).Update("status", "disabled").Error; err != nil {
		t.Fatalf("disable credential: %v", err)
	}
	authenticator.store.Invalidate(cred.AccessKey)
	disabled := httptest.NewRequest(http.MethodPut, "http://localhost:9000/test-bucket/v2.txt", strings.NewReader("hello"))
	disabled.Header.Set("x-amz-date", v2ISO8601(issuedAt))
	disabled = signV2Request(t, disabled, cred.AccessKey, "V2SECRET")
	if _, err := authenticator.Verify(disabled); ErrorCode(err) != CodeAccessDenied {
		t.Fatalf("disabled credential error = %v, want AccessDenied", err)
	}
}

// TestLocalSigV2AuthenticatorVerifyDateHeader 验证用 Date 头(RFC1123)而非 x-amz-date。
func TestLocalSigV2AuthenticatorVerifyDateHeader(t *testing.T) {
	issuedAt := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	authenticator, cred := newV2TestAuthenticator(t, "V2DATE", "V2SECRET", issuedAt)

	req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/test-bucket/v2.txt", nil)
	req.Header.Set("Date", issuedAt.UTC().Format(http.TimeFormat))
	req = signV2Request(t, req, cred.AccessKey, "V2SECRET")
	if _, err := authenticator.Verify(req); err != nil {
		t.Fatalf("verify v2 with Date header: %v", err)
	}
}

// TestLocalSigV2AuthenticatorVerifyXAmzDateEmptiesDateLine 验证 x-amz-date 存在时
// Date 行置空的变体:同时设置 Date 与 x-amz-date,签名应以 x-amz-date 为准且 Date 行置空。
func TestLocalSigV2AuthenticatorVerifyXAmzDateEmptiesDateLine(t *testing.T) {
	issuedAt := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	authenticator, cred := newV2TestAuthenticator(t, "V2XAMZ", "V2SECRET", issuedAt)

	req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/test-bucket/v2.txt", nil)
	// 同时设置 Date(不同值)与 x-amz-date:签名应以 x-amz-date 为准
	req.Header.Set("Date", "Tue, 27 Mar 2007 19:36:42 +0000")
	req.Header.Set("x-amz-date", v2ISO8601(issuedAt))
	req = signV2Request(t, req, cred.AccessKey, "V2SECRET")
	if _, err := authenticator.Verify(req); err != nil {
		t.Fatalf("verify v2 with x-amz-date (Date line emptied): %v", err)
	}
}

// ---- 中文与括号 key(issue #3 原始用例)----

// TestLocalSigV2AuthenticatorVerifyChineseAndParensKey 是 issue #3 的关键验收用例:
// key 含中文与括号,客户端按 percent-encoding 发送,v2 验签必须通过。
func TestLocalSigV2AuthenticatorVerifyChineseAndParensKey(t *testing.T) {
	issuedAt := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	authenticator, cred := newV2TestAuthenticator(t, "V2CN", "V2SECRET", issuedAt)

	// botocore 风格:中文与括号都 percent-encode
	target := "/test-bucket/img/2026/08/05/%E5%B1%8F%E5%B9%95%E6%88%AA%E5%9B%BE%2810%29_20260805084300287924.png"
	req := httptest.NewRequest(http.MethodPut, "http://localhost:9000"+target, strings.NewReader("data"))
	req.Header.Set("x-amz-date", v2ISO8601(issuedAt))
	req = signV2Request(t, req, cred.AccessKey, "V2SECRET")

	id, err := authenticator.Verify(req)
	if err != nil {
		t.Fatalf("verify v2 chinese+parens key: %v", err)
	}
	if id.AccessKey != cred.AccessKey {
		t.Fatalf("identity = %+v", id)
	}
}

// TestLocalSigV2AuthenticatorVerifySpecialCharsKey 验证空格/+/%%/& 字面量 key。
func TestLocalSigV2AuthenticatorVerifySpecialCharsKey(t *testing.T) {
	issuedAt := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	authenticator, cred := newV2TestAuthenticator(t, "V2SP", "V2SECRET", issuedAt)

	for _, target := range []string{
		"/test-bucket/my%20file.txt", // 空格
		"/test-bucket/a%2Bb.png",     // '+' 字面量
		"/test-bucket/a%25b.png",     // '%' 字面量
		"/test-bucket/a%26b.png",     // '&' 字面量
	} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:9000"+target, nil)
		req.Header.Set("x-amz-date", v2ISO8601(issuedAt))
		req = signV2Request(t, req, cred.AccessKey, "V2SECRET")
		if _, err := authenticator.Verify(req); err != nil {
			t.Fatalf("verify v2 special key %q: %v", target, err)
		}
	}
}

// ---- boto3 checksum 头兼容 ----

func TestLocalSigV2AuthenticatorVerifyBoto3ChecksumHeaders(t *testing.T) {
	issuedAt := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	authenticator, cred := newV2TestAuthenticator(t, "V2BOTO", "V2SECRET", issuedAt)

	req := httptest.NewRequest(http.MethodPut, "http://localhost:9000/test-bucket/v2.txt", strings.NewReader("hello"))
	req.Header.Set("x-amz-date", v2ISO8601(issuedAt))
	req.Header.Set("x-amz-checksum-crc32", "AAAA==")
	req.Header.Set("x-amz-sdk-checksum-algorithm", "CRC32")
	req = signV2Request(t, req, cred.AccessKey, "V2SECRET")
	if _, err := authenticator.Verify(req); err != nil {
		t.Fatalf("verify v2 with boto3 checksum headers: %v", err)
	}
}

// ---- 子资源与非白名单参数 ----

func TestLocalSigV2AuthenticatorVerifySubResources(t *testing.T) {
	issuedAt := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	authenticator, cred := newV2TestAuthenticator(t, "V2SUB", "V2SECRET", issuedAt)

	for _, target := range []string{
		"/test-bucket?acl",
		"/test-bucket/key.txt?acl",
		"/test-bucket?uploads",
		"/test-bucket/key.txt?uploadId=abc123&partNumber=1",
		"/test-bucket/key.txt?tagging",
	} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:9000"+target, nil)
		req.Header.Set("x-amz-date", v2ISO8601(issuedAt))
		req = signV2Request(t, req, cred.AccessKey, "V2SECRET")
		if _, err := authenticator.Verify(req); err != nil {
			t.Fatalf("verify v2 subresource %q: %v", target, err)
		}
	}
}

// TestLocalSigV2AuthenticatorVerifyNonWhitelistedParamIgnored 验证非白名单参数不参与签名:
// 同样的签名在带与不带 ?x-custom=1 时都应通过(因为该参数不进 StringToSign)。
func TestLocalSigV2AuthenticatorVerifyNonWhitelistedParamIgnored(t *testing.T) {
	issuedAt := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	authenticator, cred := newV2TestAuthenticator(t, "V2IGN", "V2SECRET", issuedAt)

	// 先签不带非白名单参数的请求
	base := httptest.NewRequest(http.MethodGet, "http://localhost:9000/test-bucket/key.txt", nil)
	base.Header.Set("x-amz-date", v2ISO8601(issuedAt))
	base = signV2Request(t, base, cred.AccessKey, "V2SECRET")
	sig := strings.TrimPrefix(base.Header.Get("Authorization"), "AWS "+cred.AccessKey+":")

	// 把同样的签名贴到带非白名单参数的请求上:仍应通过
	withExtra := httptest.NewRequest(http.MethodGet, "http://localhost:9000/test-bucket/key.txt?x-custom=1", nil)
	withExtra.Header.Set("x-amz-date", v2ISO8601(issuedAt))
	withExtra.Header.Set("Authorization", "AWS "+cred.AccessKey+":"+sig)
	if _, err := authenticator.Verify(withExtra); err != nil {
		t.Fatalf("non-whitelisted param must not affect signature: %v", err)
	}
}

// ---- v2 预签名 URL ----

func TestLocalSigV2AuthenticatorVerifyPresignedURL(t *testing.T) {
	issuedAt := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	gdb := testDB(t)
	cred := db.Credential{AccessKey: "V2PRESIGN", SecretKey: "V2SECRET", Status: "enabled"}
	if err := gdb.Create(&cred).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	authenticator := NewLocalSigV2Authenticator(NewCredentialStore(gdb, time.Second))

	expires := issuedAt.Add(60 * time.Second).Unix()
	authenticator.now = func() time.Time { return issuedAt.Add(30 * time.Second) }

	// 先构造不含 Signature 的请求算出签名
	expiresStr := fmt.Sprintf("%d", expires)
	probe := httptest.NewRequest(http.MethodGet, "http://localhost:9000/test-bucket/p.txt", nil)
	sts := StringToSignV2(probe, expiresStr)
	signature := SignStringV2(cred.SecretKey, sts)

	q := url.Values{}
	q.Set("AWSAccessKeyId", cred.AccessKey)
	q.Set("Expires", expiresStr)
	q.Set("Signature", signature)
	req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/test-bucket/p.txt?"+q.Encode(), nil)

	id, err := authenticator.Verify(req)
	if err != nil {
		t.Fatalf("verify v2 presigned: %v", err)
	}
	if id.AccessKey != cred.AccessKey {
		t.Fatalf("identity = %+v", id)
	}

	// 过期
	authenticator.now = func() time.Time { return issuedAt.Add(61 * time.Second) }
	if _, err := authenticator.Verify(req); ErrorCode(err) != CodeAccessDenied {
		t.Fatalf("expired presign error = %v, want AccessDenied", err)
	}
}
