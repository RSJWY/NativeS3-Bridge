package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// PanelConfig is the panel process configuration. The panel hosts the human
// admin surface (WebAdmin UI + REST, reusing the existing login/session/lockout/
// TOTP/captcha stack) and the node control-plane listener (mTLS WebSocket). It
// has NO S3 data plane and no storage backend: object traffic never transits the
// panel (design §1.3).
type PanelConfig struct {
	// AdminAddr is the human admin UI/REST listener (sits behind a trusted HTTPS
	// reverse proxy or admin TLS, same as the monolith's admin surface).
	AdminAddr string     `yaml:"admin_addr"`
	AdminTLS  *TLSConfig `yaml:"admin_tls"`

	// Agent is the node control-plane listener config (mTLS WebSocket + one-shot
	// registration endpoint). Physically separate from AdminAddr so certificate
	// and firewall policy differ per surface (design §1.3).
	Agent AgentListenerConfig `yaml:"agent"`

	// PKI locates the online intermediate CA used to sign node client certs.
	PKI PKIConfig `yaml:"pki"`

	// MasterKeyFile points to the external AEAD master key used to encrypt S3
	// secret keys at rest. It MUST live outside the database (design §2.3); the
	// panel refuses to start if it is missing or the wrong length (fail-closed).
	MasterKeyFile string `yaml:"master_key_file"`

	Database DatabaseConfig `yaml:"database"`
	WebAdmin WebAdminConfig `yaml:"webadmin"`
	LogLevel string         `yaml:"log_level"`
	Log      LogConfig      `yaml:"log"`

	// HeartbeatInterval / OfflineMultiplier tune node liveness accounting.
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	OfflineMultiplier int           `yaml:"offline_multiplier"`
}

// AgentListenerConfig configures the node接入 listener. It always uses server
// TLS (nodes verify the panel), and requires client certs on the /agent route
// (mTLS) while allowing their absence on the one-shot /register route.
type AgentListenerConfig struct {
	Addr     string `yaml:"addr"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// PKIConfig locates the online intermediate CA. The offline root CA is not
// referenced here: it only signs/rotates the intermediate out of band.
type PKIConfig struct {
	IntermediateCertFile string `yaml:"intermediate_cert_file"`
	IntermediateKeyFile  string `yaml:"intermediate_key_file"`
	// ClientCertTTL is the validity period for issued node client certificates.
	ClientCertTTL time.Duration `yaml:"client_cert_ttl"`
}

// LoadPanel reads and validates a panel configuration file.
func LoadPanel(path string) (*PanelConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read panel config %q: %w", path, err)
	}
	var cfg PanelConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse panel config %q: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *PanelConfig) applyDefaults() {
	if c.AdminAddr == "" {
		c.AdminAddr = "127.0.0.1:9001"
	}
	if c.Agent.Addr == "" {
		c.Agent.Addr = "0.0.0.0:9443"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if !c.Log.maxSizeSet && c.Log.MaxSizeMB == 0 {
		c.Log.MaxSizeMB = 100
	}
	if !c.Log.maxBackupsSet && c.Log.MaxBackups == 0 {
		c.Log.MaxBackups = 5
	}
	if c.WebAdmin.SessionTTLMinutes == 0 {
		c.WebAdmin.SessionTTLMinutes = 720
	}
	if c.WebAdmin.LoginMaxFailures == 0 {
		c.WebAdmin.LoginMaxFailures = 5
	}
	if c.WebAdmin.LoginLockoutWindow == 0 {
		c.WebAdmin.LoginLockoutWindow = 15 * time.Minute
	}
	if c.WebAdmin.TOTP.Issuer == "" {
		c.WebAdmin.TOTP.Issuer = "NativeS3-Bridge"
	}
	if c.WebAdmin.TOTP.Account == "" {
		c.WebAdmin.TOTP.Account = "admin"
	}
	if c.WebAdmin.Captcha.Provider == "" {
		c.WebAdmin.Captcha.Provider = "turnstile"
	}
	if c.WebAdmin.Captcha.VerifyURL == "" {
		c.WebAdmin.Captcha.VerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	}
	if c.WebAdmin.Captcha.Timeout == 0 {
		c.WebAdmin.Captcha.Timeout = 3 * time.Second
	}
	c.WebAdmin.Ops.PublicHealthz = true
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = 15 * time.Second
	}
	if c.OfflineMultiplier == 0 {
		c.OfflineMultiplier = 3
	}
	if c.PKI.ClientCertTTL == 0 {
		c.PKI.ClientCertTTL = 90 * 24 * time.Hour
	}
}

// Validate enforces the panel's required fields. The master key path and the CA
// files are mandatory: without them the panel cannot encrypt secrets or issue
// node certs, and it must refuse to start (fail-closed) rather than degrade.
func (c *PanelConfig) Validate() error {
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required")
	}
	if err := validateSessionSecret(c.WebAdmin.SessionSecret); err != nil {
		return err
	}
	if strings.TrimSpace(c.MasterKeyFile) == "" {
		return fmt.Errorf("master_key_file is required (secret-key encryption master key must live outside the database)")
	}
	if strings.TrimSpace(c.PKI.IntermediateCertFile) == "" || strings.TrimSpace(c.PKI.IntermediateKeyFile) == "" {
		return fmt.Errorf("pki.intermediate_cert_file and pki.intermediate_key_file are required")
	}
	if strings.TrimSpace(c.Agent.CertFile) == "" || strings.TrimSpace(c.Agent.KeyFile) == "" {
		return fmt.Errorf("agent.cert_file and agent.key_file are required (node listener server TLS)")
	}
	if c.Log.Dir != "" && c.Log.File != "" {
		return fmt.Errorf("log.dir and log.file are mutually exclusive")
	}
	if c.Log.EffectiveFile() != "" && c.Log.MaxSizeMB < 1 {
		return fmt.Errorf("log.max_size_mb must be at least 1 when file logging is enabled")
	}
	adminTLS := c.EffectiveAdminTLS()
	if adminTLS.Enabled && (adminTLS.CertFile == "" || adminTLS.KeyFile == "") {
		return fmt.Errorf("admin_tls cert_file and key_file are required when enabled")
	}
	if c.WebAdmin.TOTP.Enabled {
		if _, err := decodeTOTPSecret(c.WebAdmin.TOTP.Secret); err != nil {
			return fmt.Errorf("webadmin.totp.secret must be valid base32: %w", err)
		}
	}
	switch c.Database.Driver {
	case "sqlite", "mysql", "postgres":
		return nil
	case "":
		return fmt.Errorf("database.driver is required")
	default:
		return fmt.Errorf("database.driver must be one of sqlite, mysql, postgres")
	}
}

// EffectiveAdminTLS returns the admin TLS config, defaulting to disabled when
// unset (the panel is expected to sit behind a trusted reverse proxy otherwise).
func (c *PanelConfig) EffectiveAdminTLS() TLSConfig {
	if c.AdminTLS == nil {
		return TLSConfig{}
	}
	return *c.AdminTLS
}
