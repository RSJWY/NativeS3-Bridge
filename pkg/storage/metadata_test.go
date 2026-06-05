package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSidecarWriteReadDeleteAndMissingTolerance(t *testing.T) {
	objPath := filepath.Join(t.TempDir(), "test-bucket", "dir", "a.txt")
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		t.Fatalf("mkdir object dir: %v", err)
	}
	if err := os.WriteFile(objPath, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write object fixture: %v", err)
	}

	missing, exists, err := ReadSidecar(objPath, ".s3meta")
	if err != nil {
		t.Fatalf("read missing sidecar: %v", err)
	}
	if exists || missing.ETag != "" {
		t.Fatalf("missing sidecar should be tolerated, got exists=%t sidecar=%+v", exists, missing)
	}

	want := Sidecar{
		ETag:        "abc123",
		ContentType: "text/plain",
		Metadata:    map[string]string{"author": "jdoe", "team": "infra"},
		Tags:        map[string]string{"env": "prod"},
		Size:        5,
		UploadedAt:  "2026-06-05T00:00:00Z",
	}
	if err := WriteSidecar(objPath, ".s3meta", want); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	if _, err := os.Stat(objPath + ".s3meta.tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("atomic temp sidecar should not remain, stat err=%v", err)
	}

	got, exists, err := ReadSidecar(objPath, ".s3meta")
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if !exists {
		t.Fatal("sidecar should exist")
	}
	if got.ETag != want.ETag || got.ContentType != want.ContentType || got.Size != want.Size || got.Metadata["author"] != "jdoe" || got.Tags["env"] != "prod" {
		t.Fatalf("sidecar = %+v, want %+v", got, want)
	}

	if err := DeleteSidecar(objPath, ".s3meta"); err != nil {
		t.Fatalf("delete sidecar: %v", err)
	}
	_, exists, err = ReadSidecar(objPath, ".s3meta")
	if err != nil || exists {
		t.Fatalf("read deleted sidecar exists=%t err=%v", exists, err)
	}
}

func TestFileBackendMetadataSidecarAndExternalFileFallback(t *testing.T) {
	backend, err := NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if _, err := backend.PutObjectWithMetadata("test-bucket", "dir/a.txt", stringsReader("hello"), "text/plain", map[string]string{"author": "jdoe"}); err != nil {
		t.Fatalf("put with metadata: %v", err)
	}
	head, err := backend.HeadObject("test-bucket", "dir/a.txt")
	if err != nil {
		t.Fatalf("head object: %v", err)
	}
	if head.ContentType != "text/plain" || head.Metadata["author"] != "jdoe" {
		t.Fatalf("head metadata = content-type %q metadata %+v", head.ContentType, head.Metadata)
	}
	sidecarPath := filepath.Join(backend.root, "test-bucket", "dir", "a.txt.s3meta")
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("sidecar should exist: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(backend.root, "test-bucket"), 0o755); err != nil {
		t.Fatalf("mkdir bucket: %v", err)
	}
	externalPath := filepath.Join(backend.root, "test-bucket", "external.txt")
	if err := os.WriteFile(externalPath, []byte("external"), 0o644); err != nil {
		t.Fatalf("write external file: %v", err)
	}
	external, err := backend.HeadObject("test-bucket", "external.txt")
	if err != nil {
		t.Fatalf("head external file without sidecar: %v", err)
	}
	if external.ContentType != "text/plain; charset=utf-8" || len(external.Metadata) != 0 {
		t.Fatalf("external fallback content-type %q metadata %+v", external.ContentType, external.Metadata)
	}

	if err := backend.DeleteObject("test-bucket", "dir/a.txt"); err != nil {
		t.Fatalf("delete object: %v", err)
	}
	if _, err := os.Stat(sidecarPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sidecar should be deleted with object, stat err=%v", err)
	}
}
