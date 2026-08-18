package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// writeNodeConfig 生成一份最小可用的 node 配置,panel 块由调用方覆写。
func writeNodeConfig(t *testing.T, panelBlock string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.yaml")
	body := `
storage: {data_root: /data}
database: {driver: sqlite, dsn: ./x.db}
panel:
` + panelBlock
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// H2 回归:控制面 URL 用明文 scheme 时 mTLS 整个消失——节点不再验证 panel,
// 而注册响应里的 CA 会被无条件落盘为信任根,一次 MITM 就能永久接管节点。
// 旧代码对 agent_url 只查非空,ws:// 一路畅通。
func TestValidateRejectsCleartextControlPlaneURLs(t *testing.T) {
	const identity = `  node_id: 1
  cert_file: c
  key_file: k
  ca_file: ca
`
	cases := map[string]struct {
		panelBlock string
		wantField  string
	}{
		"ws agent_url": {
			panelBlock: identity + `  agent_url: "ws://127.0.0.1:9000/agent"` + "\n",
			wantField:  "panel.agent_url",
		},
		"http register_url": {
			panelBlock: identity + `  agent_url: "wss://p:9443/agent"` + "\n" + `  register_url: "http://127.0.0.1:9443/register"` + "\n",
			wantField:  "panel.register_url",
		},
		"scheme-less agent_url": {
			panelBlock: identity + `  agent_url: "panel.example.com:9443/agent"` + "\n",
			wantField:  "panel.agent_url",
		},
		"unsupported agent_url scheme": {
			panelBlock: identity + `  agent_url: "https://p:9443/agent"` + "\n",
			wantField:  "panel.agent_url",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadNode(writeNodeConfig(t, tc.panelBlock))
			if err == nil {
				t.Fatalf("%s: cleartext/invalid control-plane URL was accepted", name)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Fatalf("%s: error = %v, want it to name %s", name, err, tc.wantField)
			}
		})
	}
}

// R2.2:逃生门必须是显式的、且只有显式打开才放行明文。默认关闭保证存量正确配置
// 与"忘了配"的人都走安全路径。
func TestAllowInsecureTransportOpensCleartextSchemes(t *testing.T) {
	const identity = `  node_id: 1
  cert_file: c
  key_file: k
  ca_file: ca
  agent_url: "ws://127.0.0.1:9000/agent"
  register_url: "http://127.0.0.1:9000/register"
`
	if _, err := LoadNode(writeNodeConfig(t, identity)); err == nil {
		t.Fatal("cleartext URLs accepted without allow_insecure_transport")
	}

	cfg, err := LoadNode(writeNodeConfig(t, identity+"  allow_insecure_transport: true\n"))
	if err != nil {
		t.Fatalf("allow_insecure_transport: true should permit cleartext URLs: %v", err)
	}
	if !cfg.Panel.AllowInsecureTransport {
		t.Fatal("allow_insecure_transport did not survive parsing")
	}
}

// AC4 回归:存量正常配置(wss/https)行为与升级前完全一致,校验加严不得误伤。
func TestValidateAcceptsEncryptedControlPlaneURLs(t *testing.T) {
	const panelBlock = `  node_id: 1
  cert_file: c
  key_file: k
  ca_file: ca
  agent_url: "wss://panel.example.com:9443/agent"
  register_url: "https://panel.example.com:9443/register"
`
	cfg, err := LoadNode(writeNodeConfig(t, panelBlock))
	if err != nil {
		t.Fatalf("wss/https config rejected: %v", err)
	}
	if cfg.Panel.AllowInsecureTransport {
		t.Fatal("allow_insecure_transport must default to false")
	}

	// register_url 留空是已注册节点的常态,不能因为新校验而被拒。
	if _, err := LoadNode(writeNodeConfig(t, `  node_id: 1
  cert_file: c
  key_file: k
  ca_file: ca
  agent_url: "wss://panel.example.com:9443/agent"
`)); err != nil {
		t.Fatalf("empty register_url rejected: %v", err)
	}
}

// R5.1:node.yaml 带着注册令牌和 DSN,同机任何用户可读就等于泄露。只告警不拦截——
// 存量部署普遍是 0644,升级不该被这个挡在门外。
func TestInsecureNodeConfigMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		mode     os.FileMode
		wantWarn bool
	}{
		{0o600, false},
		{0o640, false},
		{0o400, false},
		// 属主位与执行位不影响暴露面:0700/0750 在"谁能读到内容"上不比 0640 松,
		// 早期实现按「宽于 0640」做字面位比较,把它们误报成了权限过宽。
		{0o700, false},
		{0o750, false},
		{0o644, true},
		{0o660, true},
		{0o666, true},
	} {
		path := filepath.Join(dir, fmt.Sprintf("node-%04o.yaml", tc.mode))
		if err := os.WriteFile(path, []byte("panel: {}\n"), tc.mode); err != nil {
			t.Fatal(err)
		}
		// WriteFile 受 umask 影响,显式 chmod 才能拿到想要的位。
		if err := os.Chmod(path, tc.mode); err != nil {
			t.Fatal(err)
		}
		got := InsecureNodeConfigMode(path)
		if tc.wantWarn && got == 0 {
			t.Fatalf("mode %04o should be reported as too permissive", tc.mode)
		}
		if !tc.wantWarn && got != 0 {
			t.Fatalf("mode %04o should not warn, got %04o", tc.mode, got)
		}
	}

	// 文件不存在不告警(调用方此时早已因读取失败退出)。
	if got := InsecureNodeConfigMode(filepath.Join(dir, "missing.yaml")); got != 0 {
		t.Fatalf("missing file should not warn, got %04o", got)
	}
}
