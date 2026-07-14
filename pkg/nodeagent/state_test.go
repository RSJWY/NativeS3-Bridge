package nodeagent

import (
	"path/filepath"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"gorm.io/gorm"
)

func openNodeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := db.Open("sqlite", filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return gdb
}

// TestMigrateStateIsAdditive is the safety-net C assertion: the agent state
// migration only ADDS tables and never touches existing base tables or their
// rows. We seed the base schema with a credential + bucket, run MigrateState,
// and verify the base rows are intact and the new tables exist.
func TestMigrateStateIsAdditive(t *testing.T) {
	gdb := openNodeTestDB(t)
	// Base node schema first (as cmd/node does via db.MigrateConfigured).
	if err := db.Migrate(gdb); err != nil {
		t.Fatalf("base migrate: %v", err)
	}
	// Seed pre-existing business rows the monolith would have written.
	seedCred := dbpkg.Credential{AccessKey: "AKPRE", SecretKey: "sk", Status: "enabled"}
	if err := gdb.Create(&seedCred).Error; err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if err := gdb.Create(&dbpkg.Bucket{Name: "prebucket", ACL: "private"}).Error; err != nil {
		t.Fatalf("seed bucket: %v", err)
	}

	// Now the additive agent-state migration.
	if err := MigrateState(gdb); err != nil {
		t.Fatalf("MigrateState: %v", err)
	}

	// Base rows must be untouched.
	var cred dbpkg.Credential
	if err := gdb.Where("access_key = ?", "AKPRE").First(&cred).Error; err != nil {
		t.Fatalf("pre-existing credential lost: %v", err)
	}
	if cred.SecretKey != "sk" {
		t.Fatalf("credential mutated: secret = %q", cred.SecretKey)
	}
	var bucketCount int64
	gdb.Model(&dbpkg.Bucket{}).Where("name = ?", "prebucket").Count(&bucketCount)
	if bucketCount != 1 {
		t.Fatalf("pre-existing bucket lost")
	}

	// New tables must exist.
	if !gdb.Migrator().HasTable(&AgentMeta{}) {
		t.Fatal("agent_meta table missing after MigrateState")
	}
	if !gdb.Migrator().HasTable(&AppliedTask{}) {
		t.Fatal("applied_tasks table missing after MigrateState")
	}

	// MigrateState is idempotent.
	if err := MigrateState(gdb); err != nil {
		t.Fatalf("second MigrateState: %v", err)
	}
}

// TestMetaLifecycle covers LoadMeta creating a zero row and SaveMeta upserting.
func TestMetaLifecycle(t *testing.T) {
	gdb := openNodeTestDB(t)
	if err := db.Migrate(gdb); err != nil {
		t.Fatalf("base migrate: %v", err)
	}
	if err := MigrateState(gdb); err != nil {
		t.Fatalf("MigrateState: %v", err)
	}
	meta, err := LoadMeta(gdb)
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	if meta.AppliedVersion != 0 {
		t.Fatalf("fresh applied version = %d, want 0", meta.AppliedVersion)
	}
	if err := SaveMeta(gdb, 5, "hash5"); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}
	meta, _ = LoadMeta(gdb)
	if meta.AppliedVersion != 5 || meta.ContentHash != "hash5" {
		t.Fatalf("after save: %+v", meta)
	}
}
