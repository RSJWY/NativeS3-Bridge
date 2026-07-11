package server

import (
	"bytes"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"github.com/RSJWY/NativeS3-Bridge/pkg/quota"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
	"gorm.io/gorm"
)

type stubAuthenticator struct {
	verifyCalls int
	id          *auth.Identity
	err         error
}

func (s *stubAuthenticator) Verify(r *http.Request) (*auth.Identity, error) {
	s.verifyCalls++
	if s.err != nil {
		return nil, s.err
	}
	if s.id != nil {
		return s.id, nil
	}
	return &auth.Identity{CredentialID: 7, AccessKey: "signed"}, nil
}

func TestAuthAnonymousObjectReadMatrix(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		acl        string
		exists     bool
		wantStatus int
		wantAnon   bool
	}{
		{name: "public get object", method: http.MethodGet, path: "/bucket/key.txt", acl: storage.ACLPublicRead, exists: true, wantStatus: http.StatusNoContent, wantAnon: true},
		{name: "public head object", method: http.MethodHead, path: "/bucket/key.txt", acl: storage.ACLPublicRead, exists: true, wantStatus: http.StatusNoContent, wantAnon: true},
		{name: "private get object", method: http.MethodGet, path: "/bucket/key.txt", acl: storage.ACLPrivate, exists: true, wantStatus: http.StatusForbidden},
		{name: "missing bucket metadata", method: http.MethodGet, path: "/bucket/key.txt", acl: "", exists: false, wantStatus: http.StatusForbidden},
		{name: "list bucket", method: http.MethodGet, path: "/bucket", acl: storage.ACLPublicRead, exists: true, wantStatus: http.StatusForbidden},
		{name: "put object", method: http.MethodPut, path: "/bucket/key.txt", acl: storage.ACLPublicRead, exists: true, wantStatus: http.StatusForbidden},
		{name: "delete object", method: http.MethodDelete, path: "/bucket/key.txt", acl: storage.ACLPublicRead, exists: true, wantStatus: http.StatusForbidden},
		{name: "tagging subresource", method: http.MethodGet, path: "/bucket/key.txt?tagging", acl: storage.ACLPublicRead, exists: true, wantStatus: http.StatusForbidden},
		{name: "upload id subresource", method: http.MethodGet, path: "/bucket/key.txt?uploadId=abc", acl: storage.ACLPublicRead, exists: true, wantStatus: http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authenticator := &stubAuthenticator{}
			aclCalls := 0
			h := Auth(authenticator, func(bucket string) (string, bool, error) {
				aclCalls++
				if bucket != "bucket" {
					t.Fatalf("bucket = %q, want bucket", bucket)
				}
				return tt.acl, tt.exists, nil
			})(Quota(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id, ok := auth.IdentityFromContext(r.Context())
				if !ok || id == nil {
					t.Fatal("identity missing from context")
				}
				if tt.wantAnon && !auth.IsAnonymous(id) {
					t.Fatalf("identity = %#v, want anonymous", id)
				}
				w.WriteHeader(http.StatusNoContent)
			})))

			req := httptest.NewRequest(tt.method, tt.path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tt.wantStatus, rr.Body.String())
			}
			if authenticator.verifyCalls != 0 {
				t.Fatalf("Verify calls = %d, want 0", authenticator.verifyCalls)
			}
			if tt.wantAnon && aclCalls != 1 {
				t.Fatalf("ACL calls = %d, want 1", aclCalls)
			}
		})
	}
}

func TestAuthSignedRequestsBypassAnonymousACL(t *testing.T) {
	authenticator := &stubAuthenticator{}
	aclCalls := 0
	h := Auth(authenticator, func(bucket string) (string, bool, error) {
		aclCalls++
		return storage.ACLPrivate, true, nil
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFromContext(r.Context())
		if !ok || id == nil || auth.IsAnonymous(id) {
			t.Fatalf("identity = %#v, want signed identity", id)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPut, "/bucket/key.txt", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
	if authenticator.verifyCalls != 1 {
		t.Fatalf("Verify calls = %d, want 1", authenticator.verifyCalls)
	}
	if aclCalls != 0 {
		t.Fatalf("ACL calls = %d, want 0", aclCalls)
	}
}

func TestAuthBucketScopedCredentialEnforcesBucketBoundary(t *testing.T) {
	authenticator := &stubAuthenticator{id: &auth.Identity{CredentialID: 9, AccessKey: "scoped", Bucket: "alpha"}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := Auth(authenticator, func(bucket string) (string, bool, error) {
		return storage.ACLPrivate, true, nil
	})(handler)

	authHeader := "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc"

	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{name: "same bucket get", method: http.MethodGet, path: "/alpha/key.txt", wantStatus: http.StatusNoContent},
		{name: "same bucket put", method: http.MethodPut, path: "/alpha/key.txt", wantStatus: http.StatusNoContent},
		{name: "same bucket list", method: http.MethodGet, path: "/alpha", wantStatus: http.StatusNoContent},
		{name: "different bucket denied", method: http.MethodGet, path: "/beta/key.txt", wantStatus: http.StatusForbidden},
		{name: "different bucket put denied", method: http.MethodPut, path: "/beta/key.txt", wantStatus: http.StatusForbidden},
		{name: "service level denied", method: http.MethodGet, path: "/", wantStatus: http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authenticator.verifyCalls = 0
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", authHeader)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}
}

func TestAuthUnscopedCredentialAccessesAllBuckets(t *testing.T) {
	authenticator := &stubAuthenticator{id: &auth.Identity{CredentialID: 10, AccessKey: "admin", Bucket: ""}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := Auth(authenticator, func(bucket string) (string, bool, error) {
		return storage.ACLPrivate, true, nil
	})(handler)

	authHeader := "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc"

	for _, path := range []string{"/alpha/key.txt", "/beta/key.txt", "/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", authHeader)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("path %q status = %d, want %d; body=%s", path, rr.Code, http.StatusNoContent, rr.Body.String())
		}
	}
}

func TestQuotaSkipsCopyObjectRequestBodyLength(t *testing.T) {
	reached := false
	h := Quota(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPut, "/bucket/copy.txt", nil)
	req.ContentLength = -1
	req.Header.Set("x-amz-copy-source", "bucket/source.txt")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if !reached {
		t.Fatalf("copy object request did not reach handler; status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestLoggingAddsRequestIDHeaderAndLogField(t *testing.T) {
	var logBuf bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	h := Logging(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodHead, "/bucket/key.txt", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	requestID := rr.Header().Get("x-amz-request-id")
	assertGeneratedRequestID(t, requestID)
	logLine := logBuf.String()
	for _, want := range []string{
		`"request_id":"` + requestID + `"`,
		`"method":"HEAD"`,
		`"path":"/bucket/key.txt"`,
		`"elapsed":`,
	} {
		if !strings.Contains(logLine, want) {
			t.Fatalf("log entry = %s, want %s", logLine, want)
		}
	}
}

func TestRouterSuccessIncludesGeneratedRequestID(t *testing.T) {
	router, _, _ := newOpsTestRouter(t)
	req := headerSignedRequest(http.MethodPut, "/test-bucket/request-id.txt")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, withBody(req, "body"))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	assertGeneratedRequestID(t, rr.Header().Get("x-amz-request-id"))
}

func TestRouterErrorBodyUsesGeneratedRequestID(t *testing.T) {
	router, _, _ := newOpsTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/test-bucket/private.txt", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	requestID := rr.Header().Get("x-amz-request-id")
	assertGeneratedRequestID(t, requestID)
	var parsed struct {
		Code      string `xml:"Code"`
		RequestID string `xml:"RequestId"`
	}
	if err := xml.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("parse s3 error xml: %v; body=%s", err, rr.Body.String())
	}
	if parsed.Code != auth.CodeAccessDenied {
		t.Fatalf("error code = %q, want %q", parsed.Code, auth.CodeAccessDenied)
	}
	if parsed.RequestID != requestID {
		t.Fatalf("xml request id = %q, want response header %q", parsed.RequestID, requestID)
	}
}

func TestAnonRateLimitReturnsSlowDown(t *testing.T) {
	h := AnonRateLimit(config.RateLimitConfig{AnonymousRPS: 1, AnonymousBurst: 1})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/bucket/key.txt", nil)
	first.RemoteAddr = "192.0.2.1:1234"
	firstRR := httptest.NewRecorder()
	h.ServeHTTP(firstRR, first)
	if firstRR.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want 204", firstRR.Code)
	}

	second := httptest.NewRequest(http.MethodGet, "/bucket/key.txt", nil)
	second.RemoteAddr = "192.0.2.1:1234"
	secondRR := httptest.NewRecorder()
	h.ServeHTTP(secondRR, second)
	if secondRR.Code != http.StatusServiceUnavailable {
		t.Fatalf("second status = %d, want 503; body=%s", secondRR.Code, secondRR.Body.String())
	}
	if !strings.Contains(secondRR.Body.String(), "<Code>SlowDown</Code>") {
		t.Fatalf("body = %s, want SlowDown XML", secondRR.Body.String())
	}
}

func TestAnonRateLimitSignedAndNonObjectReadsBypass(t *testing.T) {
	calls := 0
	h := AnonRateLimit(config.RateLimitConfig{AnonymousRPS: 1, AnonymousBurst: 1})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, req := range []*http.Request{
		headerSignedRequest(http.MethodGet, "/bucket/key.txt"),
		headerSignedRequest(http.MethodGet, "/bucket/key.txt"),
		httptest.NewRequest(http.MethodGet, "/bucket", nil),
		httptest.NewRequest(http.MethodPut, "/bucket/key.txt", nil),
	} {
		req.RemoteAddr = "192.0.2.1:1234"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 for %s %s; body=%s", rr.Code, req.Method, req.URL.String(), rr.Body.String())
		}
	}
	if calls != 4 {
		t.Fatalf("calls = %d, want 4", calls)
	}
}

func TestAnonRateLimitDefaultIgnoresForwardedHeaders(t *testing.T) {
	h := AnonRateLimit(config.RateLimitConfig{AnonymousRPS: 1, AnonymousBurst: 1})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/bucket/key.txt", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		req.Header.Set("X-Forwarded-For", "203.0.113."+string(rune('1'+i)))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if i == 0 && rr.Code != http.StatusNoContent {
			t.Fatalf("first status = %d, want 204", rr.Code)
		}
		if i == 1 && rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("second status = %d, want 503 when forwarded headers are ignored", rr.Code)
		}
	}
}

func TestAnonRateLimitTrustForwardedUsesForwardedIP(t *testing.T) {
	h := AnonRateLimit(config.RateLimitConfig{AnonymousRPS: 1, AnonymousBurst: 1, TrustForwarded: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for i, xff := range []string{"203.0.113.1", "203.0.113.2"} {
		req := httptest.NewRequest(http.MethodGet, "/bucket/key.txt", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		req.Header.Set("X-Forwarded-For", xff)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("request %d status = %d, want 204", i, rr.Code)
		}
	}
}

func TestRouterAnonymousRateLimitAndQuotaSkip(t *testing.T) {
	gdb := newServerTestDB(t)
	dataRoot := t.TempDir()
	backend, err := storage.NewFileBackend(dataRoot)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	bucketStore := storage.NewBucketStore(gdb, dataRoot, storage.DefaultBucketACLCacheTTL)
	if err := bucketStore.Create("test-bucket"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := bucketStore.SetACL("test-bucket", storage.ACLPublicRead); err != nil {
		t.Fatalf("set acl: %v", err)
	}
	if _, err := backend.PutObject("test-bucket", "key.txt", bytes.NewBufferString("hello"), "text/plain"); err != nil {
		t.Fatalf("put object: %v", err)
	}

	commitCalls := 0
	router := NewRouter(backend, nil, bucketStore, &stubAuthenticator{}, func(uint, int64, quota.Op) error {
		commitCalls++
		return nil
	}, nil, config.RateLimitConfig{AnonymousRPS: 1, AnonymousBurst: 1})

	first := httptest.NewRequest(http.MethodGet, "/test-bucket/key.txt", nil)
	first.RemoteAddr = "192.0.2.1:1234"
	firstRR := httptest.NewRecorder()
	router.ServeHTTP(firstRR, first)
	if firstRR.Code != http.StatusOK || firstRR.Body.String() != "hello" {
		t.Fatalf("first status/body = %d/%q, want 200 hello", firstRR.Code, firstRR.Body.String())
	}
	if commitCalls != 0 {
		t.Fatalf("anonymous GET committed usage %d times, want 0", commitCalls)
	}

	second := httptest.NewRequest(http.MethodGet, "/test-bucket/key.txt", nil)
	second.RemoteAddr = "192.0.2.1:1234"
	secondRR := httptest.NewRecorder()
	router.ServeHTTP(secondRR, second)
	if secondRR.Code != http.StatusServiceUnavailable {
		t.Fatalf("second status = %d, want 503; body=%s", secondRR.Code, secondRR.Body.String())
	}
}

func TestRouterPutCreatesBucketMetadataForImplicitNativeBucket(t *testing.T) {
	gdb := newServerTestDB(t)
	dataRoot := t.TempDir()
	backend, err := storage.NewFileBackend(dataRoot)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	bucketStore := storage.NewBucketStore(gdb, dataRoot, storage.DefaultBucketACLCacheTTL)
	router := NewRouter(backend, nil, bucketStore, &stubAuthenticator{}, func(uint, int64, quota.Op) error { return nil }, nil, config.RateLimitConfig{})

	req := headerSignedRequest(http.MethodPut, "/implicit-bucket/key.txt")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, requestBody(req, "hello"))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	acl, exists, err := bucketStore.GetACL("implicit-bucket")
	if err != nil {
		t.Fatalf("get acl: %v", err)
	}
	if !exists || acl != storage.ACLPrivate {
		t.Fatalf("acl = %q exists=%v, want private true", acl, exists)
	}
	if _, err := backend.HeadObject("implicit-bucket", "key.txt"); err != nil {
		t.Fatalf("object not stored: %v", err)
	}
}

func TestRouterMultipartCreateCreatesBucketMetadataForImplicitNativeBucket(t *testing.T) {
	gdb := newServerTestDB(t)
	dataRoot := t.TempDir()
	backend, err := storage.NewFileBackend(dataRoot)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	multipartStore, err := storage.NewMultipartStore(dataRoot, filepath.Join(dataRoot, ".multipart"), storage.DefaultMetadataSuffix)
	if err != nil {
		t.Fatalf("new multipart store: %v", err)
	}
	bucketStore := storage.NewBucketStore(gdb, dataRoot, storage.DefaultBucketACLCacheTTL)
	router := NewRouter(backend, multipartStore, bucketStore, &stubAuthenticator{}, func(uint, int64, quota.Op) error { return nil }, nil, config.RateLimitConfig{})

	req := headerSignedRequest(http.MethodPost, "/implicit-multipart/big.bin?uploads")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	acl, exists, err := bucketStore.GetACL("implicit-multipart")
	if err != nil {
		t.Fatalf("get acl: %v", err)
	}
	if !exists || acl != storage.ACLPrivate {
		t.Fatalf("acl = %q exists=%v, want private true", acl, exists)
	}
}

func headerSignedRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
	return req
}

func requestBody(req *http.Request, body string) *http.Request {
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	return req
}

func newServerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := dbpkg.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := dbpkg.Migrate(gdb); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return gdb
}

func assertGeneratedRequestID(t *testing.T, requestID string) {
	t.Helper()
	if !strings.HasPrefix(requestID, "req-") || len(requestID) != len("req-0000000000000000-00000000") {
		t.Fatalf("request id = %q, want generated req-<time>-<seq> format", requestID)
	}
	for _, r := range strings.TrimPrefix(requestID, "req-") {
		if (r >= 'a' && r <= 'f') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		t.Fatalf("request id = %q contains unsafe character %q", requestID, r)
	}
}
