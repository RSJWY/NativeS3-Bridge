package panel

import (
	"path/filepath"
	"testing"

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
