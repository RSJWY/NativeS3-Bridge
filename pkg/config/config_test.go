package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadRejectsMissingDataRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte(`
storage:
  data_root: ""
database:
  driver: "sqlite"
  dsn: "./natives3.db"
webadmin:
  session_secret: "change-me-32bytes-random"
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected missing data_root error")
	}
	if !strings.Contains(err.Error(), "storage.data_root is required") {
		t.Fatalf("expected data_root error, got %v", err)
	}
}

func TestLoadParsesMultipartDurations(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "configs", "config.example.yaml"))
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}
	if cfg.Storage.MultipartGCInterval != time.Hour {
		t.Fatalf("multipart gc interval = %v, want 1h", cfg.Storage.MultipartGCInterval)
	}
	if cfg.Storage.MultipartTTL != 24*time.Hour {
		t.Fatalf("multipart ttl = %v, want 24h", cfg.Storage.MultipartTTL)
	}
}
