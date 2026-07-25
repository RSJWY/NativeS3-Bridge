package logging

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
)

func TestSetupWritesStdoutFileAndRing(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "logs")
	stdout := captureSetupStdout(t)
	runtime, err := Setup("info", config.LogConfig{
		Dir:        directory,
		MaxSizeMB:  1,
		MaxBackups: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.File != filepath.Join(directory, config.DefaultLogFileName) {
		t.Fatalf("runtime file = %q", runtime.File)
	}

	slog.Debug("filtered setup record")
	slog.Info("shared setup record", "bucket", "media", "secret_token", "hidden")
	if err := stdout.Sync(); err != nil {
		t.Fatal(err)
	}
	stdoutData, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	fileData, err := os.ReadFile(runtime.File)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"stdout": stdoutData, "file": fileData} {
		if !bytes.Contains(data, []byte("shared setup record")) || bytes.Contains(data, []byte("filtered setup record")) {
			t.Fatalf("%s output = %q", name, data)
		}
	}
	entries := runtime.Ring.Snapshot(10, "INFO", "media")
	if len(entries) != 1 || entries[0].Message != "shared setup record" {
		t.Fatalf("ring entries = %+v", entries)
	}
	if _, exists := entries[0].Attrs["secret_token"]; exists {
		t.Fatalf("ring leaked sensitive attr: %+v", entries[0].Attrs)
	}

	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o027 != 0 {
		t.Fatalf("log directory permissions = %o, want no group write or other access", info.Mode().Perm())
	}
}

func TestSetupWithoutFileKeepsStdoutAndRing(t *testing.T) {
	stdout := captureSetupStdout(t)
	runtime, err := Setup("error", config.LogConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.File != "" {
		t.Fatalf("runtime file = %q, want disabled", runtime.File)
	}
	slog.Warn("filtered warning")
	slog.Error("memory-only error")
	if err := stdout.Sync(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "memory-only error") || strings.Contains(string(data), "filtered warning") {
		t.Fatalf("stdout = %q", data)
	}
	entries := runtime.Ring.Snapshot(10, "", "")
	if len(entries) != 1 || entries[0].Message != "memory-only error" {
		t.Fatalf("ring entries = %+v", entries)
	}
}

func TestSetupRejectsUnusablePath(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Setup("info", config.LogConfig{File: filepath.Join(parent, "app.log"), MaxSizeMB: 1}); err == nil {
		t.Fatal("Setup succeeded with a file as the parent directory")
	}
}

func TestSetupDoesNotRotateExistingFileOnStartup(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "app.log")
	existing := bytes.Repeat([]byte("x"), 1100*1024)
	if err := os.WriteFile(path, existing, 0o600); err != nil {
		t.Fatal(err)
	}
	captureSetupStdout(t)
	runtime, err := Setup("info", config.LogConfig{File: path, MaxSizeMB: 1, MaxBackups: 2})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.File != path {
		t.Fatalf("runtime file = %q, want %q", runtime.File, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, existing) {
		t.Fatalf("existing log changed during setup: got %d bytes, want %d", len(data), len(existing))
	}
	backups, err := filepath.Glob(filepath.Join(directory, "app-*.log*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 0 {
		t.Fatalf("setup created rotated backups: %v", backups)
	}
}

func TestSetupRotationHonorsBackupAndCompressionOptions(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "app.log")
	captureSetupStdout(t)
	if _, err := Setup("info", config.LogConfig{
		File:       path,
		MaxSizeMB:  1,
		MaxBackups: 1,
		Compress:   true,
	}); err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 700*1024)
	for index := 0; index < 4; index++ {
		slog.Info(payload, "index", index)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		compressed, err := filepath.Glob(filepath.Join(directory, "app-*.log.gz"))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := filepath.Glob(filepath.Join(directory, "app-*.log"))
		if err != nil {
			t.Fatal(err)
		}
		if len(compressed) == 1 && len(plain) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rotated files = compressed %v plain %v, want one compressed backup", compressed, plain)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSetupRotationHonorsMaxAge(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "app.log")
	oldBackup := filepath.Join(directory, "app-2000-01-01T00-00-00.000.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 700*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldBackup, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	captureSetupStdout(t)
	if _, err := Setup("info", config.LogConfig{
		File:       path,
		MaxSizeMB:  1,
		MaxBackups: 0,
		MaxAgeDays: 1,
	}); err != nil {
		t.Fatal(err)
	}
	slog.Info(strings.Repeat("x", 700*1024))

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := os.Stat(oldBackup)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("old backup %q was not pruned by max_age_days", oldBackup)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestParseLevelPreservesHistoricalFallback(t *testing.T) {
	tests := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		" WARN ":  slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelInfo,
		"warning": slog.LevelInfo,
		"unknown": slog.LevelInfo,
	}
	for input, want := range tests {
		if got := ParseLevel(input); got != want {
			t.Fatalf("ParseLevel(%q) = %v, want %v", input, got, want)
		}
	}
}

func captureSetupStdout(t *testing.T) *os.File {
	t.Helper()
	originalStdout := os.Stdout
	originalLogger := slog.Default()
	file, err := os.CreateTemp(t.TempDir(), "stdout-*.log")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = file
	t.Cleanup(func() {
		slog.SetDefault(originalLogger)
		os.Stdout = originalStdout
		_ = file.Close()
	})
	return file
}
