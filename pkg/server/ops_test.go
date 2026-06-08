package server

import (
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/quota"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

func newOpsTestRouter(t *testing.T) (http.Handler, *storage.FileBackend, string) {
	t.Helper()
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
	router := NewRouter(backend, nil, bucketStore, &stubAuthenticator{}, func(uint, int64, quota.Op) error { return nil }, nil, config.RateLimitConfig{})
	return router, backend, dataRoot
}

func TestRouterPutContentMD5MismatchReturnsBadDigest(t *testing.T) {
	router, backend, dataRoot := newOpsTestRouter(t)

	req := headerSignedRequest(http.MethodPut, "/test-bucket/obj.txt")
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(md5.New().Sum(nil))) // md5 of empty != real-bytes
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, withBody(req, "real-bytes"))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "BadDigest") {
		t.Fatalf("body = %s, want BadDigest", rr.Body.String())
	}
	assertObjectWriteFailedCleanly(t, backend, dataRoot, "obj.txt")
}

func TestRouterPutContentMD5MatchSucceeds(t *testing.T) {
	router, backend, _ := newOpsTestRouter(t)
	body := "verified-bytes"
	sum := md5.Sum([]byte(body))

	req := headerSignedRequest(http.MethodPut, "/test-bucket/obj.txt")
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, withBody(req, body))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if _, err := backend.HeadObject("test-bucket", "obj.txt"); err != nil {
		t.Fatalf("object not stored: %v", err)
	}
}

func TestRouterPutInvalidContentMD5ReturnsInvalidDigestAndDoesNotWrite(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{name: "invalid base64", header: "not-base64!!!"},
		{name: "wrong decoded length", header: base64.StdEncoding.EncodeToString([]byte("short"))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, backend, dataRoot := newOpsTestRouter(t)
			req := headerSignedRequest(http.MethodPut, "/test-bucket/invalid.txt")
			req.Header.Set("Content-MD5", tc.header)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, withBody(req, "body-not-written"))

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "InvalidDigest") {
				t.Fatalf("body = %s, want InvalidDigest", rr.Body.String())
			}
			assertObjectWriteFailedCleanly(t, backend, dataRoot, "invalid.txt")
		})
	}
}

func TestRouterPutWithoutContentMD5StillSucceeds(t *testing.T) {
	router, backend, _ := newOpsTestRouter(t)
	body := "ordinary-upload"

	req := headerSignedRequest(http.MethodPut, "/test-bucket/no-md5.txt")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, withBody(req, body))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	info, err := backend.HeadObject("test-bucket", "no-md5.txt")
	if err != nil {
		t.Fatalf("object not stored: %v", err)
	}
	sum := md5.Sum([]byte(body))
	if info.ETag != fmt.Sprintf("%x", sum) {
		t.Fatalf("etag = %q, want md5 of body", info.ETag)
	}
}

func TestRouterCopyObject(t *testing.T) {
	router, backend, _ := newOpsTestRouter(t)
	if _, err := backend.PutObject("test-bucket", "src.txt", strings.NewReader("source-data"), "text/plain"); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	req := headerSignedRequest(http.MethodPut, "/test-bucket/dst.txt")
	req.Header.Set("x-amz-copy-source", "/test-bucket/src.txt")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "CopyObjectResult") {
		t.Fatalf("body = %s, want CopyObjectResult", rr.Body.String())
	}
	info, err := backend.HeadObject("test-bucket", "dst.txt")
	if err != nil {
		t.Fatalf("copied object missing: %v", err)
	}
	if info.Size != int64(len("source-data")) {
		t.Fatalf("copied size = %d, want %d", info.Size, len("source-data"))
	}
}

func TestRouterDeleteObjects(t *testing.T) {
	router, backend, _ := newOpsTestRouter(t)
	for _, k := range []string{"a.txt", "b.txt"} {
		if _, err := backend.PutObject("test-bucket", k, strings.NewReader("x"), "text/plain"); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	body := `<Delete><Object><Key>a.txt</Key></Object><Object><Key>b.txt</Key></Object></Delete>`
	req := headerSignedRequest(http.MethodPost, "/test-bucket?delete")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, withBody(req, body))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	if !strings.Contains(out, "<Key>a.txt</Key>") || !strings.Contains(out, "<Key>b.txt</Key>") {
		t.Fatalf("body = %s, want both keys in DeleteResult", out)
	}
	if _, err := backend.HeadObject("test-bucket", "a.txt"); err == nil {
		t.Fatal("a.txt should be deleted")
	}
}

func TestRouterBucketSubresources(t *testing.T) {
	router, _, _ := newOpsTestRouter(t)

	cases := []struct {
		query string
		want  string
	}{
		{"?location", "LocationConstraint"},
		{"?versioning", "VersioningConfiguration"},
	}
	for _, c := range cases {
		req := headerSignedRequest(http.MethodGet, "/test-bucket"+c.query)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", c.query, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), c.want) {
			t.Fatalf("%s body = %s, want %s", c.query, rr.Body.String(), c.want)
		}
	}
}

func assertObjectWriteFailedCleanly(t *testing.T, backend *storage.FileBackend, dataRoot, key string) {
	t.Helper()
	if _, err := backend.HeadObject("test-bucket", key); !errors.Is(err, storage.ErrNoSuchKey) {
		t.Fatalf("HeadObject after failed write = %v, want ErrNoSuchKey", err)
	}
	target := filepath.Join(dataRoot, "test-bucket", filepath.FromSlash(key))
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target object exists or unexpected stat error: %v", err)
	}
	if _, err := os.Stat(target + storage.DefaultMetadataSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sidecar exists or unexpected stat error: %v", err)
	}
	if err := filepath.WalkDir(filepath.Join(dataRoot, "test-bucket"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.Contains(d.Name(), ".tmp-") {
			return fmt.Errorf("unexpected leftover temp file: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func withBody(req *http.Request, body string) *http.Request {
	clone := httptest.NewRequest(req.Method, req.URL.String(), strings.NewReader(body))
	for k, v := range req.Header {
		clone.Header[k] = v
	}
	return clone
}
