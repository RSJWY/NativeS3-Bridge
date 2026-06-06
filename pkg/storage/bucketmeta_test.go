package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"gorm.io/gorm"
)

func TestBucketStoreCreateGetACLAndList(t *testing.T) {
	store, gdb, root := newTestBucketStore(t)

	if err := store.Create("test-bucket"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := store.Create("test-bucket"); err != nil {
		t.Fatalf("idempotent create bucket: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "test-bucket")); err != nil {
		t.Fatalf("bucket directory missing: %v", err)
	}
	var count int64
	if err := gdb.Model(&db.Bucket{}).Where("name = ?", "test-bucket").Count(&count).Error; err != nil {
		t.Fatalf("count buckets: %v", err)
	}
	if count != 1 {
		t.Fatalf("bucket row count = %d, want 1", count)
	}

	acl, exists, err := store.GetACL("test-bucket")
	if err != nil {
		t.Fatalf("get acl: %v", err)
	}
	if !exists || acl != ACLPrivate {
		t.Fatalf("acl = %q exists=%v, want private true", acl, exists)
	}

	buckets, err := store.List()
	if err != nil {
		t.Fatalf("list buckets: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Name != "test-bucket" || buckets[0].ACL != ACLPrivate {
		t.Fatalf("buckets = %+v, want one private test-bucket", buckets)
	}
}

func TestBucketStoreGetACLHistoricalBucketNegativeCache(t *testing.T) {
	store, gdb, root := newTestBucketStore(t)
	if err := os.MkdirAll(filepath.Join(root, "legacy-bucket"), 0o755); err != nil {
		t.Fatalf("mkdir legacy bucket: %v", err)
	}

	acl, exists, err := store.GetACL("legacy-bucket")
	if err != nil {
		t.Fatalf("get acl legacy: %v", err)
	}
	if exists || acl != "" {
		t.Fatalf("legacy acl = %q exists=%v, want empty false", acl, exists)
	}

	if err := gdb.Create(&db.Bucket{Name: "legacy-bucket", ACL: ACLPublicRead}).Error; err != nil {
		t.Fatalf("insert legacy bucket row: %v", err)
	}
	acl, exists, err = store.GetACL("legacy-bucket")
	if err != nil {
		t.Fatalf("get cached legacy acl: %v", err)
	}
	if exists || acl != "" {
		t.Fatalf("cached legacy acl = %q exists=%v, want empty false before invalidation", acl, exists)
	}

	store.Invalidate("legacy-bucket")
	acl, exists, err = store.GetACL("legacy-bucket")
	if err != nil {
		t.Fatalf("get invalidated legacy acl: %v", err)
	}
	if !exists || acl != ACLPublicRead {
		t.Fatalf("invalidated legacy acl = %q exists=%v, want public-read true", acl, exists)
	}
}

func TestBucketStoreSetACLInvalidatesCache(t *testing.T) {
	store, _, _ := newTestBucketStore(t)
	if err := store.Create("test-bucket"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if _, _, err := store.GetACL("test-bucket"); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	if err := store.SetACL("test-bucket", "authenticated-read"); !errors.Is(err, ErrInvalidACL) {
		t.Fatalf("invalid acl error = %v, want ErrInvalidACL", err)
	}
	if err := store.SetACL("missing-bucket", ACLPublicRead); !errors.Is(err, ErrNoSuchBucket) {
		t.Fatalf("missing bucket set acl error = %v, want ErrNoSuchBucket", err)
	}
	if err := store.SetACL("test-bucket", ACLPublicRead); err != nil {
		t.Fatalf("set acl: %v", err)
	}
	acl, exists, err := store.GetACL("test-bucket")
	if err != nil {
		t.Fatalf("get updated acl: %v", err)
	}
	if !exists || acl != ACLPublicRead {
		t.Fatalf("updated acl = %q exists=%v, want public-read true", acl, exists)
	}
}

func TestBucketStoreDeleteEmptyAndRejectNonEmpty(t *testing.T) {
	store, gdb, root := newTestBucketStore(t)
	if err := store.Create("empty-bucket"); err != nil {
		t.Fatalf("create empty bucket: %v", err)
	}
	if err := store.Delete("empty-bucket"); err != nil {
		t.Fatalf("delete empty bucket: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "empty-bucket")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted bucket dir stat error = %v, want not exist", err)
	}
	var count int64
	if err := gdb.Model(&db.Bucket{}).Where("name = ?", "empty-bucket").Count(&count).Error; err != nil {
		t.Fatalf("count deleted bucket: %v", err)
	}
	if count != 0 {
		t.Fatalf("deleted bucket row count = %d, want 0", count)
	}

	if err := store.Create("full-bucket"); err != nil {
		t.Fatalf("create full bucket: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "full-bucket", "object.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write object fixture: %v", err)
	}
	if err := store.Delete("full-bucket"); !errors.Is(err, ErrBucketNotEmpty) {
		t.Fatalf("delete full bucket error = %v, want ErrBucketNotEmpty", err)
	}
}

func newTestBucketStore(t *testing.T) (*BucketStore, *gorm.DB, string) {
	t.Helper()
	root := t.TempDir()
	gdb, err := db.Open("sqlite", filepath.Join(t.TempDir(), "natives3.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.Migrate(gdb); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewBucketStore(gdb, root, time.Hour), gdb, root
}
