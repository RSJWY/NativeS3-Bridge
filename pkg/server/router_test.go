package server

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

type fixedAuthenticator struct {
	id *auth.Identity
}

func (a fixedAuthenticator) Verify(*http.Request) (*auth.Identity, error) {
	return a.id, nil
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

func TestQuotaRejectsBodyLargerThanDeclaredLength(t *testing.T) {
	reached := false
	h := Quota(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, err := io.ReadAll(r.Body)
		if !errors.Is(err, quota.ErrQuotaExceeded) {
			t.Fatalf("read body error = %v, want ErrQuotaExceeded", err)
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	req := httptest.NewRequest(http.MethodPut, "/bucket/key.txt", strings.NewReader("larger"))
	req.ContentLength = 3
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{CredentialID: 1, QuotaBytes: 100}))
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if !reached || rr.Code != http.StatusForbidden {
		t.Fatalf("reached=%v status=%d, want true/403", reached, rr.Code)
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
		`"status":204`,
		`"elapsed":`,
	} {
		if !strings.Contains(logLine, want) {
			t.Fatalf("log entry = %s, want %s", logLine, want)
		}
	}
}

func TestLoggingRecordsNetHTTPStatusSemantics(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantStatus int
		wantBody   string
	}{
		{
			name: "explicit status",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "implicit ok",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("ok"))
			},
			wantStatus: http.StatusOK,
			wantBody:   "ok",
		},
		{
			name: "first header wins",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "write locks implicit ok",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("ok"))
				w.WriteHeader(http.StatusNotFound)
			},
			wantStatus: http.StatusOK,
			wantBody:   "ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logBuf := captureDefaultLogger(t)
			rr := httptest.NewRecorder()
			Logging(tt.handler).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/bucket/key.txt", nil))

			if rr.Code != tt.wantStatus {
				t.Fatalf("response status = %d, want %d", rr.Code, tt.wantStatus)
			}
			if rr.Body.String() != tt.wantBody {
				t.Fatalf("response body = %q, want %q", rr.Body.String(), tt.wantBody)
			}
			entry := requireJSONLogEntry(t, logBuf, "s3 request")
			if entry["status"] != float64(tt.wantStatus) {
				t.Fatalf("logged status = %#v, want %d; log=%s", entry["status"], tt.wantStatus, logBuf.String())
			}
		})
	}

	rr := httptest.NewRecorder()
	w := &statusResponseWriter{ResponseWriter: rr, status: http.StatusOK}
	if w.Unwrap() != rr {
		t.Fatal("Unwrap did not return the underlying response writer")
	}
}

func TestAuthDeniedLogsDiagnosticFields(t *testing.T) {
	sensitiveAuthorization := "AUTHORIZATION-SENTINEL secret-sentinel"
	tests := []struct {
		name       string
		method     string
		target     string
		signed     bool
		id         *auth.Identity
		verifyErr  error
		acl        string
		exists     bool
		aclErr     error
		nilLookup  bool
		wantStatus int
		wantReason string
		wantCode   string
		wantFields map[string]any
	}{
		{
			name:   "verify failed",
			method: http.MethodGet, target: "/bucket/key.txt?X-Amz-Signature=query-secret", signed: true,
			verifyErr: auth.NewError(auth.CodeSignatureDoesNotMatch), wantStatus: http.StatusForbidden,
			wantReason: "verify_failed", wantCode: auth.CodeSignatureDoesNotMatch,
		},
		{
			name:   "bound bucket mismatch",
			method: http.MethodGet, target: "/other/key.txt", signed: true,
			id: &auth.Identity{CredentialID: 9, AccessKey: "scoped", Bucket: "bound"}, wantStatus: http.StatusForbidden,
			wantReason: "bucket_mismatch", wantCode: auth.CodeAccessDenied,
			wantFields: map[string]any{"bound_bucket": "bound", "request_bucket": "other"},
		},
		{
			name:   "credentials required",
			method: http.MethodPut, target: "/bucket/key.txt", wantStatus: http.StatusForbidden,
			wantReason: "credentials_required", wantCode: auth.CodeAccessDenied,
		},
		{
			name:   "acl lookup unavailable",
			method: http.MethodGet, target: "/bucket/key.txt", nilLookup: true, wantStatus: http.StatusForbidden,
			wantReason: "acl_lookup_unavailable", wantCode: auth.CodeAccessDenied,
			wantFields: map[string]any{"bucket": "bucket"},
		},
		{
			name:   "acl lookup failed",
			method: http.MethodGet, target: "/bucket/key.txt", aclErr: errors.New("acl unavailable"), wantStatus: http.StatusInternalServerError,
			wantReason: "acl_lookup_failed", wantCode: "InternalError",
			wantFields: map[string]any{"bucket": "bucket", "error": "acl unavailable"},
		},
		{
			name:   "private bucket",
			method: http.MethodHead, target: "/bucket/key.txt", acl: storage.ACLPrivate, exists: true, wantStatus: http.StatusForbidden,
			wantReason: "anonymous_not_allowed", wantCode: auth.CodeAccessDenied,
			wantFields: map[string]any{"bucket": "bucket", "bucket_exists": true, "acl": storage.ACLPrivate},
		},
		{
			name:   "missing bucket metadata",
			method: http.MethodGet, target: "/missing/key.txt", wantStatus: http.StatusForbidden,
			wantReason: "anonymous_not_allowed", wantCode: auth.CodeAccessDenied,
			wantFields: map[string]any{"bucket": "missing", "bucket_exists": false, "acl": ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logBuf := captureDefaultLogger(t)
			authenticator := &stubAuthenticator{id: tt.id, err: tt.verifyErr}
			var aclLookup ACLLookup
			if !tt.nilLookup {
				aclLookup = func(string) (string, bool, error) {
					return tt.acl, tt.exists, tt.aclErr
				}
			}
			h := Logging(Auth(authenticator, aclLookup)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})))
			req := httptest.NewRequest(tt.method, tt.target, nil)
			if tt.signed {
				req.Header.Set("Authorization", sensitiveAuthorization)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tt.wantStatus, rr.Body.String())
			}
			requestID := rr.Header().Get("x-amz-request-id")
			assertGeneratedRequestID(t, requestID)
			denied := requireJSONLogEntry(t, logBuf, "s3 auth denied")
			for key, want := range map[string]any{
				"request_id": requestID,
				"method":     tt.method,
				"path":       req.URL.Path,
				"reason":     tt.wantReason,
				"code":       tt.wantCode,
			} {
				if denied[key] != want {
					t.Fatalf("auth log %s = %#v, want %#v; log=%s", key, denied[key], want, logBuf.String())
				}
			}
			for key, want := range tt.wantFields {
				if denied[key] != want {
					t.Fatalf("auth log %s = %#v, want %#v; log=%s", key, denied[key], want, logBuf.String())
				}
			}
			access := requireJSONLogEntry(t, logBuf, "s3 request")
			if access["request_id"] != requestID || access["status"] != float64(tt.wantStatus) {
				t.Fatalf("access log does not correlate status/request id: %v", access)
			}
			if tt.name == "verify failed" {
				for _, secret := range []string{sensitiveAuthorization, "secret-sentinel", "query-secret", "Authorization", "X-Amz-Signature"} {
					if strings.Contains(logBuf.String(), secret) {
						t.Fatalf("log leaked sensitive value %q: %s", secret, logBuf.String())
					}
				}
			}
		})
	}
}

func TestRouterSignedHeadMissingKeyLogsNotFound(t *testing.T) {
	logBuf := captureDefaultLogger(t)
	router, _, _ := newOpsTestRouter(t)
	req := headerSignedRequest(http.MethodHead, "/test-bucket/missing")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "<Code>NoSuchKey</Code>") {
		t.Fatalf("body = %s, want NoSuchKey XML", rr.Body.String())
	}
	requestID := rr.Header().Get("x-amz-request-id")
	access := requireJSONLogEntry(t, logBuf, "s3 request")
	if access["request_id"] != requestID || access["status"] != float64(http.StatusNotFound) {
		t.Fatalf("access log = %v, want request_id=%q status=404", access, requestID)
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

func TestAnonRateLimitTrustForwardedIgnoresSpoofedPrefix(t *testing.T) {
	h := AnonRateLimit(config.RateLimitConfig{AnonymousRPS: 1, AnonymousBurst: 1, TrustForwarded: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for i, xff := range []string{"198.51.100.1, 203.0.113.10", "198.51.100.2, 203.0.113.10"} {
		req := httptest.NewRequest(http.MethodGet, "/bucket/key.txt", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		req.Header.Set("X-Forwarded-For", xff)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		want := http.StatusNoContent
		if i == 1 {
			want = http.StatusServiceUnavailable
		}
		if rr.Code != want {
			t.Fatalf("request %d status = %d, want %d", i, rr.Code, want)
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

func TestManagedRouterUsesDeclarationsAndRejectsDirectBucketMutation(t *testing.T) {
	gdb := newServerTestDB(t)
	credential := dbpkg.Credential{AccessKey: "managed", SecretKey: "secret", Status: "enabled"}
	if err := gdb.Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	dataRoot := t.TempDir()
	backend, err := storage.NewFileBackend(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	bucketStore := storage.NewBucketStore(gdb, dataRoot, storage.DefaultBucketACLCacheTTL)
	if err := bucketStore.Create("managed-bucket"); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.PutObject("managed-bucket", "visible.txt", strings.NewReader("visible"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	retainedDir := filepath.Join(dataRoot, "retained-bucket")
	if err := os.MkdirAll(retainedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	retainedObject := filepath.Join(retainedDir, "hidden.txt")
	if err := os.WriteFile(retainedObject, []byte("hidden"), 0o644); err != nil {
		t.Fatal(err)
	}
	authenticator := fixedAuthenticator{id: &auth.Identity{CredentialID: credential.ID, AccessKey: credential.AccessKey}}
	router := NewManagedRouterWithQuotaManager(backend, nil, bucketStore, authenticator, quota.NewManager(gdb), nil, NewRateLimitController(config.RateLimitConfig{}))

	list := headerSignedRequest(http.MethodGet, "/")
	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, list)
	if listRR.Code != http.StatusOK || !strings.Contains(listRR.Body.String(), "managed-bucket") || strings.Contains(listRR.Body.String(), "retained-bucket") {
		t.Fatalf("managed list = %d %s", listRR.Code, listRR.Body.String())
	}

	visible := headerSignedRequest(http.MethodGet, "/managed-bucket/visible.txt")
	visibleRR := httptest.NewRecorder()
	router.ServeHTTP(visibleRR, visible)
	if visibleRR.Code != http.StatusOK || visibleRR.Body.String() != "visible" {
		t.Fatalf("managed object = %d %q", visibleRR.Code, visibleRR.Body.String())
	}

	hidden := headerSignedRequest(http.MethodGet, "/retained-bucket/hidden.txt")
	hiddenRR := httptest.NewRecorder()
	router.ServeHTTP(hiddenRR, hidden)
	if hiddenRR.Code != http.StatusNotFound || !strings.Contains(hiddenRR.Body.String(), "NoSuchBucket") {
		t.Fatalf("retained object = %d %s", hiddenRR.Code, hiddenRR.Body.String())
	}

	copyRequest := headerSignedRequest(http.MethodPut, "/managed-bucket/copied.txt")
	copyRequest.Header.Set("x-amz-copy-source", "/retained-bucket/hidden.txt")
	copyRR := httptest.NewRecorder()
	router.ServeHTTP(copyRR, copyRequest)
	if copyRR.Code != http.StatusNotFound || !strings.Contains(copyRR.Body.String(), "NoSuchBucket") {
		t.Fatalf("retained copy source = %d %s", copyRR.Code, copyRR.Body.String())
	}
	if _, err := backend.HeadObject("managed-bucket", "copied.txt"); !errors.Is(err, storage.ErrNoSuchKey) {
		t.Fatalf("copy from retained source created destination: %v", err)
	}

	create := headerSignedRequest(http.MethodPut, "/direct-bucket")
	createRR := httptest.NewRecorder()
	router.ServeHTTP(createRR, create)
	if createRR.Code != http.StatusForbidden || !strings.Contains(createRR.Body.String(), "AccessDenied") {
		t.Fatalf("direct create = %d %s", createRR.Code, createRR.Body.String())
	}
	deleteRequest := headerSignedRequest(http.MethodDelete, "/managed-bucket")
	deleteRR := httptest.NewRecorder()
	router.ServeHTTP(deleteRR, deleteRequest)
	if deleteRR.Code != http.StatusForbidden || !strings.Contains(deleteRR.Body.String(), "AccessDenied") {
		t.Fatalf("direct delete = %d %s", deleteRR.Code, deleteRR.Body.String())
	}
	if data, err := os.ReadFile(retainedObject); err != nil || string(data) != "hidden" {
		t.Fatalf("retained bytes = %q err=%v", data, err)
	}
}

func TestManagedRouterRejectsCopySourceOutsideCredentialBucketScope(t *testing.T) {
	gdb := newServerTestDB(t)
	credential := dbpkg.Credential{AccessKey: "scoped", SecretKey: "secret", Bucket: "destination-bucket", Status: "enabled"}
	if err := gdb.Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	dataRoot := t.TempDir()
	backend, err := storage.NewFileBackend(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	bucketStore := storage.NewBucketStore(gdb, dataRoot, storage.DefaultBucketACLCacheTTL)
	for _, name := range []string{"destination-bucket", "source-bucket"} {
		if err := bucketStore.Create(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := backend.PutObject("source-bucket", "source.txt", strings.NewReader("source"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.PutObject("destination-bucket", "same-scope.txt", strings.NewReader("same"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	authenticator := fixedAuthenticator{id: &auth.Identity{
		CredentialID: credential.ID, AccessKey: credential.AccessKey, Bucket: credential.Bucket,
	}}
	router := NewManagedRouterWithQuotaManager(backend, nil, bucketStore, authenticator, quota.NewManager(gdb), nil, NewRateLimitController(config.RateLimitConfig{}))

	crossScope := headerSignedRequest(http.MethodPut, "/destination-bucket/copied.txt")
	crossScope.Header.Set("x-amz-copy-source", "/source-bucket/source.txt")
	crossScopeRR := httptest.NewRecorder()
	router.ServeHTTP(crossScopeRR, crossScope)
	if crossScopeRR.Code != http.StatusForbidden || !strings.Contains(crossScopeRR.Body.String(), auth.CodeAccessDenied) {
		t.Fatalf("cross-scope copy = %d %s", crossScopeRR.Code, crossScopeRR.Body.String())
	}
	if _, err := backend.HeadObject("destination-bucket", "copied.txt"); !errors.Is(err, storage.ErrNoSuchKey) {
		t.Fatalf("cross-scope copy created destination: %v", err)
	}

	sameScope := headerSignedRequest(http.MethodPut, "/destination-bucket/copied.txt")
	sameScope.Header.Set("x-amz-copy-source", "/destination-bucket/same-scope.txt")
	sameScopeRR := httptest.NewRecorder()
	router.ServeHTTP(sameScopeRR, sameScope)
	if sameScopeRR.Code != http.StatusOK {
		t.Fatalf("same-scope copy = %d %s", sameScopeRR.Code, sameScopeRR.Body.String())
	}
}

func TestRateLimitControllerUpdateReplacesLimiterSet(t *testing.T) {
	controller := NewRateLimitController(config.RateLimitConfig{AnonymousRPS: 1, AnonymousBurst: 1})
	handler := controller.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := func() int {
		req := httptest.NewRequest(http.MethodGet, "/public-bucket/key.txt", nil)
		req.RemoteAddr = "192.0.2.20:1234"
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr.Code
	}
	if got := request(); got != http.StatusNoContent {
		t.Fatalf("first status = %d", got)
	}
	if got := request(); got != http.StatusServiceUnavailable {
		t.Fatalf("limited status = %d", got)
	}
	controller.Update(config.RateLimitConfig{AnonymousRPS: 100, AnonymousBurst: 2, TrustForwarded: true})
	if got := request(); got != http.StatusNoContent {
		t.Fatalf("status after update = %d", got)
	}
	if cfg := controller.Config(); cfg.AnonymousRPS != 100 || cfg.AnonymousBurst != 2 || !cfg.TrustForwarded {
		t.Fatalf("controller config = %+v", cfg)
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

func TestRouterQuotaManagerConcurrentPutsCannotExceedQuota(t *testing.T) {
	gdb := newServerTestDB(t)
	credential := dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 10}
	if err := gdb.Create(&credential).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	dataRoot := t.TempDir()
	backend, err := storage.NewFileBackend(dataRoot)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	bucketStore := storage.NewBucketStore(gdb, dataRoot, storage.DefaultBucketACLCacheTTL)
	if err := bucketStore.Create("test-bucket"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	authenticator := fixedAuthenticator{id: &auth.Identity{CredentialID: credential.ID, AccessKey: credential.AccessKey, QuotaBytes: 10}}
	router := NewRouterWithQuotaManager(backend, nil, bucketStore, authenticator, quota.NewManager(gdb), nil, nil, config.RateLimitConfig{})

	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for _, key := range []string{"a.txt", "b.txt"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			req := headerSignedRequest(http.MethodPut, "/test-bucket/"+key)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, requestBody(req, "123456"))
			statuses <- rr.Code
		}(key)
	}
	wg.Wait()
	close(statuses)
	okCount := 0
	quotaCount := 0
	for status := range statuses {
		switch status {
		case http.StatusOK:
			okCount++
		case http.StatusForbidden:
			quotaCount++
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	if okCount != 1 || quotaCount != 1 {
		t.Fatalf("status counts ok=%d quota=%d, want 1/1", okCount, quotaCount)
	}
	var got dbpkg.Credential
	if err := gdb.First(&got, credential.ID).Error; err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if got.UsedBytes != 6 {
		t.Fatalf("used_bytes = %d, want 6", got.UsedBytes)
	}
}

func TestRouterQuotaManagerUnderreportedPutPreservesObject(t *testing.T) {
	gdb := newServerTestDB(t)
	credential := dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 100}
	if err := gdb.Create(&credential).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	dataRoot := t.TempDir()
	backend, err := storage.NewFileBackend(dataRoot)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if _, err := backend.PutObject("test-bucket", "keep.txt", strings.NewReader("original"), "text/plain"); err != nil {
		t.Fatalf("put original: %v", err)
	}
	bucketStore := storage.NewBucketStore(gdb, dataRoot, storage.DefaultBucketACLCacheTTL)
	if err := bucketStore.Create("test-bucket"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	authenticator := &stubAuthenticator{id: &auth.Identity{CredentialID: credential.ID, AccessKey: credential.AccessKey, QuotaBytes: 100}}
	router := NewRouterWithQuotaManager(backend, nil, bucketStore, authenticator, quota.NewManager(gdb), nil, nil, config.RateLimitConfig{})

	req := headerSignedRequest(http.MethodPut, "/test-bucket/keep.txt")
	req.Body = io.NopCloser(strings.NewReader("replacement"))
	req.ContentLength = 3
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	rc, _, err := backend.GetObject("test-bucket", "keep.txt", nil)
	if err != nil {
		t.Fatalf("get preserved object: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read preserved object: %v", err)
	}
	if string(body) != "original" {
		t.Fatalf("preserved object = %q, want original", body)
	}
	var got dbpkg.Credential
	if err := gdb.First(&got, credential.ID).Error; err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if got.UsedBytes != 0 {
		t.Fatalf("used_bytes = %d, want released reservation", got.UsedBytes)
	}
}

func TestRouterQuotaManagerOverwriteSettlesNetGrowth(t *testing.T) {
	gdb := newServerTestDB(t)
	credential := dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 10, UsedBytes: 8}
	if err := gdb.Create(&credential).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	dataRoot := t.TempDir()
	backend, err := storage.NewFileBackend(dataRoot)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if _, err := backend.PutObject("test-bucket", "key.txt", strings.NewReader("12345678"), "text/plain"); err != nil {
		t.Fatalf("put original: %v", err)
	}
	bucketStore := storage.NewBucketStore(gdb, dataRoot, storage.DefaultBucketACLCacheTTL)
	if err := bucketStore.Create("test-bucket"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	authenticator := &stubAuthenticator{id: &auth.Identity{CredentialID: credential.ID, AccessKey: credential.AccessKey, QuotaBytes: 10, UsedBytes: 8}}
	router := NewRouterWithQuotaManager(backend, nil, bucketStore, authenticator, quota.NewManager(gdb), nil, nil, config.RateLimitConfig{})

	req := headerSignedRequest(http.MethodPut, "/test-bucket/key.txt")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, requestBody(req, "123456789"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got dbpkg.Credential
	if err := gdb.First(&got, credential.ID).Error; err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if got.UsedBytes != 9 {
		t.Fatalf("used_bytes = %d, want net object size 9", got.UsedBytes)
	}
	var stat dbpkg.RequestStat
	if err := gdb.Where("credential_id = ?", credential.ID).First(&stat).Error; err != nil {
		t.Fatalf("read stat: %v", err)
	}
	if stat.PutCount != 1 || stat.BytesIn != 9 {
		t.Fatalf("unexpected stat: %+v", stat)
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

func captureDefaultLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logBuf bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	return &logBuf
}

func requireJSONLogEntry(t *testing.T, logBuf *bytes.Buffer, message string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("parse log entry: %v; line=%s", err, line)
		}
		if entry["msg"] == message {
			return entry
		}
	}
	t.Fatalf("log entry %q not found: %s", message, logBuf.String())
	return nil
}
