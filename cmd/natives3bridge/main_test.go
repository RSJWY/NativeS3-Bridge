package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
)

func TestSeedCredentialRequiresExistingBucket(t *testing.T) {
	gdb, err := db.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(gdb); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	if err := seedCredential(gdb, "MISSING", "secret", 0, "missing-bucket"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing bucket seed error = %v, want does not exist", err)
	}
	if err := gdb.Create(&db.Bucket{Name: "existing-bucket", ACL: "private"}).Error; err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := seedCredential(gdb, "SCOPED", "secret", 0, "existing-bucket"); err != nil {
		t.Fatalf("seed scoped credential: %v", err)
	}
	if err := seedCredential(gdb, "GLOBAL", "secret", 0, ""); err != nil {
		t.Fatalf("seed global credential: %v", err)
	}
}
