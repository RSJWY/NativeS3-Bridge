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
	if !cfg.WebAdmin.Ops.HealthzPublic() {
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

func TestValidateAllowsPublicAdminWithoutTLSWithWarning(t *testing.T) {
	base := Config{
		Storage:  StorageConfig{DataRoot: t.TempDir()},
		Database: DatabaseConfig{Driver: "sqlite", DSN: "test.db"},
		WebAdmin: WebAdminConfig{SessionSecret: validTestSessionSecret},
	}

	cfg := base
	cfg.Server.AdminAddr = "0.0.0.0:9001"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("public plaintext admin returned error: %v", err)
	}
	if warnings := strings.Join(cfg.ProductionWarnings(), "\n"); !strings.Contains(warnings, "server.admin_addr listens publicly without admin TLS") {
		t.Fatalf("public plaintext admin warnings = %q, want admin TLS warning", warnings)
	}

	cfg = base
	cfg.Server.AdminAddr = "127.0.0.1:9001"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("loopback plaintext admin returned error: %v", err)
	}
	if warnings := strings.Join(cfg.ProductionWarnings(), "\n"); strings.Contains(warnings, "server.admin_addr listens publicly without admin TLS") {
		t.Fatalf("loopback plaintext admin warnings = %q, do not want public-listener warning", warnings)
	}

	cfg = base
	cfg.Server.AdminAddr = "0.0.0.0:9001"
	cfg.Server.AdminTLS = &TLSConfig{Enabled: true, CertFile: "admin.crt", KeyFile: "admin.key"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("public TLS admin returned error: %v", err)
	}
	if warnings := strings.Join(cfg.ProductionWarnings(), "\n"); strings.Contains(warnings, "server.admin_addr listens publicly without admin TLS") {
		t.Fatalf("public TLS admin warnings = %q, do not want admin TLS warning", warnings)
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

// H1 回归:configs/panel.example.yaml 的占位值只比旧黑名单条目多了 `-value`
// 后缀,长度又达标(42 字节),旧的逐串精确匹配放它过关——占位密钥可以一路进生产,
// 而它就写在公开仓库里,等于任何人都能伪造管理员会话 cookie。
func TestValidateRejectsPlaceholderSessionSecretVariants(t *testing.T) {
	base := Config{
		Storage:  StorageConfig{DataRoot: t.TempDir()},
		Database: DatabaseConfig{Driver: "sqlite", DSN: "test.db"},
	}

	for _, secret := range []string{
		"replace-with-a-random-32-byte-secret-value", // panel.example.yaml 原值(H1)
		"replace-with-a-random-32-byte-secret-VALUE", // 大小写变体
		"Replace-With-Yet-Another-Long-Enough-Value", // 前缀变体
		"change-me-please-this-is-long-enough-abcde",
		"this-is-an-example-secret-long-enough-xxxx",
		"todo-generate-a-real-secret-before-shipping",
		"my-placeholder-session-secret-32-bytes-plus",
		"your-secret-goes-here-and-is-long-enough-ok",
	} {
		cfg := base
		cfg.WebAdmin.SessionSecret = secret
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("placeholder session secret %q was accepted", secret)
		}
		if !strings.Contains(err.Error(), "webadmin.session_secret") {
			t.Fatalf("session secret %q error = %v, want webadmin.session_secret error", secret, err)
		}
		// R1.2:光说"不合法"会让运维把占位值改得更长而不是换成随机值,
		// 所以报错必须自解释并直接给出生成命令。
		if !strings.Contains(err.Error(), "placeholder") || !strings.Contains(err.Error(), "openssl rand -base64 32") {
			t.Fatalf("session secret %q error lacks placeholder wording or openssl hint: %v", secret, err)
		}
	}
}

// 回归保护:特征词匹配不得误杀真随机密钥,否则加严校验会把正常部署挡在门外。
func TestValidateAcceptsRandomSessionSecrets(t *testing.T) {
	base := Config{
		Storage:  StorageConfig{DataRoot: t.TempDir()},
		Database: DatabaseConfig{Driver: "sqlite", DSN: "test.db"},
	}

	for _, secret := range []string{
		validTestSessionSecret,
		"kQ8xN2vR7pL4mT9wZ1cY6bH3sD5fG0jA8eU2iO4nK7Q=",                     // openssl rand -base64 32 形状
		"9f3c1a7e5b2d8046af91c3e7d5b0a2648f1e9c3a7b5d20468e1f9c3a7b5d2046", // openssl rand -hex 32 形状(安装脚本用的就是它)
	} {
		cfg := base
		cfg.WebAdmin.SessionSecret = secret
		if err := cfg.Validate(); err != nil {
			t.Fatalf("random session secret %q rejected: %v", secret, err)
		}
	}
}

// R3/L4 回归:显式写 public_healthz: false 必须生效。旧代码在 applyDefaults 里
// 无条件赋 true,把用户的选择静默吞掉,/healthz 在任何配置下都是裸奔的。
func TestPublicHealthzHonoursExplicitFalse(t *testing.T) {
	write := func(t *testing.T, opsBlock string) *Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		content := `
storage:
  data_root: "` + filepath.ToSlash(t.TempDir()) + `"
database:
  driver: sqlite
  dsn: test.db
webadmin:
  session_secret: "` + validTestSessionSecret + `"
` + opsBlock
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		return cfg
	}

	if got := write(t, "  ops:\n    public_healthz: false\n"); got.WebAdmin.Ops.HealthzPublic() {
		t.Fatal("explicit public_healthz: false was overridden to true")
	}
	if got := write(t, "  ops:\n    public_healthz: true\n"); !got.WebAdmin.Ops.HealthzPublic() {
		t.Fatal("explicit public_healthz: true should stay true")
	}
	// 不写 ops 块:沿用历史默认 true,升级前后行为一致。
	if got := write(t, ""); !got.WebAdmin.Ops.HealthzPublic() {
		t.Fatal("unset public_healthz should default to true")
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
	for _, want := range []string{"session_secret", "admin_bootstrap_password", "server.admin_addr", "public_metrics", "trust_forwarded"} {
		if !strings.Contains(warnings, want) {
			t.Fatalf("warnings missing %q: %s", want, warnings)
		}
	}
	if strings.Contains(warnings, "do-not-print") {
		t.Fatalf("warnings leaked secret value: %s", warnings)
	}
}

func TestLogConfigDefaultsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `
storage:
  data_root: "` + filepath.ToSlash(t.TempDir()) + `"
database:
  driver: sqlite
  dsn: test.db
webadmin:
  session_secret: "` + validTestSessionSecret + `"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.File != "" || cfg.Log.Dir != "" || cfg.Log.MaxSizeMB != 100 || cfg.Log.MaxBackups != 5 || cfg.Log.MaxAgeDays != 0 {
		t.Fatalf("log defaults = %+v", cfg.Log)
	}
	legacyFile := filepath.Join(t.TempDir(), "legacy.log")
	if effective := (LogConfig{File: legacyFile}).EffectiveFile(); effective != legacyFile {
		t.Fatalf("legacy effective file = %q, want %q", effective, legacyFile)
	}
	logDir := t.TempDir()
	if effective := (LogConfig{Dir: logDir}).EffectiveFile(); effective != filepath.Join(logDir, DefaultLogFileName) {
		t.Fatalf("directory effective file = %q", effective)
	}

	cfg.Log.File = filepath.Join(t.TempDir(), "app.log")
	cfg.Log.MaxSizeMB = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_size_mb") {
		t.Fatalf("max size validation = %v", err)
	}
	cfg.Log.MaxSizeMB = 1
	cfg.Log.MaxBackups = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_backups") {
		t.Fatalf("max backups validation = %v", err)
	}
	cfg.Log.MaxBackups = 5
	cfg.Log.Dir = t.TempDir()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("log dir/file conflict validation = %v", err)
	}
	cfg.Log.File = ""
	cfg.Log.MaxSizeMB = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_size_mb") {
		t.Fatalf("log dir max size validation = %v", err)
	}

	explicitPath := filepath.Join(t.TempDir(), "explicit.yaml")
	explicit := `
storage:
  data_root: "` + filepath.ToSlash(t.TempDir()) + `"
database:
  driver: sqlite
  dsn: test.db
webadmin:
  session_secret: "` + validTestSessionSecret + `"
log:
  file: app.log
  max_size_mb: 0
  max_backups: 0
`
	if err := os.WriteFile(explicitPath, []byte(explicit), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(explicitPath); err == nil || !strings.Contains(err.Error(), "max_size_mb") {
		t.Fatalf("explicit zero max size error = %v", err)
	}
	explicit = strings.Replace(explicit, "max_size_mb: 0", "max_size_mb: 1", 1)
	if err := os.WriteFile(explicitPath, []byte(explicit), 0o600); err != nil {
		t.Fatal(err)
	}
	explicitCfg, err := Load(explicitPath)
	if err != nil {
		t.Fatal(err)
	}
	if explicitCfg.Log.MaxBackups != 0 {
		t.Fatalf("explicit max_backups = %d, want 0", explicitCfg.Log.MaxBackups)
	}
}

func TestPanelAndNodeShareLogDefaultsAndValidation(t *testing.T) {
	panelCfg := PanelConfig{}
	panelCfg.applyDefaults()
	nodeCfg := NodeConfig{}
	nodeCfg.applyDefaults()
	for name, logCfg := range map[string]LogConfig{
		"panel": panelCfg.Log,
		"node":  nodeCfg.Log,
	} {
		if logCfg.MaxSizeMB != 100 || logCfg.MaxBackups != 5 || logCfg.MaxAgeDays != 0 {
			t.Fatalf("%s log defaults = %+v", name, logCfg)
		}
	}

	validPanel := PanelConfig{
		Database:      DatabaseConfig{Driver: "sqlite", DSN: "panel.db"},
		WebAdmin:      WebAdminConfig{SessionSecret: validTestSessionSecret},
		MasterKeyFile: "master.key",
		PKI:           PKIConfig{IntermediateCertFile: "ca.crt", IntermediateKeyFile: "ca.key"},
		Agent:         AgentListenerConfig{CertFile: "agent.crt", KeyFile: "agent.key"},
	}
	validNode := NodeConfig{
		Storage:  StorageConfig{DataRoot: "/data"},
		Database: DatabaseConfig{Driver: "sqlite", DSN: "node.db"},
		Panel: PanelClientConfig{
			NodeID: 1, AgentURL: "wss://panel/agent", CertFile: "node.crt", KeyFile: "node.key", CAFile: "ca.crt",
		},
	}

	tests := []struct {
		name string
		log  LogConfig
		want string
	}{
		{name: "mutually exclusive paths", log: LogConfig{Dir: "/logs", File: "/logs/app.log", MaxSizeMB: 1}, want: "mutually exclusive"},
		{name: "zero size with file", log: LogConfig{File: "/logs/app.log"}, want: "max_size_mb"},
		{name: "negative backups", log: LogConfig{MaxBackups: -1}, want: "max_backups"},
		{name: "negative age", log: LogConfig{MaxAgeDays: -1}, want: "max_age_days"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			panelCase := validPanel
			panelCase.Log = tt.log
			if err := panelCase.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("panel validation error = %v, want %q", err, tt.want)
			}
			nodeCase := validNode
			nodeCase.Log = tt.log
			if err := nodeCase.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("node validation error = %v, want %q", err, tt.want)
			}
		})
	}

	validLog := LogConfig{File: "/logs/app.log", MaxSizeMB: 1, MaxBackups: 0, MaxAgeDays: 0}
	panelCase := validPanel
	panelCase.Log = validLog
	if err := panelCase.Validate(); err != nil {
		t.Fatalf("panel rejected explicit zero backups: %v", err)
	}
	nodeCase := validNode
	nodeCase.Log = validLog
	if err := nodeCase.Validate(); err != nil {
		t.Fatalf("node rejected explicit zero backups: %v", err)
	}
}
