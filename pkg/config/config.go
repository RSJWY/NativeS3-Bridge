package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Storage  StorageConfig  `yaml:"storage"`
	Database DatabaseConfig `yaml:"database"`
	Hooks    HooksConfig    `yaml:"hooks"`
	WebAdmin WebAdminConfig `yaml:"webadmin"`
	Region   string         `yaml:"region"`
	LogLevel string         `yaml:"log_level"`
}

type ServerConfig struct {
	S3Addr    string    `yaml:"s3_addr"`
	AdminAddr string    `yaml:"admin_addr"`
	TLS       TLSConfig `yaml:"tls"`
}

type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type StorageConfig struct {
	DataRoot            string        `yaml:"data_root"`
	MultipartTmp        string        `yaml:"multipart_tmp"`
	MetadataSuffix      string        `yaml:"metadata_suffix"`
	MultipartGCInterval time.Duration `yaml:"multipart_gc_interval"`
	MultipartTTL        time.Duration `yaml:"multipart_ttl"`
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
	PasswordHash           string `yaml:"password_hash"`
	AdminBootstrapPassword string `yaml:"admin_bootstrap_password"`
	SessionSecret          string `yaml:"session_secret"`
	SessionTTLMinutes      int    `yaml:"session_ttl_minutes"`
}

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
		c.Server.AdminAddr = "0.0.0.0:9001"
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
	if c.WebAdmin.SessionTTLMinutes == 0 {
		c.WebAdmin.SessionTTLMinutes = 720
	}
}

func (c *Config) Validate() error {
	if c.Storage.DataRoot == "" {
		return fmt.Errorf("storage.data_root is required")
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required")
	}
	if c.WebAdmin.SessionSecret == "" {
		return fmt.Errorf("webadmin.session_secret is required")
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
