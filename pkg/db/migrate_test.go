package db

import "testing"

func TestMigrateCreatesTables(t *testing.T) {
	gdb, err := Open("sqlite", t.TempDir()+"/natives3.db")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := Migrate(gdb); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	for _, table := range []string{"credentials", "request_stats", "hook_configs"} {
		if !gdb.Migrator().HasTable(table) {
			t.Fatalf("expected table %q to exist", table)
		}
	}
}
