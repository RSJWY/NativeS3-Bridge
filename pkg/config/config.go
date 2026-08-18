package config

import (
	"encoding/base32"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Storage   StorageConfig   `yaml:"storage"`
	Database  DatabaseConfig  `yaml:"database"`
	Hooks     HooksConfig     `yaml:"hooks"`
	WebAdmin  WebAdminConfig  `yaml:"webadmin"`
	RateLimit RateLimitConfig `yaml:"rate_limit"`
	Auth      AuthConfig      `yaml:"auth"`
	Region    string          `yaml:"region"`
	LogLevel  string          `yaml:"log_level"`
	Log       LogConfig       `yaml:"log"`
}

// AuthConfig 控制 S3 签名认证行为。
type AuthConfig struct {
	// AllowSigV2 允许接受 S3 Signature Version 2 请求。默认 false:
	// v2 用 HMAC-SHA1、不签请求体、无 region/service scope,安全性明显弱于 v4,
	// 且 v2 + aws-chunked 的 payload 完整性完全依赖 aws-chunked 解码器。
	// 仅在必须兼容仅支持 v2 的客户端时显式开启。
	AllowSigV2 bool `yaml:"allow_sigv2"`
}

type LogConfig struct {
	File          string `yaml:"file"`
	Dir           string `yaml:"dir"`
	MaxSizeMB     int    `yaml:"max_size_mb"`
	MaxBackups    int    `yaml:"max_backups"`
	MaxAgeDays    int    `yaml:"max_age_days"`
	Compress      bool   `yaml:"compress"`
	maxSizeSet    bool
	maxBackupsSet bool
}

const DefaultLogFileName = "natives3bridge.log"

func (c LogConfig) EffectiveFile() string {
	if c.Dir != "" {
		return filepath.Join(c.Dir, DefaultLogFileName)
	}
	return c.File
}

func (c *LogConfig) applyDefaults() {
	if !c.maxSizeSet && c.MaxSizeMB == 0 {
		c.MaxSizeMB = 100
	}
	if !c.maxBackupsSet && c.MaxBackups == 0 {
		c.MaxBackups = 5
	}
}

func (c LogConfig) validate() error {
	if c.Dir != "" && c.File != "" {
		return fmt.Errorf("log.dir and log.file are mutually exclusive")
	}
	if c.EffectiveFile() != "" && c.MaxSizeMB < 1 {
		return fmt.Errorf("log.max_size_mb must be at least 1 when file logging is enabled")
	}
	if c.MaxBackups < 0 {
		return fmt.Errorf("log.max_backups must not be negative")
	}
	if c.MaxAgeDays < 0 {
		return fmt.Errorf("log.max_age_days must not be negative")
	}
	return nil
}

func (c *LogConfig) UnmarshalYAML(node *yaml.Node) error {
	type rawLogConfig LogConfig
	var raw rawLogConfig
	if err := node.Decode(&raw); err != nil {
		return err
	}
	*c = LogConfig(raw)
	for index := 0; index+1 < len(node.Content); index += 2 {
		switch node.Content[index].Value {
		case "max_size_mb":
			c.maxSizeSet = true
		case "max_backups":
			c.maxBackupsSet = true
		}
	}
	return nil
}

type ServerConfig struct {
	S3Addr    string     `yaml:"s3_addr"`
	AdminAddr string     `yaml:"admin_addr"`
	TLS       TLSConfig  `yaml:"tls"`
	AdminTLS  *TLSConfig `yaml:"admin_tls"`
}

type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

func (c ServerConfig) EffectiveAdminTLS() TLSConfig {
	if c.AdminTLS == nil {
		return c.TLS
	}
	return *c.AdminTLS
}

type RateLimitConfig struct {
	AnonymousRPS   float64 `yaml:"anonymous_rps"`
	AnonymousBurst int     `yaml:"anonymous_burst"`
	TrustForwarded bool    `yaml:"trust_forwarded"`
}

const (
	DefaultAnonymousRPS   = 10.0
	DefaultAnonymousBurst = 20
)

type StorageConfig struct {
	DataRoot                 string        `yaml:"data_root"`
	MultipartTmp             string        `yaml:"multipart_tmp"`
	MetadataSuffix           string        `yaml:"metadata_suffix"`
	MultipartGCInterval      time.Duration `yaml:"multipart_gc_interval"`
	MultipartTTL             time.Duration `yaml:"multipart_ttl"`
	MultipartMaxPendingBytes int64         `yaml:"multipart_max_pending_bytes"`
}

type DatabaseConfig struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

type HooksConfig struct {
	QueueSize int           `yaml:"queue_size"`
	Workers   int           `yaml:"workers"`
	MaxRetry  int           `yaml:"max_retry"`
	Timeout   time.Duration `yaml:"timeout"`
}

type WebAdminConfig struct {
	PasswordHash           string        `yaml:"password_hash"`
	AdminBootstrapPassword string        `yaml:"admin_bootstrap_password"`
	SessionSecret          string        `yaml:"session_secret"`
	SessionTTLMinutes      int           `yaml:"session_ttl_minutes"`
	LoginMaxFailures       int           `yaml:"login_max_failures"`
	LoginLockoutWindow     time.Duration `yaml:"login_lockout_window"`
	TOTP                   TOTPConfig    `yaml:"totp"`
	Captcha                CaptchaConfig `yaml:"captcha"`
	Ops                    OpsConfig     `yaml:"ops"`
}

type TOTPConfig struct {
	Enabled bool   `yaml:"enabled"`
	Issuer  string `yaml:"issuer"`
	Account string `yaml:"account"`
	Secret  string `yaml:"secret"`
}

type CaptchaConfig struct {
	Enabled   bool          `yaml:"enabled"`
	Provider  string        `yaml:"provider"`
	SiteKey   string        `yaml:"site_key"`
	SecretKey string        `yaml:"secret_key"`
	VerifyURL string        `yaml:"verify_url"`
	Timeout   time.Duration `yaml:"timeout"`
}

type OpsConfig struct {
	// PublicHealthz 用指针以区分「未配置」与「显式 false」。yaml 里不写这一项时为
	// nil,沿用历史默认(公开 /healthz);写了 public_healthz: false 才真正关掉。
	// 读取一律走 HealthzPublic(),不要直接解引用。
	PublicHealthz *bool  `yaml:"public_healthz"`
	PublicReadyz  bool   `yaml:"public_readyz"`
	PublicMetrics bool   `yaml:"public_metrics"`
	MetricsToken  string `yaml:"metrics_token"`
}

// HealthzPublic 返回 /healthz 是否免鉴权。nil(未配置)按历史默认 true 处理,
// 因此直接构造 OpsConfig{} 的调用方也能拿到与升级前一致的行为。
func (o OpsConfig) HealthzPublic() bool {
	return o.PublicHealthz == nil || *o.PublicHealthz
}

// boolPtr 供 applyDefaults 填充可选布尔项的默认值。
func boolPtr(v bool) *bool { return &v }

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.S3Addr == "" {
		c.Server.S3Addr = "0.0.0.0:9000"
	}
	if c.Server.AdminAddr == "" {
		c.Server.AdminAddr = "127.0.0.1:9001"
	}
	if c.Storage.MetadataSuffix == "" {
		c.Storage.MetadataSuffix = ".s3meta"
	}
	if c.Storage.MultipartTmp == "" && c.Storage.DataRoot != "" {
		c.Storage.MultipartTmp = filepath.Join(c.Storage.DataRoot, ".multipart")
	}
	if c.Storage.MultipartGCInterval == 0 {
		c.Storage.MultipartGCInterval = time.Hour
	}
	if c.Storage.MultipartTTL == 0 {
		c.Storage.MultipartTTL = 24 * time.Hour
	}
	if c.Storage.MultipartMaxPendingBytes == 0 {
		c.Storage.MultipartMaxPendingBytes = 10 << 30
	}
	if c.Hooks.QueueSize == 0 {
		c.Hooks.QueueSize = 1024
	}
	if c.Hooks.Workers == 0 {
		c.Hooks.Workers = 4
	}
	if c.Hooks.MaxRetry == 0 {
		c.Hooks.MaxRetry = 3
	}
	if c.Hooks.Timeout == 0 {
		c.Hooks.Timeout = 5 * time.Second
	}
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	c.Log.applyDefaults()
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
	// 未配置时才填默认值。旧代码在这里无条件赋 true,把用户显式写的
	// public_healthz: false 静默吞掉,/healthz 始终裸奔。
	if c.WebAdmin.Ops.PublicHealthz == nil {
		c.WebAdmin.Ops.PublicHealthz = boolPtr(true)
	}
	if c.RateLimit.AnonymousRPS <= 0 {
		c.RateLimit.AnonymousRPS = DefaultAnonymousRPS
	}
	if c.RateLimit.AnonymousBurst <= 0 {
		c.RateLimit.AnonymousBurst = DefaultAnonymousBurst
	}
}

func (c *Config) Validate() error {
	if c.Storage.DataRoot == "" {
		return fmt.Errorf("storage.data_root is required")
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required")
	}
	if c.Storage.MultipartMaxPendingBytes < 0 {
		return fmt.Errorf("storage.multipart_max_pending_bytes must be positive")
	}
	if err := c.Log.validate(); err != nil {
		return err
	}
	if err := validateSessionSecret(c.WebAdmin.SessionSecret); err != nil {
		return err
	}
	if c.Server.TLS.Enabled && (c.Server.TLS.CertFile == "" || c.Server.TLS.KeyFile == "") {
		return fmt.Errorf("server.tls cert_file and key_file are required when enabled")
	}
	adminTLS := c.Server.EffectiveAdminTLS()
	if adminTLS.Enabled && (adminTLS.CertFile == "" || adminTLS.KeyFile == "") {
		return fmt.Errorf("server.admin_tls cert_file and key_file are required when enabled")
	}
	if c.WebAdmin.TOTP.Enabled {
		if _, err := decodeTOTPSecret(c.WebAdmin.TOTP.Secret); err != nil {
			return fmt.Errorf("webadmin.totp.secret must be valid base32: %w", err)
		}
	}
	if c.WebAdmin.Captcha.Enabled {
		if strings.TrimSpace(c.WebAdmin.Captcha.Provider) == "" {
			return fmt.Errorf("webadmin.captcha.provider is required when captcha is enabled")
		}
		if strings.ToLower(strings.TrimSpace(c.WebAdmin.Captcha.Provider)) != "turnstile" {
			return fmt.Errorf("webadmin.captcha.provider must be turnstile")
		}
		if strings.TrimSpace(c.WebAdmin.Captcha.SiteKey) == "" {
			return fmt.Errorf("webadmin.captcha.site_key is required when captcha is enabled")
		}
		if strings.TrimSpace(c.WebAdmin.Captcha.SecretKey) == "" {
			return fmt.Errorf("webadmin.captcha.secret_key is required when captcha is enabled")
		}
		if strings.TrimSpace(c.WebAdmin.Captcha.VerifyURL) == "" {
			return fmt.Errorf("webadmin.captcha.verify_url is required when captcha is enabled")
		}
		if c.WebAdmin.Captcha.Timeout <= 0 {
			return fmt.Errorf("webadmin.captcha.timeout must be positive when captcha is enabled")
		}
	}
	if isExampleSecret(c.WebAdmin.Ops.MetricsToken) {
		return fmt.Errorf("webadmin.ops.metrics_token must not use the example value")
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

func (c *Config) ProductionWarnings() []string {
	var warnings []string
	adminTLS := c.Server.EffectiveAdminTLS()
	if isWeakSessionSecret(c.WebAdmin.SessionSecret) {
		warnings = append(warnings, "webadmin.session_secret still uses the example value")
	}
	if c.WebAdmin.AdminBootstrapPassword != "" {
		warnings = append(warnings, "webadmin.admin_bootstrap_password is set; copy the generated hash and clear the bootstrap password")
	}
	if c.WebAdmin.PasswordHash == "" {
		warnings = append(warnings, "webadmin.password_hash is empty; admin login is disabled until a bcrypt hash is configured")
	}
	if isPublicListenAddr(c.Server.AdminAddr) && !adminTLS.Enabled {
		warnings = append(warnings, "server.admin_addr listens publicly without admin TLS; use a trusted HTTPS reverse proxy or enable server.admin_tls")
	}
	if c.RateLimit.TrustForwarded {
		warnings = append(warnings, "rate_limit.trust_forwarded is enabled; only use it when a trusted proxy overwrites forwarded headers")
	}
	if !c.WebAdmin.TOTP.Enabled {
		warnings = append(warnings, "webadmin.totp.enabled is false; public admin deployments should require TOTP")
	}
	if !c.WebAdmin.Captcha.Enabled {
		warnings = append(warnings, "webadmin.captcha.enabled is false; public admin deployments should enable human verification")
	}
	if c.WebAdmin.Ops.PublicReadyz {
		warnings = append(warnings, "webadmin.ops.public_readyz is true; avoid exposing /readyz on public admin hostnames")
	}
	if c.WebAdmin.Ops.PublicMetrics && c.WebAdmin.Ops.MetricsToken == "" {
		warnings = append(warnings, "webadmin.ops.public_metrics is true without a metrics token; avoid exposing /metrics publicly")
	}
	return warnings
}

func decodeTOTPSecret(secret string) ([]byte, error) {
	normalized := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	if normalized == "" {
		return nil, fmt.Errorf("secret is empty")
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(normalized, "="))
	if err != nil {
		return nil, err
	}
	if len(decoded) < 10 {
		return nil, fmt.Errorf("secret is too short")
	}
	return decoded, nil
}

func isExampleSecret(value string) bool {
	switch strings.TrimSpace(value) {
	case "", "change-me", "change-me-token", "change-me-32bytes-random":
		return value != ""
	default:
		return false
	}
}

// minSessionSecretBytes 是 session_secret 的最小长度。该值直接用作会话 cookie 的
// HMAC 密钥(pkg/webadmin/auth.go),短于此长度不足以抵抗离线爆破。
const minSessionSecretBytes = 32

// placeholderSecretMarkers 是示例/占位密钥的特征词(小写比较)。
//
// 逐串精确匹配挡不住占位值的变体:configs/panel.example.yaml 的
// "replace-with-a-random-32-byte-secret-value" 只比旧黑名单里的条目多了 `-value`
// 后缀,长度又足够(42 字节),于是占位密钥一路通过校验直达生产——任何读过本仓库
// 的人都能据此伪造管理员会话。改为特征词包含匹配,让下一个变体也进不来。
var placeholderSecretMarkers = []string{
	"replace-with", "replace_with", "replacewith",
	"change-me", "change_me", "changeme",
	"example", "todo", "placeholder",
	"your-secret", "your_secret", "yoursecret",
}

// exactWeakSessionSecrets 是历史示例值的精确兜底名单:即使某个值不含上面的特征词,
// 只要它在仓库示例配置里出现过就必须继续被拒。
var exactWeakSessionSecrets = []string{
	"change-me-32bytes-random",
	"replace-with-random-secret-at-least-32-bytes",
	"replace-with-a-random-32-byte-secret",
	"replace-with-a-random-32-byte-secret-value",
}

// sessionSecretPlaceholderMarker 返回命中的占位特征;未命中返回空串。
// 命中精确名单时返回整串,便于报错文案指名道姓。
func sessionSecretPlaceholderMarker(value string) string {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	for _, exact := range exactWeakSessionSecrets {
		if trimmed == exact {
			return exact
		}
	}
	for _, marker := range placeholderSecretMarkers {
		if strings.Contains(trimmed, marker) {
			return marker
		}
	}
	return ""
}

func validateSessionSecret(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("webadmin.session_secret is required; generate one with `openssl rand -base64 32`")
	}
	// 占位符检查先于长度检查:占位值往往长度达标,先报长度会给出误导性指引。
	if marker := sessionSecretPlaceholderMarker(trimmed); marker != "" {
		return fmt.Errorf("webadmin.session_secret looks like an example placeholder (matched %q); "+
			"a placeholder secret lets anyone who has read this repository forge an admin session. "+
			"Replace it with a random value: `openssl rand -base64 32`", marker)
	}
	if len([]byte(trimmed)) < minSessionSecretBytes {
		return fmt.Errorf("webadmin.session_secret must be at least %d bytes (got %d); generate one with `openssl rand -base64 32`",
			minSessionSecretBytes, len([]byte(trimmed)))
	}
	return nil
}

// isWeakSessionSecret 是 ProductionWarnings 用的判定入口:与启动校验同源,
// 避免"警告说没问题、启动却被拒"这类两套标准。
func isWeakSessionSecret(value string) bool {
	return validateSessionSecret(value) != nil
}

func isPublicListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	return host == "" || host == "0.0.0.0" || host == "::"
}
