package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePanelConfig 生成一份最小可通过 Validate 的 panel 配置。路径类字段只做非空
// 检查,不需要真实存在。
func writePanelConfig(t *testing.T, opsBlock string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "panel.yaml")
	body := `
database: {driver: sqlite, dsn: ./panel.db}
master_key_file: /data/secrets/master.key
pki:
  intermediate_cert_file: /data/pki/intermediate-ca.crt
  intermediate_key_file: /data/pki/intermediate-ca.key
agent:
  cert_file: /data/pki/panel-server.crt
  key_file: /data/pki/panel-server.key
webadmin:
  session_secret: "` + validTestSessionSecret + `"
` + opsBlock
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// R3/L4 回归(panel 路径):panel.applyDefaults 与 Config.applyDefaults 各有一份
// `PublicHealthz = true` 的无条件赋值,两处都会吞掉用户显式写的 false。
// panel.example.yaml 里根本没有 ops 块,所以这条路径上的默认值语义尤其要守住。
func TestPanelPublicHealthzHonoursExplicitFalse(t *testing.T) {
	cfg, err := LoadPanel(writePanelConfig(t, "  ops:\n    public_healthz: false\n"))
	if err != nil {
		t.Fatalf("load panel config: %v", err)
	}
	if cfg.WebAdmin.Ops.HealthzPublic() {
		t.Fatal("explicit public_healthz: false was overridden to true")
	}

	// 不写 ops 块(panel.example.yaml 的形状):沿用历史默认 true。
	cfg, err = LoadPanel(writePanelConfig(t, ""))
	if err != nil {
		t.Fatalf("load panel config without ops block: %v", err)
	}
	if !cfg.WebAdmin.Ops.HealthzPublic() {
		t.Fatal("unset public_healthz should default to true")
	}
}

// panel 与 monolith 共用 validateSessionSecret,H1 的修复必须两边同时生效
// (R1.4)。panel.example.yaml 的占位值走的正是这条路径。
func TestPanelRejectsPlaceholderSessionSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel.yaml")
	body := `
database: {driver: sqlite, dsn: ./panel.db}
master_key_file: /data/secrets/master.key
pki:
  intermediate_cert_file: /data/pki/intermediate-ca.crt
  intermediate_key_file: /data/pki/intermediate-ca.key
agent:
  cert_file: /data/pki/panel-server.crt
  key_file: /data/pki/panel-server.key
webadmin:
  session_secret: "replace-with-a-random-32-byte-secret-value"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPanel(path)
	if err == nil {
		t.Fatal("panel accepted the example placeholder session_secret")
	}
	if got := err.Error(); !strings.Contains(got, "webadmin.session_secret") || !strings.Contains(got, "openssl rand -base64 32") {
		t.Fatalf("panel session secret error = %v, want it to name the field and give the generate command", err)
	}
}
