package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
)

// recordAuthenticator 记录自己被调用了多少次,用于断言分派走向。
type recordAuthenticator struct {
	calls int
	id    *auth_IdentityAlias
	err   error
}

// 用别名避免与本包 Identity 冲突的导入噪音。
type auth_IdentityAlias = Identity

func (r *recordAuthenticator) Verify(*http.Request) (*Identity, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.id, nil
}

func TestMultiSchemeAuthenticatorDispatch(t *testing.T) {
	v4 := &recordAuthenticator{id: &Identity{CredentialID: 1, AccessKey: "v4"}}
	v2 := &recordAuthenticator{id: &Identity{CredentialID: 2, AccessKey: "v2"}}
	multi := NewMultiSchemeAuthenticator(v4, v2)

	tests := []struct {
		name        string
		build       func() *http.Request
		wantV4Calls int
		wantV2Calls int
	}{
		{
			name: "v4 header authorization",
			build: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/b/k", nil)
				req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AK/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
				return req
			},
			wantV4Calls: 1,
		},
		{
			name: "v4 presign query",
			build: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "http://localhost:9000/b/k?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AK/20260101/us-east-1/s3/aws4_request&X-Amz-Date=20260101T000000Z&X-Amz-Expires=60&X-Amz-SignedHeaders=host&X-Amz-Signature=abc", nil)
			},
			wantV4Calls: 1,
		},
		{
			name: "v2 header authorization",
			build: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/b/k", nil)
				req.Header.Set("Authorization", "AWS AKIAEXAMPLE:signature==")
				return req
			},
			wantV2Calls: 1,
		},
		{
			name: "v2 presign query",
			build: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "http://localhost:9000/b/k?AWSAccessKeyId=AK&Expires=9999999999&Signature=sig==", nil)
			},
			wantV2Calls: 1,
		},
		{
			name: "no credentials falls through to v4",
			build: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "http://localhost:9000/b/k", nil)
			},
			wantV4Calls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v4.calls = 0
			v2.calls = 0
			_, _ = multi.Verify(tt.build())
			if v4.calls != tt.wantV4Calls || v2.calls != tt.wantV2Calls {
				t.Fatalf("v4 calls=%d v2 calls=%d, want v4=%d v2=%d", v4.calls, v2.calls, tt.wantV4Calls, tt.wantV2Calls)
			}
		})
	}
}

// TestMultiSchemeAuthenticatorV4NotMisjudgedAsV2 是关键回归守卫:
// "AWS4-HMAC-SHA256" 本身也以 "AWS" 开头,绝不能被分派到 v2。
func TestMultiSchemeAuthenticatorV4NotMisjudgedAsV2(t *testing.T) {
	v4 := &recordAuthenticator{id: &Identity{CredentialID: 1, AccessKey: "v4"}}
	v2 := &recordAuthenticator{id: &Identity{CredentialID: 2, AccessKey: "v2"}}
	multi := NewMultiSchemeAuthenticator(v4, v2)

	req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/b/k", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AK/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
	_, _ = multi.Verify(req)
	if v2.calls != 0 {
		t.Fatalf("v4 request must not be dispatched to v2; v2 calls=%d", v2.calls)
	}
	if v4.calls != 1 {
		t.Fatalf("v4 request must reach v4 authenticator; v4 calls=%d", v4.calls)
	}
}

// TestMultiSchemeAuthenticatorV2DisabledReturnsInvalidRequest 验证 v2 关闭时
// 返回 CodeInvalidRequest 而非 SignatureDoesNotMatch(红线 3)。
func TestMultiSchemeAuthenticatorV2DisabledReturnsInvalidRequest(t *testing.T) {
	v4 := &recordAuthenticator{id: &Identity{CredentialID: 1, AccessKey: "v4"}}
	multi := NewMultiSchemeAuthenticator(v4, nil)

	// v2 头部形态
	headerReq := httptest.NewRequest(http.MethodGet, "http://localhost:9000/b/k", nil)
	headerReq.Header.Set("Authorization", "AWS AKIAEXAMPLE:signature==")
	if _, err := multi.Verify(headerReq); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("disabled v2 header error = %v, want InvalidRequest", err)
	}

	// v2 预签名形态
	presignReq := httptest.NewRequest(http.MethodGet, "http://localhost:9000/b/k?AWSAccessKeyId=AK&Expires=9999999999&Signature=sig==", nil)
	if _, err := multi.Verify(presignReq); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("disabled v2 presign error = %v, want InvalidRequest", err)
	}
}

// TestMultiSchemeAuthenticatorV4ParityWithDirect 验证 v4 请求在 MultiScheme 下
// 与直接用 v4 authenticator 行为完全一致(同样的请求,同样的结果/错误码)。
func TestMultiSchemeAuthenticatorV4ParityWithDirect(t *testing.T) {
	gdb := testDB(t)
	cred := db.Credential{AccessKey: "PARITY", SecretKey: "SECRET", Status: "enabled"}
	if err := gdb.Create(&cred).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	issuedAt := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	store := NewCredentialStore(gdb, time.Second)
	direct := NewLocalSigV4Authenticator(store, "us-east-1")
	direct.now = func() time.Time { return issuedAt }

	v2 := &recordAuthenticator{}
	multi := NewMultiSchemeAuthenticator(direct, v2)

	// 一个合法 v4 签名请求:两次构造同样的请求,分别走 direct 与 multi
	build := func() *http.Request {
		return signedHeaderTestRequest(t, http.MethodGet, "http://localhost:9000/test-bucket/p.txt", cred.AccessKey, cred.SecretKey, issuedAt, "us-east-1", nil)
	}
	directID, directErr := direct.Verify(build())
	multiID, multiErr := multi.Verify(build())
	if (directErr == nil) != (multiErr == nil) {
		t.Fatalf("parity error mismatch: direct=%v multi=%v", directErr, multiErr)
	}
	if directErr == nil && (directID.AccessKey != multiID.AccessKey || directID.CredentialID != multiID.CredentialID) {
		t.Fatalf("parity identity mismatch: direct=%+v multi=%+v", directID, multiID)
	}
	if v2.calls != 0 {
		t.Fatalf("v4 request must not reach v2; v2 calls=%d", v2.calls)
	}

	// 一个必然失败的 v4 请求(签名错):两边错误码必须一致
	badBuild := func() *http.Request {
		req := signedHeaderTestRequest(t, http.MethodGet, "http://localhost:9000/test-bucket/p.txt", cred.AccessKey, "WRONG", issuedAt, "us-east-1", nil)
		return req
	}
	_, directBadErr := direct.Verify(badBuild())
	_, multiBadErr := multi.Verify(badBuild())
	if ErrorCode(directBadErr) != ErrorCode(multiBadErr) {
		t.Fatalf("parity error code mismatch: direct=%v multi=%v", directBadErr, multiBadErr)
	}
	if !strings.Contains(ErrorCode(multiBadErr), "SignatureDoesNotMatch") {
		t.Fatalf("want SignatureDoesNotMatch, got %v", multiBadErr)
	}
}
