package config

import (
	"fmt"
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
	if !c.Log.maxSizeSet && c.Log.MaxSizeMB == 0 {
		c.Log.MaxSizeMB = 100
	}
	if !c.Log.maxBackupsSet && c.Log.MaxBackups == 0 {
		c.Log.MaxBackups = 5
	}
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
	if c.Log.Dir != "" && c.Log.File != "" {
		return fmt.Errorf("log.dir and log.file are mutually exclusive")
	}
	if c.Log.EffectiveFile() != "" && c.Log.MaxSizeMB < 1 {
		return fmt.Errorf("log.max_size_mb must be at least 1 when file logging is enabled")
	}
	if c.Server.TLS.Enabled && (c.Server.TLS.CertFile == "" || c.Server.TLS.KeyFile == "") {
		return fmt.Errorf("server.tls cert_file and key_file are required when enabled")
	}
	// Panel client fields: required for the node to reach the control plane. The
	// registration token is only needed on first boot and is validated there.
	if strings.TrimSpace(c.Panel.AgentURL) == "" {
		return fmt.Errorf("panel.agent_url is required")
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
