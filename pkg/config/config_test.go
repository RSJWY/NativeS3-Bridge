package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validTestSessionSecret = "test-only-session-secret-32-bytes-minimum"

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
	examplePath := filepath.Join("..", "..", "configs", "config.example.yaml")
	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	data = []byte(strings.Replace(string(data), "replace-with-random-secret-at-least-32-bytes", validTestSessionSecret, 1))
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}
	if cfg.Storage.MultipartGCInterval != time.Hour {
		t.Fatalf("multipart gc interval = %v, want 1h", cfg.Storage.MultipartGCInterval)
	}
	if cfg.Storage.MultipartTTL != 24*time.Hour {
		t.Fatalf("multipart ttl = %v, want 24h", cfg.Storage.MultipartTTL)
	}
	if cfg.Storage.MultipartMaxPendingBytes != 10<<30 {
		t.Fatalf("multipart max pending bytes = %d, want 10 GiB", cfg.Storage.MultipartMaxPendingBytes)
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
	if !cfg.WebAdmin.Ops.PublicHealthz {
		t.Fatal("public_healthz default/example should be true")
	}
	if cfg.WebAdmin.Ops.PublicReadyz {
		t.Fatal("public_readyz default/example should be false")
	}
	if cfg.WebAdmin.Ops.PublicMetrics {
		t.Fatal("public_metrics default/example should be false")
	}
	if cfg.WebAdmin.Captcha.Timeout != 3*time.Second {
		t.Fatalf("captcha timeout = %v, want 3s", cfg.WebAdmin.Captcha.Timeout)
	}
}

func TestApplyDefaultsUsesLoopbackAdminAddress(t *testing.T) {
	cfg := Config{}
	cfg.applyDefaults()

	if cfg.Server.AdminAddr != "127.0.0.1:9001" {
		t.Fatalf("admin addr = %q, want loopback default", cfg.Server.AdminAddr)
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
		WebAdmin: WebAdminConfig{SessionSecret: validTestSessionSecret},
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

func TestValidateRejectsPublicAdminWithoutTLS(t *testing.T) {
	base := Config{
		Storage:  StorageConfig{DataRoot: t.TempDir()},
		Database: DatabaseConfig{Driver: "sqlite", DSN: "test.db"},
		WebAdmin: WebAdminConfig{SessionSecret: validTestSessionSecret},
	}

	cfg := base
	cfg.Server.AdminAddr = "0.0.0.0:9001"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "must not listen publicly") {
		t.Fatalf("public plaintext admin validation error = %v, want public-listener error", err)
	}

	cfg = base
	cfg.Server.AdminAddr = "127.0.0.1:9001"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("loopback plaintext admin returned error: %v", err)
	}

	cfg = base
	cfg.Server.AdminAddr = "0.0.0.0:9001"
	cfg.Server.AdminTLS = &TLSConfig{Enabled: true, CertFile: "admin.crt", KeyFile: "admin.key"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("public TLS admin returned error: %v", err)
	}
}

func TestValidateRejectsInvalidSecurityConfig(t *testing.T) {
	base := Config{
		Storage:  StorageConfig{DataRoot: t.TempDir()},
		Database: DatabaseConfig{Driver: "sqlite", DSN: "test.db"},
		WebAdmin: WebAdminConfig{SessionSecret: validTestSessionSecret},
	}

	cfg := base
	cfg.WebAdmin.TOTP.Enabled = true
	cfg.WebAdmin.TOTP.Secret = "not-base32"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "webadmin.totp.secret") {
		t.Fatalf("totp validation error = %v, want webadmin.totp.secret error", err)
	}

	cfg = base
	cfg.WebAdmin.Captcha.Enabled = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "webadmin.captcha.provider") {
		t.Fatalf("captcha validation error = %v, want provider error", err)
	}

	cfg = base
	cfg.WebAdmin.Captcha = CaptchaConfig{
		Enabled:   true,
		Provider:  "other",
		SiteKey:   "site",
		SecretKey: "secret",
		VerifyURL: "http://127.0.0.1/verify",
		Timeout:   time.Second,
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "must be turnstile") {
		t.Fatalf("captcha provider validation error = %v, want turnstile error", err)
	}

	cfg = base
	cfg.WebAdmin.Ops.MetricsToken = "change-me-token"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "metrics_token") {
		t.Fatalf("metrics token validation error = %v, want metrics_token error", err)
	}
}

func TestValidateRejectsWeakSessionSecrets(t *testing.T) {
	base := Config{
		Storage:  StorageConfig{DataRoot: t.TempDir()},
		Database: DatabaseConfig{Driver: "sqlite", DSN: "test.db"},
	}

	for _, secret := range []string{"", "short-session-secret", "change-me-32bytes-random", "replace-with-random-secret-at-least-32-bytes"} {
		cfg := base
		cfg.WebAdmin.SessionSecret = secret
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "webadmin.session_secret") {
			t.Fatalf("session secret %q validation error = %v, want webadmin.session_secret error", secret, err)
		}
	}

	cfg := base
	cfg.WebAdmin.SessionSecret = validTestSessionSecret
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid session secret returned error: %v", err)
	}
}

func TestProductionWarningsDoNotIncludeSecretValues(t *testing.T) {
	cfg := Config{
		Server: ServerConfig{AdminAddr: "0.0.0.0:9001"},
		WebAdmin: WebAdminConfig{
			AdminBootstrapPassword: "do-not-print",
			SessionSecret:          "change-me-32bytes-random",
			Ops:                    OpsConfig{PublicMetrics: true},
		},
		RateLimit: RateLimitConfig{TrustForwarded: true},
	}

	warnings := strings.Join(cfg.ProductionWarnings(), "\n")
	for _, want := range []string{"session_secret", "admin_bootstrap_password", "public_metrics", "trust_forwarded"} {
		if !strings.Contains(warnings, want) {
			t.Fatalf("warnings missing %q: %s", want, warnings)
		}
	}
	if strings.Contains(warnings, "do-not-print") {
		t.Fatalf("warnings leaked secret value: %s", warnings)
	}
}
