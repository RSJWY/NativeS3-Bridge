package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
)

func TestSetupLoggingReturnsEffectiveFileAndRing(t *testing.T) {
	originalLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(originalLogger) })

	directory := filepath.Join(t.TempDir(), "logs")
	runtime, err := setupLogging("info", config.LogConfig{Dir: directory, MaxSizeMB: 1, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	wantFile := filepath.Join(directory, config.DefaultLogFileName)
	if runtime.File != wantFile || runtime.Ring == nil {
		t.Fatalf("runtime = %+v, want file %q and ring", runtime, wantFile)
	}
	slog.Info("panel setup test")
	if _, err := os.Stat(wantFile); err != nil {
		t.Fatal(err)
	}
	if entries := runtime.Ring.Snapshot(1, "", ""); len(entries) != 1 || entries[0].Message != "panel setup test" {
		t.Fatalf("ring entries = %+v", entries)
	}
}
