package db

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"
)

var migrationModels = []any{&Credential{}, &RequestStat{}, &HookConfig{}, &Bucket{}}

var expectedTables = []struct {
	name  string
	model any
}{
	{name: "credentials", model: &Credential{}},
	{name: "request_stats", model: &RequestStat{}},
	{name: "hook_configs", model: &HookConfig{}},
	{name: "buckets", model: &Bucket{}},
}

var expectedIndexes = []struct {
	table string
	name  string
	model any
}{
	{table: "credentials", name: "idx_credentials_access_key", model: &Credential{}},
	{table: "request_stats", name: "idx_cred_day", model: &RequestStat{}},
	{table: "buckets", name: "idx_buckets_name", model: &Bucket{}},
}

func Migrate(gdb *gorm.DB) error {
	if gdb == nil {
		return errors.New("database handle is nil")
	}
	if err := gdb.AutoMigrate(migrationModels...); err != nil {
		return err
	}
	if err := validateSchema(gdb); err != nil {
		return fmt.Errorf("validate schema: %w", err)
	}
	return nil
}

func MigrateConfigured(driver, dsn string, gdb *gorm.DB) error {
	if gdb == nil {
		return errors.New("database handle is nil")
	}

	switch driver {
	case "sqlite":
		if err := checkSQLiteIntegrity(gdb); err != nil {
			return fmt.Errorf("sqlite preflight integrity check: %w", err)
		}
		backupPath, backedUp, err := backupSQLiteBeforeMigration(gdb, dsn, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("sqlite pre-upgrade backup: %w", err)
		}
		if backedUp {
			slog.Info("created sqlite pre-upgrade database backup", "path", backupPath)
		}
	case "mysql", "postgres":
	default:
		return fmt.Errorf("unsupported db driver: %q", driver)
	}

	if err := Migrate(gdb); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}

	if driver == "sqlite" {
		if err := checkSQLiteIntegrity(gdb); err != nil {
			return fmt.Errorf("sqlite postflight integrity check: %w", err)
		}
	}

	return nil
}

func validateSchema(gdb *gorm.DB) error {
	for _, table := range expectedTables {
		if !gdb.Migrator().HasTable(table.model) {
			return fmt.Errorf("missing table %q", table.name)
		}
	}
	for _, index := range expectedIndexes {
		if !gdb.Migrator().HasIndex(index.model, index.name) {
			return fmt.Errorf("missing index %q on table %q", index.name, index.table)
		}
	}
	return nil
}

func checkSQLiteIntegrity(gdb *gorm.DB) error {
	rows, err := gdb.Raw("PRAGMA integrity_check").Rows()
	if err != nil {
		return err
	}
	defer rows.Close()

	var results []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return err
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(results) == 1 && results[0] == "ok" {
		return nil
	}
	if len(results) == 0 {
		return errors.New("integrity_check returned no rows")
	}
	return fmt.Errorf("integrity_check failed: %s", strings.Join(results, "; "))
}

func backupSQLiteBeforeMigration(gdb *gorm.DB, dsn string, now time.Time) (string, bool, error) {
	dbPath, ok, err := sqliteDatabasePathForBackup(dsn)
	if err != nil || !ok {
		return "", false, err
	}

	info, err := os.Stat(dbPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		return "", false, fmt.Errorf("%q is a directory", dbPath)
	}
	if info.Size() == 0 {
		return "", false, nil
	}
	hasTables, err := sqliteHasUserTables(gdb)
	if err != nil {
		return "", false, err
	}
	if !hasTables {
		return "", false, nil
	}

	backupPath, err := nextSQLiteBackupPath(dbPath, now)
	if err != nil {
		return "", false, err
	}
	if err := gdb.Exec("VACUUM INTO ?", backupPath).Error; err != nil {
		return "", false, err
	}
	if err := os.Chmod(backupPath, info.Mode().Perm()); err != nil {
		return "", false, err
	}
	if err := syncFile(backupPath); err != nil {
		return "", false, err
	}
	backupInfo, err := os.Stat(backupPath)
	if err != nil {
		return "", false, err
	}
	if backupInfo.Size() == 0 {
		return "", false, fmt.Errorf("backup %q is empty", backupPath)
	}

	return backupPath, true, nil
}

func sqliteHasUserTables(gdb *gorm.DB) (bool, error) {
	var count int64
	if err := gdb.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'").Scan(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func sqliteDatabasePathForBackup(dsn string) (string, bool, error) {
	trimmed := strings.TrimSpace(dsn)
	if trimmed == "" {
		return "", false, errors.New("sqlite dsn is empty")
	}

	lower := strings.ToLower(trimmed)
	if lower == ":memory:" || strings.HasPrefix(lower, "file::memory:") {
		return "", false, nil
	}

	if !strings.HasPrefix(lower, "file:") {
		return trimmed, true, nil
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", false, fmt.Errorf("parse sqlite file dsn: %w", err)
	}
	if strings.EqualFold(parsed.Query().Get("mode"), "memory") {
		return "", false, nil
	}
	if parsed.Host != "" && parsed.Host != "localhost" {
		return "", false, nil
	}

	dbPath := parsed.Path
	if dbPath == "" {
		dbPath = parsed.Opaque
	}
	if dbPath == "" || dbPath == ":memory:" {
		return "", false, nil
	}
	dbPath, err = url.PathUnescape(dbPath)
	if err != nil {
		return "", false, fmt.Errorf("parse sqlite file path: %w", err)
	}

	return filepath.Clean(dbPath), true, nil
}

func nextSQLiteBackupPath(dbPath string, now time.Time) (string, error) {
	base := fmt.Sprintf("%s.pre-upgrade-%s.bak", dbPath, now.UTC().Format("20060102T150405Z"))
	for i := 0; i < 1000; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s.%d", base, i)
		}
		_, err := os.Stat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not choose unused sqlite backup path for %q", dbPath)
}

func syncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
