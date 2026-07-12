package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReconcileBucketScansAndDeletesOrphans(t *testing.T) {
	root := t.TempDir()
	bucketPath := filepath.Join(root, "test-bucket")
	if err := os.MkdirAll(filepath.Join(bucketPath, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucketPath, "one.txt"), []byte("123"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucketPath, "one.txt.s3meta"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(bucketPath, "nested", "gone.txt.s3meta")
	if err := os.WriteFile(orphan, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucketPath, "state.sqlite"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := ReconcileBucket(root, "test-bucket", ".s3meta")
	if err != nil {
		t.Fatal(err)
	}
	if report.ObjectCount != 1 || report.ScannedBytes != 3 || report.OrphanSidecarCount() != 1 {
		t.Fatalf("report = %+v, orphan count = %d", report, report.OrphanSidecarCount())
	}
	if len(report.OrphanSidecars) != 1 || report.OrphanSidecars[0] != "nested/gone.txt.s3meta" {
		t.Fatalf("samples = %+v", report.OrphanSidecars)
	}
	deleted, err := report.DeleteOrphanSidecars()
	if err != nil || deleted != 1 {
		t.Fatalf("delete = %d, %v", deleted, err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan stat = %v", err)
	}
}

func TestReconcileBucketErrors(t *testing.T) {
	if _, err := ReconcileBucket(t.TempDir(), "Bad", ".s3meta"); !errors.Is(err, ErrInvalidBucketName) {
		t.Fatalf("invalid bucket error = %v", err)
	}
	if _, err := ReconcileBucket(t.TempDir(), "missing-bucket", ".s3meta"); !errors.Is(err, ErrNoSuchBucket) {
		t.Fatalf("missing bucket error = %v", err)
	}
}

func TestReconcileBucketEmpty(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "empty-bucket"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := ReconcileBucket(root, "empty-bucket", ".s3meta")
	if err != nil {
		t.Fatal(err)
	}
	if report.ObjectCount != 0 || report.ScannedBytes != 0 || report.OrphanSidecarCount() != 0 {
		t.Fatalf("empty report = %+v", report)
	}
}
