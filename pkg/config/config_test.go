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
	if cfg.WebAdmin.LoginMaxFailures != 5 {
		t.Fatalf("login max failures = %d, want 5", cfg.WebAdmin.LoginMaxFailures)
	}
	if cfg.WebAdmin.LoginLockoutWindow != 15*time.Minute {
		t.Fatalf("login lockout window = %v, want 15m", cfg.WebAdmin.LoginLockoutWindow)
	}
	if cfg.RateLimit.AnonymousRPS != 10 {
		t.Fatalf("anonymous rps = %v, want 10", cfg.RateLimit.AnonymousRPS)
	}
	if cfg.RateLimit.AnonymousBurst != 20 {
		t.Fatalf("anonymous burst = %d, want 20", cfg.RateLimit.AnonymousBurst)
	}
	if cfg.RateLimit.TrustForwarded {
		t.Fatal("trust_forwarded default/example should be false")
	}
}

func TestEffectiveAdminTLSInheritsWhenUnset(t *testing.T) {
	serverCfg := ServerConfig{TLS: TLSConfig{Enabled: true, CertFile: "s3.crt", KeyFile: "s3.key"}}

	got := serverCfg.EffectiveAdminTLS()

	if got != serverCfg.TLS {
		t.Fatalf("effective admin tls = %+v, want inherited %+v", got, serverCfg.TLS)
	}
}

func TestEffectiveAdminTLSUsesExplicitAdminConfig(t *testing.T) {
	serverCfg := ServerConfig{
		TLS:      TLSConfig{Enabled: true, CertFile: "s3.crt", KeyFile: "s3.key"},
		AdminTLS: &TLSConfig{Enabled: false},
	}

	got := serverCfg.EffectiveAdminTLS()

	if got.Enabled || got.CertFile != "" || got.KeyFile != "" {
		t.Fatalf("effective admin tls = %+v, want explicit disabled admin tls", got)
	}
}

func TestValidateRejectsEnabledTLSMissingFiles(t *testing.T) {
	base := Config{
		Storage:  StorageConfig{DataRoot: t.TempDir()},
		Database: DatabaseConfig{Driver: "sqlite", DSN: "test.db"},
		WebAdmin: WebAdminConfig{SessionSecret: "change-me-32bytes-random"},
	}

	cfg := base
	cfg.Server.TLS.Enabled = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "server.tls") {
		t.Fatalf("s3 tls validation error = %v, want server.tls error", err)
	}

	cfg = base
	cfg.Server.AdminTLS = &TLSConfig{Enabled: true}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "server.admin_tls") {
		t.Fatalf("admin tls validation error = %v, want server.admin_tls error", err)
	}

	cfg = base
	cfg.Server.AdminTLS = &TLSConfig{Enabled: true, CertFile: "admin.crt", KeyFile: "admin.key"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid explicit admin tls returned error: %v", err)
	}
}
