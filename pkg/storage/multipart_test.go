package storage

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMultipartCompleteMergesInOrderAndWritesMultipartETag(t *testing.T) {
	root := t.TempDir()
	store, err := NewMultipartStore(root, filepath.Join(root, ".multipart"), ".s3meta")
	if err != nil {
		t.Fatalf("new multipart store: %v", err)
	}
	uploadID, err := store.Create("test-bucket", "big.bin", "application/octet-stream", map[string]string{"author": "jdoe"}, map[string]string{"env": "prod"})
	if err != nil {
		t.Fatalf("create multipart: %v", err)
	}
	etag1, err := store.UploadPart(uploadID, 1, strings.NewReader("hello "))
	if err != nil {
		t.Fatalf("upload part 1: %v", err)
	}
	etag2, err := store.UploadPart(uploadID, 2, strings.NewReader("world"))
	if err != nil {
		t.Fatalf("upload part 2: %v", err)
	}

	parts, err := store.ListParts(uploadID)
	if err != nil {
		t.Fatalf("list parts: %v", err)
	}
	if len(parts) != 2 || parts[0].PartNumber != 1 || parts[1].PartNumber != 2 {
		t.Fatalf("parts = %+v, want 1 and 2", parts)
	}

	info, err := store.Complete(uploadID, []CompletedPart{{PartNumber: 1, ETag: `"` + etag1 + `"`}, {PartNumber: 2, ETag: etag2}})
	if err != nil {
		t.Fatalf("complete multipart: %v", err)
	}
	if info.ETag != expectedMultipartETag("hello ", "world") {
		t.Fatalf("etag = %q, want multipart etag", info.ETag)
	}
	data, err := os.ReadFile(filepath.Join(root, "test-bucket", "big.bin"))
	if err != nil {
		t.Fatalf("read merged object: %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("merged content = %q", string(data))
	}
	if _, err := os.Stat(filepath.Join(root, ".multipart", uploadID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("multipart temp dir should be cleaned, stat err=%v", err)
	}
	sidecar, exists, err := ReadSidecar(filepath.Join(root, "test-bucket", "big.bin"), ".s3meta")
	if err != nil || !exists {
		t.Fatalf("read sidecar exists=%t err=%v", exists, err)
	}
	if sidecar.ETag != info.ETag || sidecar.Metadata["author"] != "jdoe" || sidecar.Tags["env"] != "prod" {
		t.Fatalf("sidecar = %+v", sidecar)
	}
}

func TestMultipartAbortAndCleanupExpired(t *testing.T) {
	root := t.TempDir()
	store, err := NewMultipartStore(root, filepath.Join(root, ".multipart"), ".s3meta")
	if err != nil {
		t.Fatalf("new multipart store: %v", err)
	}
	uploadID, err := store.Create("test-bucket", "abort.bin", "", nil, nil)
	if err != nil {
		t.Fatalf("create abort upload: %v", err)
	}
	if err := store.Abort(uploadID); err != nil {
		t.Fatalf("abort upload: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".multipart", uploadID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aborted temp dir should be deleted, stat err=%v", err)
	}

	expiredID, err := store.Create("test-bucket", "expired.bin", "", nil, nil)
	if err != nil {
		t.Fatalf("create expired upload: %v", err)
	}
	manifest := filepath.Join(root, ".multipart", expiredID, "manifest.json")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	data = []byte(strings.Replace(string(data), time.Now().UTC().Format("2006-01-02"), time.Now().UTC().Add(-48*time.Hour).Format("2006-01-02"), 1))
	if err := os.WriteFile(manifest, data, 0o644); err != nil {
		t.Fatalf("rewrite old manifest: %v", err)
	}
	if err := store.CleanupExpired(24 * time.Hour); err != nil {
		t.Fatalf("cleanup expired: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".multipart", expiredID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired temp dir should be removed, stat err=%v", err)
	}
}

func TestMultipartRejectsNonUUIDUploadID(t *testing.T) {
	root := t.TempDir()
	store, err := NewMultipartStore(root, filepath.Join(root, ".multipart"), ".s3meta")
	if err != nil {
		t.Fatalf("new multipart store: %v", err)
	}

	if _, err := store.UploadPart("../escape", 1, strings.NewReader("part")); !errors.Is(err, ErrNoSuchUpload) {
		t.Fatalf("upload part with invalid upload id err=%v, want ErrNoSuchUpload", err)
	}
	if err := store.Abort("."); !errors.Is(err, ErrNoSuchUpload) {
		t.Fatalf("abort with invalid upload id err=%v, want ErrNoSuchUpload", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".multipart")); err != nil {
		t.Fatalf("multipart root should remain intact: %v", err)
	}
}

func TestMultipartValidateTargetRejectsMismatchedBucketOrKey(t *testing.T) {
	root := t.TempDir()
	store, err := NewMultipartStore(root, filepath.Join(root, ".multipart"), ".s3meta")
	if err != nil {
		t.Fatalf("new multipart store: %v", err)
	}
	uploadID, err := store.Create("test-bucket", "dir/a.bin", "", nil, nil)
	if err != nil {
		t.Fatalf("create multipart: %v", err)
	}

	if err := store.ValidateTarget(uploadID, "test-bucket", "dir/a.bin"); err != nil {
		t.Fatalf("validate matching target: %v", err)
	}
	if err := store.ValidateTarget(uploadID, "other-bucket", "dir/a.bin"); !errors.Is(err, ErrNoSuchUpload) {
		t.Fatalf("validate mismatched bucket err=%v, want ErrNoSuchUpload", err)
	}
	if err := store.ValidateTarget(uploadID, "test-bucket", "dir/b.bin"); !errors.Is(err, ErrNoSuchUpload) {
		t.Fatalf("validate mismatched key err=%v, want ErrNoSuchUpload", err)
	}
}

func expectedMultipartETag(parts ...string) string {
	var raw []byte
	for _, part := range parts {
		sum := md5.Sum([]byte(part))
		raw = append(raw, sum[:]...)
	}
	full := md5.Sum(raw)
	return hex.EncodeToString(full[:]) + "-" + strconv.Itoa(len(parts))
}
