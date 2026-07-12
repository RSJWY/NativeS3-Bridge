package webadmin

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	if response.Files == nil || len(response.Files) != 0 || response.SelectedFile != nil {
		t.Fatalf("file-disabled response metadata = %+v", response)
	}
}

func TestLogsAPITailsConfiguredFileAndFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("time=2026-07-12T10:00:00Z level=INFO msg=startup\ntime=2026-07-12T10:01:00Z level=ERROR msg=failed bucket=media\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyHistory := filepath.Join(filepath.Dir(path), "app-2026-07-11T10-00-00.000.log")
	if err := os.WriteFile(legacyHistory, []byte("time=2026-07-11T10:00:00Z level=INFO msg=history\n"), 0o600); err != nil {
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
	if len(response.Files) != 2 || response.Files[1].ID != filepath.Base(legacyHistory) || response.SelectedFile == nil || !response.SelectedFile.Current || response.SelectedFile.ID != filepath.Base(path) {
		t.Fatalf("file metadata = %+v selected=%+v", response.Files, response.SelectedFile)
	}
	if response.SelectedFile.Size == 0 || response.SelectedFile.ModifiedAt.IsZero() {
		t.Fatalf("selected file lacks size/mtime metadata: %+v", response.SelectedFile)
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

func TestLogsAPIListsAndReadsRotatedFiles(t *testing.T) {
	directory := t.TempDir()
	active := filepath.Join(directory, "natives3bridge.log")
	older := filepath.Join(directory, "natives3bridge-2026-07-10T09-00-00.000.log")
	newer := filepath.Join(directory, "natives3bridge-2026-07-11T09-00-00.000.log.gz")
	if err := os.WriteFile(active, []byte("time=2026-07-12T10:00:00Z level=INFO msg=current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(older, []byte("time=2026-07-10T10:00:00Z level=WARN msg=older bucket=archive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeGzipLog(t, newer, "time=2026-07-11T10:00:00Z level=INFO msg=ignored\ntime=2026-07-11T10:01:00Z level=ERROR msg=failed bucket=media\n")
	oldTime := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	newTime := oldTime.Add(24 * time.Hour)
	if err := os.Chtimes(older, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "other.log"), []byte("do not expose"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "natives3bridge-latest.log"), []byte("do not expose"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.log")
	if err := os.WriteFile(outside, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkName := "natives3bridge-2026-07-09T09-00-00.000.log"
	if err := os.Symlink(outside, filepath.Join(directory, symlinkName)); err != nil {
		t.Fatal(err)
	}

	api := NewAPI(newWebadminTestDB(t), nil, nil, APIOptions{LogFile: active, LogRing: logging.NewRing(10)})
	rr := httptest.NewRecorder()
	api.Logs(rr, httptest.NewRequest(http.MethodGet, "/api/admin/logs", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("current logs status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var response logsResponse
	decodeJSONBody(t, rr.Body.Bytes(), &response)
	if len(response.Files) != 3 {
		t.Fatalf("files = %+v, want current and two rotations", response.Files)
	}
	if !response.Files[0].Current || response.Files[0].ID != filepath.Base(active) || response.Files[1].ID != filepath.Base(newer) || response.Files[2].ID != filepath.Base(older) {
		t.Fatalf("file order = %+v", response.Files)
	}
	if !response.Files[1].Compressed || response.Files[2].Compressed {
		t.Fatalf("compression metadata = %+v", response.Files)
	}
	if response.SelectedFile == nil || response.SelectedFile.ID != filepath.Base(active) || response.Entries[0].Message != "current" {
		t.Fatalf("default selection = %+v entries=%+v", response.SelectedFile, response.Entries)
	}

	rr = httptest.NewRecorder()
	target := "/api/admin/logs?file=" + filepath.Base(newer) + "&level=ERROR&q=media&limit=1"
	api.Logs(rr, httptest.NewRequest(http.MethodGet, target, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("gzip logs status = %d, body=%s", rr.Code, rr.Body.String())
	}
	decodeJSONBody(t, rr.Body.Bytes(), &response)
	if response.SelectedFile == nil || response.SelectedFile.ID != filepath.Base(newer) || len(response.Entries) != 1 || response.Entries[0].Message != "failed" || response.Entries[0].Attrs["bucket"] != "media" {
		t.Fatalf("gzip response = %+v", response)
	}

	rr = httptest.NewRecorder()
	api.Logs(rr, httptest.NewRequest(http.MethodGet, "/api/admin/logs?file="+filepath.Base(older)+"&level=WARN", nil))
	decodeJSONBody(t, rr.Body.Bytes(), &response)
	if rr.Code != http.StatusOK || len(response.Entries) != 1 || response.Entries[0].Message != "older" {
		t.Fatalf("plain history response = %d %+v", rr.Code, response)
	}
}

func TestLogsAPIRejectsUnsafeAndUnavailableSelections(t *testing.T) {
	directory := t.TempDir()
	active := filepath.Join(directory, "natives3bridge.log")
	if err := os.WriteFile(active, []byte("time=2026-07-12T10:00:00Z level=INFO msg=current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.log")
	if err := os.WriteFile(outside, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkID := "natives3bridge-2026-07-09T09-00-00.000.log"
	if err := os.Symlink(outside, filepath.Join(directory, symlinkID)); err != nil {
		t.Fatal(err)
	}
	api := NewAPI(newWebadminTestDB(t), nil, nil, APIOptions{LogFile: active, LogRing: logging.NewRing(10)})

	tests := []struct {
		name       string
		file       string
		wantStatus int
	}{
		{name: "traversal", file: "../secret.log", wantStatus: http.StatusBadRequest},
		{name: "absolute", file: outside, wantStatus: http.StatusBadRequest},
		{name: "slash", file: "sub/secret.log", wantStatus: http.StatusBadRequest},
		{name: "backslash", file: `sub\\secret.log`, wantStatus: http.StatusBadRequest},
		{name: "non matching", file: "other.log", wantStatus: http.StatusNotFound},
		{name: "cleaned rotation", file: "natives3bridge-2026-07-08T09-00-00.000.log", wantStatus: http.StatusNotFound},
		{name: "symlink", file: symlinkID, wantStatus: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			target := "/api/admin/logs?file=" + url.QueryEscape(tt.file)
			api.Logs(rr, httptest.NewRequest(http.MethodGet, target, nil))
			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tt.wantStatus, rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), "outside-secret") {
				t.Fatalf("unsafe response leaked file: %s", rr.Body.String())
			}
		})
	}

	corrupt := filepath.Join(directory, "natives3bridge-2026-07-07T09-00-00.000.log.gz")
	if err := os.WriteFile(corrupt, []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	api.Logs(rr, httptest.NewRequest(http.MethodGet, "/api/admin/logs?file="+filepath.Base(corrupt), nil))
	if rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), "current") {
		t.Fatalf("corrupt gzip response = %d %s", rr.Code, rr.Body.String())
	}
}

func writeGzipLog(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := gzip.NewWriter(file)
	if _, err := writer.Write([]byte(content)); err != nil {
		writer.Close()
		file.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
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

func TestReconcileBucketEmptyCollectionsAreArrays(t *testing.T) {
	gdb := newWebadminTestDB(t)
	root := t.TempDir()
	bucket := "empty-bucket"
	if err := os.MkdirAll(filepath.Join(root, bucket), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&dbpkg.Bucket{Name: bucket, ACL: storage.ACLPrivate}).Error; err != nil {
		t.Fatal(err)
	}
	api := NewAPI(gdb, nil, storage.NewBucketStore(gdb, root, time.Second), APIOptions{DataRoot: root, MetadataSuffix: ".s3meta"})

	response := httptest.NewRecorder()
	api.BucketByName(response, httptest.NewRequest(http.MethodPost, "/api/admin/buckets/empty-bucket/reconcile", bytes.NewBufferString(`{"apply":false}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("reconcile = %d, %s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !bytes.Contains([]byte(body), []byte(`"orphan_sidecar_samples":[]`)) || !bytes.Contains([]byte(body), []byte(`"bound_credentials":[]`)) {
		t.Fatalf("empty collections must be arrays: %s", body)
	}
}
