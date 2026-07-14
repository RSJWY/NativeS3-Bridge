package nodeagent

import (
	"path/filepath"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"gorm.io/gorm"
)

// openNodeDB opens a temp-file SQLite node DB with the base schema plus the
// additive agent state tables.
func openNodeDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := dbpkg.Open("sqlite", filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatalf("open node db: %v", err)
	}
	if err := dbpkg.Migrate(gdb); err != nil {
		t.Fatalf("migrate base schema: %v", err)
	}
	if err := MigrateState(gdb); err != nil {
		t.Fatalf("migrate agent state: %v", err)
	}
	return gdb
}

func TestApplyCreatesUpdatesAndDeletes(t *testing.T) {
	gdb := openNodeDB(t)
	ex := NewExecutor(gdb, nil)

	// Seed a pre-existing credential the panel will delete and a bucket it keeps.
	if err := gdb.Create(&dbpkg.Credential{AccessKey: "OLD", SecretKey: "s", Status: "enabled"}).Error; err != nil {
		t.Fatalf("seed old cred: %v", err)
	}

	state := controlproto.DesiredState{
		Credentials: []controlproto.DesiredCredential{
			{AccessKey: "AK1", SecretKey: "sk1", Name: "one", Status: "enabled", QuotaBytes: 100},
		},
		Buckets: []controlproto.DesiredBucket{
			{Name: "b1", ACL: "private"},
		},
		Webhooks: []controlproto.DesiredWebhook{
			{URL: "https://hook", Events: "put", Enabled: true},
		},
	}
	hash, err := ex.Apply(state)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if hash == "" {
		t.Fatal("expected non-empty content hash")
	}

	// OLD deleted, AK1 present.
	var count int64
	gdb.Model(&dbpkg.Credential{}).Where("access_key = ?", "OLD").Count(&count)
	if count != 0 {
		t.Fatal("OLD credential should have been deleted")
	}
	var ak1 dbpkg.Credential
	if err := gdb.Where("access_key = ?", "AK1").First(&ak1).Error; err != nil {
		t.Fatalf("AK1 should exist: %v", err)
	}
	if ak1.SecretKey != "sk1" || ak1.QuotaBytes != 100 {
		t.Fatalf("AK1 not applied correctly: %+v", ak1)
	}

	// The applied hash must equal the panel's hash for the same logical content.
	if want := state.ContentHash(); want != hash {
		t.Fatalf("local hash %s != panel hash %s", hash, want)
	}
}

func TestApplyPreservesUsedBytes(t *testing.T) {
	gdb := openNodeDB(t)
	ex := NewExecutor(gdb, nil)

	// Existing credential with observed UsedBytes (node-owned observed state).
	if err := gdb.Create(&dbpkg.Credential{AccessKey: "AK1", SecretKey: "old", Status: "enabled", UsedBytes: 4096}).Error; err != nil {
		t.Fatalf("seed cred: %v", err)
	}

	// Panel pushes a new secret for the same key; UsedBytes must be preserved.
	_, err := ex.Apply(controlproto.DesiredState{
		Credentials: []controlproto.DesiredCredential{
			{AccessKey: "AK1", SecretKey: "rotated", Status: "enabled", QuotaBytes: 999},
		},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var ak1 dbpkg.Credential
	if err := gdb.Where("access_key = ?", "AK1").First(&ak1).Error; err != nil {
		t.Fatalf("load AK1: %v", err)
	}
	if ak1.SecretKey != "rotated" {
		t.Fatalf("secret not rotated: %q", ak1.SecretKey)
	}
	if ak1.UsedBytes != 4096 {
		t.Fatalf("UsedBytes should be preserved, got %d", ak1.UsedBytes)
	}
}

func TestLocalContentHashMatchesPanel(t *testing.T) {
	gdb := openNodeDB(t)
	ex := NewExecutor(gdb, nil)

	state := controlproto.DesiredState{
		Credentials: []controlproto.DesiredCredential{
			{AccessKey: "AK2", SecretKey: "s2", Status: "enabled"},
			{AccessKey: "AK1", SecretKey: "s1", Status: "enabled"},
		},
		Buckets: []controlproto.DesiredBucket{{Name: "b1", ACL: "private"}},
	}
	applied, err := ex.Apply(state)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Re-reading local state and hashing must be stable/equal.
	reread, err := ex.LocalContentHash()
	if err != nil {
		t.Fatalf("local hash: %v", err)
	}
	if reread != applied {
		t.Fatalf("hash not stable: %s != %s", reread, applied)
	}
	if reread != state.ContentHash() {
		t.Fatalf("local hash %s != panel hash %s", reread, state.ContentHash())
	}
}

func TestSaveAndLoadMeta(t *testing.T) {
	gdb := openNodeDB(t)
	meta, err := LoadMeta(gdb)
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}
	if meta.AppliedVersion != 0 {
		t.Fatalf("fresh applied version = %d, want 0", meta.AppliedVersion)
	}
	if err := SaveMeta(gdb, 5, "hash5"); err != nil {
		t.Fatalf("save meta: %v", err)
	}
	meta, err = LoadMeta(gdb)
	if err != nil {
		t.Fatalf("reload meta: %v", err)
	}
	if meta.AppliedVersion != 5 || meta.ContentHash != "hash5" {
		t.Fatalf("meta not persisted: %+v", meta)
	}
}
