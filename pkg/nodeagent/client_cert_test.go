package nodeagent

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// AC12:路径 A(本地证书过期)必须被识别为 local 类证书错误,且文案含证书
// 语义与恢复动作,而不是混进通用的 dial 错误。拨号前检查意味着测试无需
// 真正起 panel --错误在拨号之前就返回。
func TestConnectAndServeLocalCertExpired(t *testing.T) {
	tmp := t.TempDir()
	id := Identity{
		KeyFile:  filepath.Join(tmp, "node.key"),
		CertFile: filepath.Join(tmp, "node.crt"),
	}
	writeTestCert(t, id, time.Now().Add(-time.Hour))

	client := NewClient(ClientConfig{
		AgentURL: "wss://panel.invalid:9443/agent",
		NodeID:   1,
		Identity: id,
	}, nil, nil, nil)

	err := client.connectAndServe(context.Background())
	if err == nil {
		t.Fatal("expected error for expired local cert")
	}
	certErr, ok := err.(*certError)
	if !ok {
		t.Fatalf("expected *certError, got %T: %v", err, err)
	}
	if certErr.kind != "local" {
		t.Fatalf("kind = %q, want local", certErr.kind)
	}
	if !strings.Contains(certErr.Error(), "certificate") {
		t.Fatalf("error must carry certificate semantics: %v", certErr)
	}
	// 恢复动作由 certErrs.log 的 action 参数承载;路径 A 的 action 指向重注册。
	if client.certErrs.failures["local"] != 1 {
		t.Fatalf("expected local failure count 1, got %v", client.certErrs.failures)
	}
}

// 路径 A 的另一形态:证书文件损坏(非 PEM)。
func TestConnectAndServeLocalCertMalformed(t *testing.T) {
	tmp := t.TempDir()
	id := Identity{
		KeyFile:  filepath.Join(tmp, "node.key"),
		CertFile: filepath.Join(tmp, "node.crt"),
	}
	writeTestCert(t, id, time.Now().Add(time.Hour))
	// 覆盖成垃圾内容。
	writeFile(t, id.CertFile, []byte("garbage"))

	client := NewClient(ClientConfig{
		AgentURL: "wss://panel.invalid:9443/agent",
		NodeID:   1,
		Identity: id,
	}, nil, nil, nil)

	err := client.connectAndServe(context.Background())
	certErr, ok := err.(*certError)
	if !ok {
		t.Fatalf("expected *certError, got %T: %v", err, err)
	}
	if certErr.kind != "local" {
		t.Fatalf("kind = %q, want local", certErr.kind)
	}
}

// AC13:路径 B(panel 401)必须被识别为 rejected 类证书错误,且与路径 A 的
// kind 可区分。证书本身有效,由测试服务器显式返回 401。服务器证书写入
// CAFile 使节点侧的服务器校验通过,错误才会落在 401 上(TLS 握手在前)。
func TestConnectAndServePanelRejected(t *testing.T) {
	tmp := t.TempDir()
	caFile := filepath.Join(tmp, "test-ca.pem")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"client certificate required"}`))
	}))
	defer server.Close()
	serverCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	writeFile(t, caFile, serverCertPEM)

	id := Identity{
		KeyFile:  filepath.Join(tmp, "node.key"),
		CertFile: filepath.Join(tmp, "node.crt"),
		CAFile:   caFile,
	}
	writeTestCert(t, id, time.Now().Add(24*time.Hour))

	client := NewClient(ClientConfig{
		AgentURL: "wss://" + server.Listener.Addr().String() + "/agent",
		NodeID:   1,
		Identity: id,
	}, nil, nil, nil)

	err := client.connectAndServe(context.Background())
	if err == nil {
		t.Fatal("expected error for panel 401")
	}
	certErr, ok := err.(*certError)
	if !ok {
		t.Fatalf("expected *certError, got %T: %v", err, err)
	}
	if certErr.kind != "rejected" {
		t.Fatalf("kind = %q, want rejected", certErr.kind)
	}
	if !strings.Contains(certErr.Error(), "401") {
		t.Fatalf("error must mention the 401 status: %v", certErr)
	}
	if client.certErrs.failures["rejected"] != 1 {
		t.Fatalf("expected rejected failure count 1, got %v", client.certErrs.failures)
	}
}

// 非 401 的拨号失败(如网络不可达)不得被误报为证书错误。
func TestConnectAndServeNetworkErrorNotCertError(t *testing.T) {
	tmp := t.TempDir()
	id := Identity{
		KeyFile:  filepath.Join(tmp, "node.key"),
		CertFile: filepath.Join(tmp, "node.crt"),
	}
	writeTestCert(t, id, time.Now().Add(24*time.Hour))

	client := NewClient(ClientConfig{
		AgentURL:    "wss://127.0.0.1:1/agent", // 端口 1 几乎必然拒绝连接
		NodeID:      1,
		Identity:    id,
		DialTimeout: 2 * time.Second,
	}, nil, nil, nil)

	err := client.connectAndServe(context.Background())
	if err == nil {
		t.Fatal("expected dial error")
	}
	if _, ok := err.(*certError); ok {
		t.Fatalf("network error must not be classified as cert error: %v", err)
	}
}

// R4.3 降频语义:首次立即计数,之后每次失败仍计数,每 certErrLogEvery 次
// 重复一条 Error;Reset 后从 1 重新开始。
func TestCertErrorReporterThrottle(t *testing.T) {
	var r certErrorReporter
	r.log("local", "act", "d1", nil)
	if r.failures["local"] != 1 {
		t.Fatalf("first failure count = %d, want 1", r.failures["local"])
	}
	for i := 2; i <= certErrLogEvery; i++ {
		r.log("local", "act", "d", nil)
	}
	if r.failures["local"] != certErrLogEvery {
		t.Fatalf("count = %d, want %d", r.failures["local"], certErrLogEvery)
	}
	// 不同 kind 独立计数。
	r.log("rejected", "act", "d", nil)
	if r.failures["rejected"] != 1 || r.failures["local"] != certErrLogEvery {
		t.Fatalf("kinds must count independently: %v", r.failures)
	}
	// 成功连接重置:恢复后再次失败从 1 开始,重新立即报。
	r.Reset()
	if r.failures != nil {
		t.Fatalf("reset must clear counters, got %v", r.failures)
	}
	r.log("local", "act", "d", nil)
	if r.failures["local"] != 1 {
		t.Fatalf("post-reset count = %d, want 1", r.failures["local"])
	}
}
