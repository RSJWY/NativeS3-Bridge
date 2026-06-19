package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"
)

func TestMigrateCreatesTablesAndIndexes(t *testing.T) {
	gdb := openSQLiteTestDB(t, filepath.Join(t.TempDir(), "natives3.db"))
	if err := Migrate(gdb); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	for _, table := range expectedTables {
		if !gdb.Migrator().HasTable(table.model) {
			t.Fatalf("expected table %q to exist", table.name)
		}
	}
	for _, index := range expectedIndexes {
		if !gdb.Migrator().HasIndex(index.model, index.name) {
			t.Fatalf("expected index %q to exist", index.name)
		}
	}
}

func TestMigrateConfiguredSQLiteBacksUpExistingDataAndPreservesRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "natives3.db")
	gdb := openSQLiteTestDB(t, dbPath)
	if err := Migrate(gdb); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	seedMigrationSafetyRows(t, gdb)
	closeTestDB(t, gdb)

	gdb = openSQLiteTestDB(t, dbPath)
	if err := MigrateConfigured("sqlite", dbPath, gdb); err != nil {
		t.Fatalf("configured migrate: %v", err)
	}
	assertMigrationSafetyRows(t, gdb)

	backups := sqliteBackupFiles(t, dbPath)
	if len(backups) != 1 {
		t.Fatalf("backup count = %d, want 1: %v", len(backups), backups)
	}

	backupDB := openSQLiteTestDB(t, backups[0])
	assertMigrationSafetyRows(t, backupDB)
}

func TestMigrateConfiguredSQLiteNewDatabaseSkipsBackup(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "natives3.db")
	gdb := openSQLiteTestDB(t, dbPath)
	if err := MigrateConfigured("sqlite", dbPath, gdb); err != nil {
		t.Fatalf("configured migrate: %v", err)
	}

	backups := sqliteBackupFiles(t, dbPath)
	if len(backups) != 0 {
		t.Fatalf("backup count = %d, want 0: %v", len(backups), backups)
	}
}

func TestMigrateConfiguredSQLiteMemoryDatabaseSkipsFileBackup(t *testing.T) {
	gdb := openSQLiteTestDB(t, "file::memory:?cache=shared")
	if err := MigrateConfigured("sqlite", "file::memory:?cache=shared", gdb); err != nil {
		t.Fatalf("configured migrate: %v", err)
	}
}

func TestMigrateConfiguredSQLiteCorruptDatabaseFailsPreflight(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "natives3.db")
	if err := os.WriteFile(dbPath, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatalf("write corrupt db: %v", err)
	}

	gdb := openSQLiteTestDB(t, dbPath)
	err := MigrateConfigured("sqlite", dbPath, gdb)
	if err == nil {
		t.Fatal("configured migrate succeeded for corrupt sqlite database")
	}
	if !strings.Contains(err.Error(), "sqlite preflight integrity check") {
		t.Fatalf("error = %q, want preflight integrity context", err)
	}

	backups := sqliteBackupFiles(t, dbPath)
	if len(backups) != 0 {
		t.Fatalf("backup count = %d, want 0 after preflight failure: %v", len(backups), backups)
	}
}

func TestValidateSchemaDetectsMissingIndex(t *testing.T) {
	gdb := openSQLiteTestDB(t, filepath.Join(t.TempDir(), "natives3.db"))
	if err := Migrate(gdb); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	if err := gdb.Exec("DROP INDEX idx_credentials_access_key").Error; err != nil {
		t.Fatalf("drop index: %v", err)
	}

	err := validateSchema(gdb)
	if err == nil {
		t.Fatal("validateSchema succeeded with a missing index")
	}
	if !strings.Contains(err.Error(), "idx_credentials_access_key") {
		t.Fatalf("error = %q, want missing index name", err)
	}
}

func TestSQLiteDatabasePathForBackup(t *testing.T) {
	tests := []struct {
		name     string
		dsn      string
		wantPath string
		wantOK   bool
	}{
		{name: "plain path", dsn: "natives3.db", wantPath: "natives3.db", wantOK: true},
		{name: "absolute file uri", dsn: "file:///tmp/natives3.db?mode=rwc", wantPath: "/tmp/natives3.db", wantOK: true},
		{name: "relative file uri", dsn: "file:natives3.db?cache=shared", wantPath: "natives3.db", wantOK: true},
		{name: "memory", dsn: ":memory:", wantOK: false},
		{name: "memory uri", dsn: "file::memory:?cache=shared", wantOK: false},
		{name: "named memory uri", dsn: "file:memdb1?mode=memory&cache=shared", wantOK: false},
		{name: "remote file uri", dsn: "file://db.example.test/tmp/natives3.db", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath, gotOK, err := sqliteDatabasePathForBackup(tt.dsn)
			if err != nil {
				t.Fatalf("sqliteDatabasePathForBackup: %v", err)
			}
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotPath != tt.wantPath {
				t.Fatalf("path = %q, want %q", gotPath, tt.wantPath)
			}
		})
	}
}

func openSQLiteTestDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	gdb, err := Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	return gdb
}

func closeTestDB(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("database handle: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
}

func seedMigrationSafetyRows(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	credential := Credential{AccessKey: "AKIAUPGRADE", SecretKey: "SECRET", Name: "upgrade", Status: "enabled", QuotaBytes: 100, UsedBytes: 7}
	if err := gdb.Create(&credential).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if err := gdb.Create(&RequestStat{CredentialID: credential.ID, Day: "2026-06-19", PutCount: 2, GetCount: 3, DeleteCount: 1, BytesIn: 10, BytesOut: 20}).Error; err != nil {
		t.Fatalf("create request stat: %v", err)
	}
	if err := gdb.Create(&HookConfig{URL: "http://127.0.0.1:18080/hook", Events: "ObjectCreated,ObjectDeleted", Enabled: true}).Error; err != nil {
		t.Fatalf("create hook config: %v", err)
	}
	if err := gdb.Create(&Bucket{Name: "upgrade-bucket", ACL: "private"}).Error; err != nil {
		t.Fatalf("create bucket: %v", err)
	}
}

func assertMigrationSafetyRows(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	var credential Credential
	if err := gdb.Where("access_key = ?", "AKIAUPGRADE").First(&credential).Error; err != nil {
		t.Fatalf("find credential: %v", err)
	}
	if credential.SecretKey != "SECRET" || credential.UsedBytes != 7 {
		t.Fatalf("credential = %+v, want seeded secret and usage", credential)
	}

	var stat RequestStat
	if err := gdb.Where("credential_id = ? AND day = ?", credential.ID, "2026-06-19").First(&stat).Error; err != nil {
		t.Fatalf("find request stat: %v", err)
	}
	if stat.PutCount != 2 || stat.GetCount != 3 || stat.DeleteCount != 1 || stat.BytesIn != 10 || stat.BytesOut != 20 {
		t.Fatalf("request stat = %+v, want seeded counters", stat)
	}

	var hook HookConfig
	if err := gdb.Where("url = ?", "http://127.0.0.1:18080/hook").First(&hook).Error; err != nil {
		t.Fatalf("find hook config: %v", err)
	}
	if hook.Events != "ObjectCreated,ObjectDeleted" || !hook.Enabled {
		t.Fatalf("hook = %+v, want seeded config", hook)
	}

	var bucket Bucket
	if err := gdb.Where("name = ?", "upgrade-bucket").First(&bucket).Error; err != nil {
		t.Fatalf("find bucket: %v", err)
	}
	if bucket.ACL != "private" {
		t.Fatalf("bucket ACL = %q, want private", bucket.ACL)
	}
}

func sqliteBackupFiles(t *testing.T, dbPath string) []string {
	t.Helper()
	backups, err := filepath.Glob(dbPath + ".pre-upgrade-*.bak*")
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	return backups
}
