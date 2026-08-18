package config

import (
	"fmt"
	"net/url"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
	"os"
	"time"
)

// NodeConfig is the node process's configuration. Per design §8.2 the node
// config carries ONLY infrastructure fields needed to boot the S3 data plane
// and the control-plane client. Business configuration (credentials, buckets,
// quotas, webhooks, rate-limit policy) is no longer node-owned: it is delivered
// as panel-authoritative desired state.
//
// Safety net B (design §8.3): a node started with a legacy config.yaml that
// still contains business fields must NOT fail. gopkg.in/yaml.v3 ignores fields
// absent from the target struct by default, so any legacy business keys are
// simply dropped. Validation below only inspects infrastructure fields.
type NodeConfig struct {
	Server   NodeServerConfig  `yaml:"server"`
	Storage  StorageConfig     `yaml:"storage"`
	Database DatabaseConfig    `yaml:"database"`
	Panel    PanelClientConfig `yaml:"panel"`
	Auth     AuthConfig        `yaml:"auth"`
	Region   string            `yaml:"region"`
	LogLevel string            `yaml:"log_level"`
	Log      LogConfig         `yaml:"log"`
	Hooks    HooksConfig       `yaml:"hooks"`
}

// NodeServerConfig is the node's S3 listener config. Unlike the monolith's
// ServerConfig it has no admin listener: the node never serves a management
// surface (design §1.3). AdminAddr / AdminTLS are intentionally absent.
type NodeServerConfig struct {
	S3Addr string    `yaml:"s3_addr"`
	TLS    TLSConfig `yaml:"tls"`
}

// PanelClientConfig points the node at its panel and locates the node's mTLS
// identity files. The private key is generated locally on first boot and never
// leaves the node.
type PanelClientConfig struct {
	// NodeID is the logical node ID assigned by the panel when the admin created
	// the node. It scopes the registration token and the issued certificate.
	NodeID int64 `yaml:"node_id"`
	// RegisterURL is the panel's server-TLS one-shot registration endpoint, e.g.
	// https://panel.example.com:9443/register.
	RegisterURL string `yaml:"register_url"`
	// AgentURL is the panel's mTLS WebSocket control endpoint, e.g.
	// wss://panel.example.com:9443/agent.
	AgentURL string `yaml:"agent_url"`
	// Token is the single-use registration token. It is only consulted on first
	// boot (no certificate on disk yet) and may be cleared afterward.
	Token string `yaml:"registration_token"`
	// CertFile / KeyFile / CAFile locate the node identity. KeyFile is created on
	// first boot; CertFile is written after registration; CAFile holds the panel
	// CA used to verify the panel's server certificate.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	CAFile   string `yaml:"ca_file"`

	// AllowInsecureTransport 是本地开发/同机回环测试的显式逃生门。默认 false:
	// AgentURL 必须是 wss://、RegisterURL 必须是 https://。显式置 true 才放行
	// ws:// / http://,此时 mTLS 不生效,启动时会打 Warn。
	AllowInsecureTransport bool `yaml:"allow_insecure_transport"`

	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
}

// LoadNode reads and validates a node configuration file. Legacy business
// fields present in the file are ignored (safety net B); only infrastructure
// fields are validated.
func LoadNode(path string) (*NodeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read node config %q: %w", path, err)
	}
	var cfg NodeConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse node config %q: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *NodeConfig) applyDefaults() {
	if c.Server.S3Addr == "" {
		c.Server.S3Addr = "0.0.0.0:9000"
	}
	if c.Storage.MetadataSuffix == "" {
		c.Storage.MetadataSuffix = ".s3meta"
	}
	if c.Storage.MultipartTmp == "" && c.Storage.DataRoot != "" {
		c.Storage.MultipartTmp = joinDataRoot(c.Storage.DataRoot)
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
	if c.Panel.HeartbeatInterval == 0 {
		c.Panel.HeartbeatInterval = 15 * time.Second
	}
}

// Validate checks only infrastructure fields (safety net B). Business config is
// not the node's concern and must never cause a node to refuse to start.
func (c *NodeConfig) Validate() error {
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
	if c.Server.TLS.Enabled && (c.Server.TLS.CertFile == "" || c.Server.TLS.KeyFile == "") {
		return fmt.Errorf("server.tls cert_file and key_file are required when enabled")
	}
	// Panel client fields: required for the node to reach the control plane. The
	// registration token is only needed on first boot and is validated there.
	if strings.TrimSpace(c.Panel.AgentURL) == "" {
		return fmt.Errorf("panel.agent_url is required")
	}
	if err := validateControlPlaneURL("panel.agent_url", c.Panel.AgentURL, "wss", "ws", c.Panel.AllowInsecureTransport); err != nil {
		return err
	}
	// register_url 只在首次注册用,允许留空(已注册的节点会清掉它)。
	if strings.TrimSpace(c.Panel.RegisterURL) != "" {
		if err := validateControlPlaneURL("panel.register_url", c.Panel.RegisterURL, "https", "http", c.Panel.AllowInsecureTransport); err != nil {
			return err
		}
	}
	if strings.TrimSpace(c.Panel.CertFile) == "" || strings.TrimSpace(c.Panel.KeyFile) == "" {
		return fmt.Errorf("panel.cert_file and panel.key_file are required")
	}
	if strings.TrimSpace(c.Panel.CAFile) == "" {
		return fmt.Errorf("panel.ca_file is required")
	}
	if c.Panel.NodeID <= 0 {
		return fmt.Errorf("panel.node_id is required and must be positive")
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

func joinDataRoot(dataRoot string) string {
	if strings.HasSuffix(dataRoot, "/") {
		return dataRoot + ".multipart"
	}
	return dataRoot + "/.multipart"
}

// validateControlPlaneURL 强制控制面 URL 使用加密 scheme。
//
// 明文 scheme 下 mTLS 整个消失:节点不再验证 panel 身份,panel 也拿不到客户端
// 证书;更糟的是注册响应里的 CA 会被节点当作信任根落盘(pkg/nodeagent/register.go),
// 一次 MITM 就能把攻击者的 CA 永久装进节点。因此默认拒绝明文,只有显式打开
// allow_insecure_transport 才放行。
func validateControlPlaneURL(field, raw, secureScheme, insecureScheme string, allowInsecure bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %w", field, err)
	}
	switch u.Scheme {
	case secureScheme:
		return nil
	case insecureScheme:
		if allowInsecure {
			return nil
		}
		return fmt.Errorf("%s uses the cleartext scheme %q, which disables mTLS: the node stops verifying the panel, "+
			"and the CA returned by registration can be swapped by a man in the middle and is written to disk as a trust root. "+
			"Use %s:// instead, or set panel.allow_insecure_transport: true if this really is a same-host loopback test",
			field, u.Scheme, secureScheme)
	case "":
		return fmt.Errorf("%s must include a scheme, e.g. %s://panel.example.com:9443/...", field, secureScheme)
	default:
		return fmt.Errorf("%s has unsupported scheme %q; use %s:// (or %s:// together with panel.allow_insecure_transport: true)",
			field, u.Scheme, secureScheme, insecureScheme)
	}
}

// nodeConfigLeakBits 是真的会让别人读到或改写这份文件的权限位:group 写 +
// other 读写。
//
// 刻意不按「宽于 0640」做字面位比较:那样会把属主位和执行位也算进暴露面,
// `chmod 0700`(rwx------,别人根本读不到,在"谁能读"上比 0640 更严)会被倒过来
// 警告成「权限过宽」。属主位与执行位都不影响谁能读到内容,不纳入判定。
const nodeConfigLeakBits os.FileMode = 0o026

// InsecureNodeConfigMode 检查 node 配置文件权限是否放任他人读写。node.yaml 里带着
// 注册令牌和数据库 DSN,同机任何用户可读就等于泄露、可写就能被篡改。
//
// 返回实际权限位表示应当告警,返回 0 表示无需告警。只做判定不打日志:pkg/config
// 全包无日志依赖,而且调用方要等日志配置好之后再告警(否则告警落不进日志文件)。
// 调用方只应告警、不得拒绝启动——存量部署普遍是 0644,升级不该被这个挡在门外。
// Windows 没有可用的 Unix 权限位,静默跳过而不是误报。
func InsecureNodeConfigMode(path string) os.FileMode {
	if runtime.GOOS == "windows" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	mode := info.Mode().Perm()
	if mode&nodeConfigLeakBits != 0 {
		return mode
	}
	return 0
}
