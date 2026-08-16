package panel

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"gorm.io/gorm"
)

// openTestDB opens a temp-file SQLite DB and migrates the panel schema.
func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := db.Open("sqlite", filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := Migrate(gdb); err != nil {
		t.Fatalf("migrate panel schema: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("get sqlite db handle: %v", err)
	}
	// Close the pool before t.TempDir removes the SQLite directory. Control-plane
	// cleanup can finish after the WebSocket closes and leave WAL sidecars behind.
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close sqlite db: %v", err)
		}
	})
	return gdb
}

func TestMigrateCreatesTablesAndIndexes(t *testing.T) {
	gdb := openTestDB(t)
	for _, table := range expectedTables {
		if !gdb.Migrator().HasTable(table.model) {
			t.Fatalf("expected table %q to exist", table.name)
		}
	}
	for _, index := range expectedIndexes {
		if !gdb.Migrator().HasIndex(index.model, index.name) {
			t.Fatalf("expected index %q on table %q to exist", index.name, index.table)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	gdb := openTestDB(t)
	// A second migrate on the same schema must not error.
	if err := Migrate(gdb); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestMigrateNilHandle(t *testing.T) {
	if err := Migrate(nil); err == nil {
		t.Fatal("expected error for nil db handle")
	}
}

// TestMigrateAddsActivatedAtIncrementally verifies that adding the ActivatedAt
// column is an additive migration: an old-schema DB (without the column) can be
// upgraded in place, existing rows are preserved, and the new column defaults
// to NULL.
func TestMigrateAddsActivatedAtIncrementally(t *testing.T) {
	gdb, err := db.Open("sqlite", filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	// Create the node_certs table with the OLD schema (no activated_at column).
	if err := gdb.Exec(`CREATE TABLE node_certs (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		node_id         INTEGER NOT NULL,
		fingerprint     TEXT NOT NULL UNIQUE,
		serial          TEXT NOT NULL,
		not_before      DATETIME,
		not_after       DATETIME,
		revoked         BOOLEAN NOT NULL DEFAULT 0,
		revoked_at      DATETIME,
		created_at      DATETIME
	)`).Error; err != nil {
		t.Fatalf("create old node_certs table: %v", err)
	}
	// Also create the nodes table so Migrate's AutoMigrate for Node doesn't fail
	// and the expectedTables validation passes for all models.
	if err := gdb.AutoMigrate(&Node{}); err != nil {
		t.Fatalf("auto migrate node: %v", err)
	}
	// Seed a row in the old schema.
	now := time.Now().UTC()
	seed := NodeCert{
		NodeID:      1,
		Fingerprint: "abc123",
		Serial:      "1",
		NotBefore:   now,
		NotAfter:    now.Add(24 * time.Hour),
	}
	if err := gdb.Exec(`INSERT INTO node_certs (node_id, fingerprint, serial, not_before, not_after, revoked, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		seed.NodeID, seed.Fingerprint, seed.Serial, seed.NotBefore, seed.NotAfter, false, now).Error; err != nil {
		t.Fatalf("seed old node_certs row: %v", err)
	}

	// Run Migrate — it should add the activated_at column additively.
	if err := Migrate(gdb); err != nil {
		t.Fatalf("migrate after old schema: %v", err)
	}

	// The column must exist.
	if !gdb.Migrator().HasColumn(&NodeCert{}, "activated_at") {
		t.Fatal("expected activated_at column to exist after migration")
	}

	// The old row must be preserved and activated_at must be NULL.
	var row NodeCert
	if err := gdb.Where("fingerprint = ?", "abc123").First(&row).Error; err != nil {
		t.Fatalf("query seeded row: %v", err)
	}
	if row.ActivatedAt != nil {
		t.Fatalf("expected activated_at to be NULL for old row, got %v", row.ActivatedAt)
	}
	if row.Serial != "1" || row.Revoked != false {
		t.Fatalf("old row data not preserved: serial=%q revoked=%v", row.Serial, row.Revoked)
	}
}
