package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
