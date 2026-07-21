package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadNodeIgnoresLegacyBusinessFields is the safety-net B assertion: a node
// started with a legacy config.yaml that still carries business fields
// (credentials/buckets/quotas/webhooks/rate_limit/webadmin) must NOT fail. The
// node config only consumes infrastructure fields; unknown/legacy keys are
// silently ignored by yaml.v3.
func TestLoadNodeIgnoresLegacyBusinessFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte(`
server:
  s3_addr: "0.0.0.0:9000"
storage:
  data_root: "/data"
database:
  driver: "sqlite"
  dsn: "./natives3.db"
panel:
  node_id: 7
  agent_url: "wss://panel:9443/agent"
  register_url: "https://panel:9443/register"
  cert_file: "/etc/node/cert.pem"
  key_file: "/etc/node/key.pem"
  ca_file: "/etc/node/ca.pem"
# --- legacy business fields that the monolith wrote; must be ignored ---
webadmin:
  password_hash: "$2a$10$legacyhash"
  session_secret: "legacy-secret-value-that-is-long-enough-x"
rate_limit:
  anonymous_rps: 99
  anonymous_burst: 42
hooks:
  queue_size: 512
region: "eu-west-1"
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadNode(path)
	if err != nil {
		t.Fatalf("LoadNode must not fail on legacy business fields: %v", err)
	}
	// Infrastructure fields are consumed.
	if cfg.Panel.NodeID != 7 {
		t.Fatalf("node_id = %d, want 7", cfg.Panel.NodeID)
	}
	if cfg.Storage.DataRoot != "/data" {
		t.Fatalf("data_root = %q, want /data", cfg.Storage.DataRoot)
	}
	if cfg.Server.S3Addr != "0.0.0.0:9000" {
		t.Fatalf("s3_addr = %q", cfg.Server.S3Addr)
	}
	// Infrastructure hooks field is still parsed (it is a node concern).
	if cfg.Hooks.QueueSize != 512 {
		t.Fatalf("hooks.queue_size = %d, want 512", cfg.Hooks.QueueSize)
	}
}

// TestLoadNodeValidatesInfrastructure asserts validation still fires on the
// infrastructure fields the node genuinely needs.
func TestLoadNodeValidatesInfrastructure(t *testing.T) {
	cases := map[string]string{
		"missing data_root": `
database: {driver: sqlite, dsn: ./x.db}
panel: {node_id: 1, agent_url: "wss://p/agent", cert_file: c, key_file: k}
`,
		"missing agent_url": `
storage: {data_root: /data}
database: {driver: sqlite, dsn: ./x.db}
panel: {node_id: 1, cert_file: c, key_file: k}
`,
		"missing node_id": `
storage: {data_root: /data}
database: {driver: sqlite, dsn: ./x.db}
panel: {agent_url: "wss://p/agent", cert_file: c, key_file: k, ca_file: ca}
`,
		"missing ca_file": `
storage: {data_root: /data}
database: {driver: sqlite, dsn: ./x.db}
panel: {node_id: 1, agent_url: "wss://p/agent", cert_file: c, key_file: k}
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadNode(path); err == nil {
				t.Fatalf("expected validation error for %q", name)
			}
		})
	}
}
