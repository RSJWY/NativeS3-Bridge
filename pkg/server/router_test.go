package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
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
