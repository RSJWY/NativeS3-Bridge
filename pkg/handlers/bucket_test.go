package handlers

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

func TestBucketProbeResponsesDoNotReturnListBucketResult(t *testing.T) {
	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if _, err := backend.PutObject("test-bucket", "a.txt", strings.NewReader("hello"), "text/plain"); err != nil {
		t.Fatalf("put object: %v", err)
	}
	h := NewBucketHandler(backend, nil)

	tests := []struct {
		name string
		url  string
		call func(http.ResponseWriter, *http.Request)
		want string
	}{
		{
			name: "location",
			url:  "/test-bucket?location",
			call: func(w http.ResponseWriter, r *http.Request) { h.GetBucketLocation(w, r, "test-bucket") },
			want: "<LocationConstraint",
		},
		{
			name: "versioning",
			url:  "/test-bucket?versioning",
			call: func(w http.ResponseWriter, r *http.Request) { h.GetBucketVersioning(w, r, "test-bucket") },
			want: "<VersioningConfiguration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			rr := httptest.NewRecorder()
			tt.call(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			body := rr.Body.String()
			if !strings.Contains(body, tt.want) {
				t.Fatalf("body = %s, want %s", body, tt.want)
			}
			if strings.Contains(body, "ListBucketResult") {
				t.Fatalf("probe fell through to list response: %s", body)
			}
		})
	}
}

func TestBucketHandlerDeleteBucketRejectsBoundCredential(t *testing.T) {
	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if _, err := backend.PutObject("bound-bucket", "placeholder", strings.NewReader("x"), "text/plain"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := backend.DeleteObject("bound-bucket", "placeholder"); err != nil {
		t.Fatalf("empty bucket: %v", err)
	}
	h := NewBucketHandlerWithCredentialChecker(backend, nil, func(bucket string) (bool, error) {
		return bucket == "bound-bucket", nil
	})
	req := httptest.NewRequest(http.MethodDelete, "/bound-bucket", nil)
	rr := httptest.NewRecorder()

	h.DeleteBucket(rr, req, "bound-bucket")

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "<Code>BucketNotEmpty</Code>") {
		t.Fatalf("body = %s, want BucketNotEmpty", rr.Body.String())
	}
	if _, err := backend.ListObjects("bound-bucket", "", "", "", 1); err != nil {
		t.Fatalf("bound bucket was deleted: %v", err)
	}
}

func TestBucketHandlerDeleteBucketAllowsUnboundEmptyBucket(t *testing.T) {
	root := t.TempDir()
	backend, err := storage.NewFileBackend(root)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	gdb, err := dbpkg.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := dbpkg.Migrate(gdb); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	bucketStore := storage.NewBucketStore(gdb, root, time.Second)
	if err := bucketStore.Create("empty-bucket"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	h := NewBucketHandlerWithCredentialChecker(backend, bucketStore, func(string) (bool, error) { return false, nil })
	req := httptest.NewRequest(http.MethodDelete, "/empty-bucket", nil)
	rr := httptest.NewRecorder()

	h.DeleteBucket(rr, req, "empty-bucket")

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketProbeMissingBucketReturnsNoSuchBucket(t *testing.T) {
	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	h := NewBucketHandler(backend, nil)

	tests := []struct {
		name string
		url  string
		call func(http.ResponseWriter, *http.Request)
	}{
		{
			name: "location",
			url:  "/missing-bucket?location",
			call: func(w http.ResponseWriter, r *http.Request) { h.GetBucketLocation(w, r, "missing-bucket") },
		},
		{
			name: "versioning",
			url:  "/missing-bucket?versioning",
			call: func(w http.ResponseWriter, r *http.Request) { h.GetBucketVersioning(w, r, "missing-bucket") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			rr := httptest.NewRecorder()
			tt.call(rr, req)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "<Code>NoSuchBucket</Code>") {
				t.Fatalf("body = %s, want NoSuchBucket", rr.Body.String())
			}
		})
	}
}
