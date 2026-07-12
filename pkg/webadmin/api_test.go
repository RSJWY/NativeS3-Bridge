package webadmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
	"gorm.io/gorm"
)

func TestBucketAPIRequiresSession(t *testing.T) {
	handler, _ := newBucketAPITestHandler(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{name: "list", method: http.MethodGet, path: "/api/admin/buckets"},
		{name: "create", method: http.MethodPost, path: "/api/admin/buckets", body: []byte(`{"name":"test-bucket"}`)},
		{name: "delete", method: http.MethodDelete, path: "/api/admin/buckets/test-bucket"},
		{name: "set acl", method: http.MethodPut, path: "/api/admin/buckets/test-bucket/acl", body: []byte(`{"acl":"public-read"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
			if tc.body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
			}
			assertJSONError(t, rr.Body.Bytes(), "unauthorized")
		})
	}
}

func TestBucketAPIListCreateSetACLAndDelete(t *testing.T) {
	handler, auth := newBucketAPITestHandler(t)

	rr := authRequest(t, handler, auth, http.MethodGet, "/api/admin/buckets", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("initial list status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var initial []bucketResponse
	decodeJSONBody(t, rr.Body.Bytes(), &initial)
	if len(initial) != 0 {
		t.Fatalf("initial list length = %d, want 0", len(initial))
	}

	rr = authRequest(t, handler, auth, http.MethodPost, "/api/admin/buckets", []byte(`{"name":"test-bucket"}`))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var created bucketResponse
	decodeJSONBody(t, rr.Body.Bytes(), &created)
	if created.Name != "test-bucket" || created.ACL != storage.ACLPrivate || created.CreatedAt.IsZero() {
		t.Fatalf("created bucket = %+v, want name test-bucket acl private with created_at", created)
	}

	rr = authRequest(t, handler, auth, http.MethodGet, "/api/admin/buckets", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var listed []bucketResponse
	decodeJSONBody(t, rr.Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0].Name != "test-bucket" || listed[0].ACL != storage.ACLPrivate {
		t.Fatalf("listed buckets = %+v, want one private test-bucket", listed)
	}

	rr = authRequest(t, handler, auth, http.MethodPut, "/api/admin/buckets/test-bucket/acl", []byte(`{"acl":"public-read"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("set acl status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var updated bucketResponse
	decodeJSONBody(t, rr.Body.Bytes(), &updated)
	if updated.Name != "test-bucket" || updated.ACL != storage.ACLPublicRead {
		t.Fatalf("updated bucket = %+v, want public-read", updated)
	}
	rr = authRequest(t, handler, auth, http.MethodGet, "/api/admin/buckets", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list after acl status = %d, body = %s", rr.Code, rr.Body.String())
	}
	decodeJSONBody(t, rr.Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0].Name != "test-bucket" || listed[0].ACL != storage.ACLPublicRead {
		t.Fatalf("listed buckets after acl = %+v, want one public-read test-bucket", listed)
	}

	rr = authRequest(t, handler, auth, http.MethodDelete, "/api/admin/buckets/test-bucket", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var ok map[string]bool
	decodeJSONBody(t, rr.Body.Bytes(), &ok)
	if !ok["ok"] {
		t.Fatalf("delete response = %+v, want ok true", ok)
	}

	rr = authRequest(t, handler, auth, http.MethodGet, "/api/admin/buckets", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("final list status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var final []bucketResponse
	decodeJSONBody(t, rr.Body.Bytes(), &final)
	if len(final) != 0 {
		t.Fatalf("final list length = %d, want 0", len(final))
	}
}

func TestBucketAPIErrors(t *testing.T) {
	handler, auth := newBucketAPITestHandler(t)

	rr := authRequest(t, handler, auth, http.MethodPost, "/api/admin/buckets", []byte(`{"name":"Bad"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid create status = %d, body = %s", rr.Code, rr.Body.String())
	}
	assertJSONError(t, rr.Body.Bytes(), "invalid bucket name")
}

func TestBucketAPIInvalidACLAndNonEmptyDelete(t *testing.T) {
	handler, auth, dataRoot := newBucketAPITestHandlerWithRoot(t)

	rr := authRequest(t, handler, auth, http.MethodPost, "/api/admin/buckets", []byte(`{"name":"nonempty-bucket"}`))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rr.Code, rr.Body.String())
	}

	rr = authRequest(t, handler, auth, http.MethodPut, "/api/admin/buckets/nonempty-bucket/acl", []byte(`{"acl":"public"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid acl status = %d, body = %s", rr.Code, rr.Body.String())
	}
	assertJSONError(t, rr.Body.Bytes(), "acl must be private or public-read")

	objectPath := filepath.Join(dataRoot, "nonempty-bucket", "object.txt")
	if err := os.WriteFile(objectPath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write object file: %v", err)
	}

	rr = authRequest(t, handler, auth, http.MethodDelete, "/api/admin/buckets/nonempty-bucket", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("non-empty delete status = %d, body = %s", rr.Code, rr.Body.String())
	}
	assertJSONError(t, rr.Body.Bytes(), "bucket not empty")
}

func TestBucketAPINotFoundAndMethods(t *testing.T) {
	handler, auth := newBucketAPITestHandler(t)

	rr := authRequest(t, handler, auth, http.MethodDelete, "/api/admin/buckets/missing-bucket", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing delete status = %d, body = %s", rr.Code, rr.Body.String())
	}
	assertJSONError(t, rr.Body.Bytes(), "bucket not found")

	rr = authRequest(t, handler, auth, http.MethodPatch, "/api/admin/buckets/missing-bucket", nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unsupported method status = %d, body = %s", rr.Code, rr.Body.String())
	}
	assertJSONError(t, rr.Body.Bytes(), "method not allowed")
}

func newBucketAPITestHandler(t *testing.T) (http.Handler, *Auth) {
	t.Helper()
	handler, auth, _ := newBucketAPITestHandlerWithRoot(t)
	return handler, auth
}

func newBucketAPITestHandlerWithRoot(t *testing.T) (http.Handler, *Auth, string) {
	t.Helper()
	gdb := newWebadminTestDB(t)
	dataRoot := t.TempDir()
	bucketStore := storage.NewBucketStore(gdb, dataRoot, time.Second)
	api := NewAPI(gdb, nil, bucketStore)
	auth := NewAuth(config.WebAdminConfig{PasswordHash: mustPasswordHash(t), SessionSecret: "test-session-secret", SessionTTLMinutes: 10})

	mux := http.NewServeMux()
	mux.Handle("/api/admin/buckets", auth.Middleware(http.HandlerFunc(api.Buckets)))
	mux.Handle("/api/admin/buckets/", auth.Middleware(http.HandlerFunc(api.BucketByName)))
	mux.Handle("/api/admin/credentials", auth.Middleware(http.HandlerFunc(api.Credentials)))
	mux.Handle("/api/admin/credentials/", auth.Middleware(http.HandlerFunc(api.CredentialByID)))
	return mux, auth, dataRoot
}

func TestCredentialBucketScoping(t *testing.T) {
	handler, auth := newBucketAPITestHandler(t)
	for _, bucket := range []string{"my-bucket", "other-bucket"} {
		rr := authRequest(t, handler, auth, http.MethodPost, "/api/admin/buckets", []byte(`{"name":"`+bucket+`"}`))
		if rr.Code != http.StatusCreated {
			t.Fatalf("create bucket %q status = %d, body = %s", bucket, rr.Code, rr.Body.String())
		}
	}

	rr := authRequest(t, handler, auth, http.MethodPost, "/api/admin/credentials", []byte(`{"name":"scoped-key","bucket":"my-bucket","quota_bytes":0}`))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create scoped credential status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var created createCredentialResponse
	decodeJSONBody(t, rr.Body.Bytes(), &created)
	if created.Bucket != "my-bucket" {
		t.Fatalf("created bucket = %q, want my-bucket", created.Bucket)
	}

	rr = authRequest(t, handler, auth, http.MethodPost, "/api/admin/credentials", []byte(`{"name":"global-key","quota_bytes":0}`))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create global credential status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var global createCredentialResponse
	decodeJSONBody(t, rr.Body.Bytes(), &global)
	if global.Bucket != "" {
		t.Fatalf("global bucket = %q, want empty", global.Bucket)
	}

	rr = authRequest(t, handler, auth, http.MethodGet, "/api/admin/credentials", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var listed []credentialResponse
	decodeJSONBody(t, rr.Body.Bytes(), &listed)
	if len(listed) != 2 {
		t.Fatalf("list length = %d, want 2", len(listed))
	}
	if listed[0].Bucket != "" && listed[1].Bucket != "" {
		t.Fatalf("expected at least one empty bucket in list: %+v", listed)
	}

	rr = authRequest(t, handler, auth, http.MethodPatch, "/api/admin/credentials/"+strconv.FormatUint(uint64(created.ID), 10), []byte(`{"bucket":"other-bucket"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("update bucket status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var updated credentialResponse
	decodeJSONBody(t, rr.Body.Bytes(), &updated)
	if updated.Bucket != "other-bucket" {
		t.Fatalf("updated bucket = %q, want other-bucket", updated.Bucket)
	}

	rr = authRequest(t, handler, auth, http.MethodPatch, "/api/admin/credentials/"+strconv.FormatUint(uint64(created.ID), 10), []byte(`{"bucket":""}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("clear bucket status = %d, body = %s", rr.Code, rr.Body.String())
	}
	decodeJSONBody(t, rr.Body.Bytes(), &updated)
	if updated.Bucket != "" {
		t.Fatalf("cleared bucket = %q, want empty", updated.Bucket)
	}
}

func TestCredentialBucketMustExistAndBlocksBucketDelete(t *testing.T) {
	handler, auth := newBucketAPITestHandler(t)

	missing := authRequest(t, handler, auth, http.MethodPost, "/api/admin/credentials", []byte(`{"name":"missing","bucket":"missing-bucket","quota_bytes":0}`))
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing bucket credential status = %d, want 400; body=%s", missing.Code, missing.Body.String())
	}
	assertJSONError(t, missing.Body.Bytes(), "bucket does not exist")

	createdBucket := authRequest(t, handler, auth, http.MethodPost, "/api/admin/buckets", []byte(`{"name":"bound-bucket"}`))
	if createdBucket.Code != http.StatusCreated {
		t.Fatalf("create bucket status = %d; body=%s", createdBucket.Code, createdBucket.Body.String())
	}
	createdCredential := authRequest(t, handler, auth, http.MethodPost, "/api/admin/credentials", []byte(`{"name":"bound","bucket":"bound-bucket","quota_bytes":0}`))
	if createdCredential.Code != http.StatusCreated {
		t.Fatalf("create credential status = %d; body=%s", createdCredential.Code, createdCredential.Body.String())
	}
	var credential createCredentialResponse
	decodeJSONBody(t, createdCredential.Body.Bytes(), &credential)

	blocked := authRequest(t, handler, auth, http.MethodDelete, "/api/admin/buckets/bound-bucket", nil)
	if blocked.Code != http.StatusConflict {
		t.Fatalf("bound bucket delete status = %d, want 409; body=%s", blocked.Code, blocked.Body.String())
	}
	assertJSONError(t, blocked.Body.Bytes(), "bucket has bound credentials")

	unbind := authRequest(t, handler, auth, http.MethodPatch, "/api/admin/credentials/"+strconv.FormatUint(uint64(credential.ID), 10), []byte(`{"bucket":""}`))
	if unbind.Code != http.StatusOK {
		t.Fatalf("unbind status = %d; body=%s", unbind.Code, unbind.Body.String())
	}
	deleted := authRequest(t, handler, auth, http.MethodDelete, "/api/admin/buckets/bound-bucket", nil)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete after unbind status = %d; body=%s", deleted.Code, deleted.Body.String())
	}
}

func TestCredentialBucketValidationRejectsInvalidName(t *testing.T) {
	handler, auth := newBucketAPITestHandler(t)

	rr := authRequest(t, handler, auth, http.MethodPost, "/api/admin/credentials", []byte(`{"name":"bad","bucket":"INVALID","quota_bytes":0}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid bucket create status = %d, body = %s", rr.Code, rr.Body.String())
	}
	assertJSONError(t, rr.Body.Bytes(), "bucket must be a valid bucket name or empty for all buckets")
}

func newWebadminTestDB(t *testing.T) *gorm.DB {
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

func mustPasswordHash(t *testing.T) string {
	t.Helper()
	cfg := config.WebAdminConfig{AdminBootstrapPassword: "test-password"}
	if err := BootstrapPasswordHash(&cfg); err != nil {
		t.Fatalf("bootstrap password: %v", err)
	}
	return cfg.PasswordHash
}

func authRequest(t *testing.T, handler http.Handler, auth *Auth, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	session, err := auth.issueSession(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sign session: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func decodeJSONBody(t *testing.T, body []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(body, dst); err != nil {
		t.Fatalf("decode response %q: %v", string(body), err)
	}
}

func assertJSONError(t *testing.T, body []byte, want string) {
	t.Helper()
	var payload map[string]string
	decodeJSONBody(t, body, &payload)
	if payload["error"] != want {
		t.Fatalf("error = %q, want %q", payload["error"], want)
	}
}
