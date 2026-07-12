package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
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

func TestSetupSlogWritesFileAndRing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "app.log")
	ring, err := setupSlog("info", config.LogConfig{File: path, MaxSizeMB: 1, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	slog.Info("file logging test", "bucket", "media")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "file logging test") {
		t.Fatalf("log file = %q", data)
	}
	entries := ring.Snapshot(1, "INFO", "media")
	if len(entries) != 1 || entries[0].Message != "file logging test" {
		t.Fatalf("ring entries = %+v", entries)
	}
}

func TestSetupSlogWritesDirectoryActiveFile(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "logs")
	if _, err := setupSlog("info", config.LogConfig{Dir: directory, MaxSizeMB: 1, MaxBackups: 1}); err != nil {
		t.Fatal(err)
	}
	slog.Info("directory logging test")
	path := filepath.Join(directory, config.DefaultLogFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "directory logging test") {
		t.Fatalf("log file = %q", data)
	}
}

func TestSetupSlogRejectsUnusablePath(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := setupSlog("info", config.LogConfig{File: filepath.Join(parent, "app.log"), MaxSizeMB: 1}); err == nil {
		t.Fatal("setupSlog succeeded with file as parent directory")
	}
}

func TestSetupSlogDoesNotRotateExistingFileOnStartup(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "app.log")
	if err := os.WriteFile(path, []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := setupSlog("info", config.LogConfig{File: path, MaxSizeMB: 1, MaxBackups: 2}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing\n" {
		t.Fatalf("existing log changed on setup: %q", data)
	}
	files, err := filepath.Glob(filepath.Join(directory, "app-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("setup created rotated backups: %v", files)
	}
}
