package webadmin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"github.com/RSJWY/NativeS3-Bridge/pkg/logging"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

type recordingInvalidator struct{ keys []string }

func (r *recordingInvalidator) Invalidate(accessKey string) { r.keys = append(r.keys, accessKey) }

func TestLogsAPIRequiresAuthAndFiltersRing(t *testing.T) {
	gdb := newWebadminTestDB(t)
	ring := logging.NewRing(10)
	ring.Append(logging.Entry{Time: time.Now(), Level: "INFO", Message: "startup"})
	ring.Append(logging.Entry{Time: time.Now(), Level: "ERROR", Message: "bucket failed", Attrs: map[string]any{"bucket": "media"}})
	api := NewAPI(gdb, nil, nil, APIOptions{LogRing: ring})
	auth := NewAuth(config.WebAdminConfig{PasswordHash: mustPasswordHash(t), SessionSecret: "test-session-secret", SessionTTLMinutes: 10})
	mux := http.NewServeMux()
	mux.Handle("/api/admin/logs", auth.Middleware(http.HandlerFunc(api.Logs)))

	unauthorized := httptest.NewRecorder()
	mux.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/admin/logs", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
	authorized := authRequest(t, mux, auth, http.MethodGet, "/api/admin/logs?limit=5000&level=error&q=media", nil)
	if authorized.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", authorized.Code, authorized.Body.String())
	}
	var response logsResponse
	decodeJSONBody(t, authorized.Body.Bytes(), &response)
	if response.Source != "ring" || response.Limit != 1000 || len(response.Entries) != 1 || response.Entries[0].Message != "bucket failed" {
		t.Fatalf("response = %+v", response)
	}
}

func TestLogsAPITailsConfiguredFileAndFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("time=2026-07-12T10:00:00Z level=INFO msg=startup\ntime=2026-07-12T10:01:00Z level=ERROR msg=failed bucket=media\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := NewAPI(newWebadminTestDB(t), nil, nil, APIOptions{LogFile: path, LogRing: logging.NewRing(10)})
	rr := httptest.NewRecorder()
	api.Logs(rr, httptest.NewRequest(http.MethodGet, "/api/admin/logs?level=ERROR", nil))
	var response logsResponse
	decodeJSONBody(t, rr.Body.Bytes(), &response)
	if response.Source != "file" || len(response.Entries) != 1 || response.Entries[0].Attrs["bucket"] != "media" {
		t.Fatalf("response = %+v", response)
	}

	ring := logging.NewRing(10)
	ring.Append(logging.Entry{Level: "WARN", Message: "fallback"})
	api = NewAPI(newWebadminTestDB(t), nil, nil, APIOptions{LogFile: path + ".missing", LogRing: ring})
	rr = httptest.NewRecorder()
	api.Logs(rr, httptest.NewRequest(http.MethodGet, "/api/admin/logs", nil))
	decodeJSONBody(t, rr.Body.Bytes(), &response)
	if response.Source != "ring" || response.Warning == "" || len(response.Entries) != 1 {
		t.Fatalf("fallback response = %+v", response)
	}
}

func TestReconcileBucketDryRunAndApply(t *testing.T) {
	gdb := newWebadminTestDB(t)
	root := t.TempDir()
	bucketPath := filepath.Join(root, "media-bucket")
	if err := os.MkdirAll(bucketPath, 0o755); err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(bucketPath, "video.bin")
	if err := os.WriteFile(objectPath, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	orphanPath := filepath.Join(bucketPath, "gone.bin.s3meta")
	if err := os.WriteFile(orphanPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&dbpkg.Bucket{Name: "media-bucket", ACL: storage.ACLPrivate}).Error; err != nil {
		t.Fatal(err)
	}
	credentials := []dbpkg.Credential{
		{AccessKey: "BOUND", SecretKey: "secret", Name: "bound", Bucket: "media-bucket", Status: "enabled", UsedBytes: 99},
		{AccessKey: "GLOBAL", SecretKey: "secret", Name: "global", Bucket: "", Status: "enabled", UsedBytes: 77},
	}
	if err := gdb.Create(&credentials).Error; err != nil {
		t.Fatal(err)
	}
	invalidator := &recordingInvalidator{}
	api := NewAPI(gdb, invalidator, storage.NewBucketStore(gdb, root, time.Second), APIOptions{DataRoot: root, MetadataSuffix: ".s3meta"})

	dryRun := httptest.NewRecorder()
	api.BucketByName(dryRun, httptest.NewRequest(http.MethodPost, "/api/admin/buckets/media-bucket/reconcile", bytes.NewBufferString(`{"apply":false}`)))
	if dryRun.Code != http.StatusOK {
		t.Fatalf("dry run = %d, %s", dryRun.Code, dryRun.Body.String())
	}
	var dryResponse reconcileResponse
	decodeJSONBody(t, dryRun.Body.Bytes(), &dryResponse)
	if dryResponse.ScannedBytes != 5 || dryResponse.OrphanSidecarCount != 1 || dryResponse.BoundCredentials[0].DiffBytes != 94 {
		t.Fatalf("dry response = %+v", dryResponse)
	}
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("dry run removed orphan: %v", err)
	}

	apply := httptest.NewRecorder()
	api.BucketByName(apply, httptest.NewRequest(http.MethodPost, "/api/admin/buckets/media-bucket/reconcile", bytes.NewBufferString(`{"apply":true}`)))
	if apply.Code != http.StatusOK {
		t.Fatalf("apply = %d, %s", apply.Code, apply.Body.String())
	}
	var applyResponse reconcileResponse
	decodeJSONBody(t, apply.Body.Bytes(), &applyResponse)
	if applyResponse.OrphansDeleted != 1 || applyResponse.CredentialsUpdated != 1 || applyResponse.BoundCredentials[0].UsedBytes != 5 || applyResponse.BoundCredentials[0].DiffBytes != 0 {
		t.Fatalf("apply response = %+v", applyResponse)
	}
	var bound, global dbpkg.Credential
	if err := gdb.Where("access_key = ?", "BOUND").First(&bound).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Where("access_key = ?", "GLOBAL").First(&global).Error; err != nil {
		t.Fatal(err)
	}
	if bound.UsedBytes != 5 || global.UsedBytes != 77 || len(invalidator.keys) != 1 || invalidator.keys[0] != "BOUND" {
		t.Fatalf("bound=%d global=%d invalidated=%v", bound.UsedBytes, global.UsedBytes, invalidator.keys)
	}
	if _, err := os.Stat(objectPath); err != nil {
		t.Fatalf("object removed: %v", err)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("orphan remains: %v", err)
	}
}

func TestReconcileBucketAuthAndErrors(t *testing.T) {
	gdb := newWebadminTestDB(t)
	root := t.TempDir()
	api := NewAPI(gdb, nil, storage.NewBucketStore(gdb, root, time.Second), APIOptions{DataRoot: root, MetadataSuffix: ".s3meta"})
	auth := NewAuth(config.WebAdminConfig{PasswordHash: mustPasswordHash(t), SessionSecret: "test-session-secret", SessionTTLMinutes: 10})
	mux := http.NewServeMux()
	mux.Handle("/api/admin/buckets/", auth.Middleware(http.HandlerFunc(api.BucketByName)))

	unauthorized := httptest.NewRecorder()
	mux.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/admin/buckets/missing-bucket/reconcile", bytes.NewBufferString(`{"apply":false}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
	invalid := authRequest(t, mux, auth, http.MethodPost, "/api/admin/buckets/Bad/reconcile", []byte(`{"apply":false}`))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid status = %d, body = %s", invalid.Code, invalid.Body.String())
	}
	missing := authRequest(t, mux, auth, http.MethodPost, "/api/admin/buckets/missing-bucket/reconcile", []byte(`{"apply":false}`))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, body = %s", missing.Code, missing.Body.String())
	}
}
