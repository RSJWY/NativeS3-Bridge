package panel

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/logging"
)

// newTrustForwardedTestServer 用给定的 trust_forwarded 取值组装一个 AdminServer,
// 登录失败上限压到 1 次以便断言锁定口径。
func newTrustForwardedTestServer(t *testing.T, trustForwarded bool) *AdminServer {
	t.Helper()
	gdb := openTestDB(t)
	key := make([]byte, masterKeyLen)
	for i := range key {
		key[i] = byte(i + 11)
	}
	cipher, err := NewSecretCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("panel-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("password hash: %v", err)
	}
	cfg := &config.PanelConfig{
		AdminAddr:      "127.0.0.1:0",
		TrustForwarded: trustForwarded,
		WebAdmin: config.WebAdminConfig{
			PasswordHash:       string(passwordHash),
			SessionSecret:      "test-panel-session-secret",
			SessionTTLMinutes:  10,
			LoginMaxFailures:   1,
			LoginLockoutWindow: time.Minute,
		},
	}
	hub := NewHub()
	desired := NewDesiredStateAuthority(gdb, cipher)
	audit := NewAuditor(gdb)
	server, err := NewAdminServer(AdminServerDeps{
		Config: cfg, DB: gdb, Hub: hub,
		Creds:   NewPanelCredentialStore(gdb, cipher),
		Desired: desired,
		Tasks:   NewTaskOrchestrator(gdb, hub, 0),
		Transport: NewTransportServer(TransportDeps{
			DB: gdb, Hub: hub, Cipher: cipher,
		}),
		Migration: NewMigrationCoordinator(gdb, cipher, desired, audit),
		Audit:     audit,
		LogRing:   logging.NewRing(10),
	})
	if err != nil {
		t.Fatalf("new admin server: %v", err)
	}
	return server
}

// loginFrom 从 remoteAddr 发起一次登录,可选携带 X-Forwarded-For。
func loginFrom(server *AdminServer, remoteAddr, xff, password string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login",
		bytes.NewBufferString(`{"password":"`+password+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rr := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(rr, req)
	return rr
}

// R1(AC8):不写 trust_forwarded 时行为与升级前完全一致——转发头一律不被采信,
// 登录失败按 TCP 来源地址计数,伪造 X-Forwarded-For 换不来新的失败额度。
func TestAdminLoginIgnoresForwardedHeaderByDefault(t *testing.T) {
	server := newTrustForwardedTestServer(t, false)

	if first := loginFrom(server, "192.0.2.9:1111", "198.51.100.1", "wrong"); first.Code != http.StatusUnauthorized {
		t.Fatalf("first login = %d, want 401, body=%s", first.Code, first.Body.String())
	}
	// 换一个伪造 IP:默认口径下仍算同一个来源,必须已被锁定。
	second := loginFrom(server, "192.0.2.9:2222", "198.51.100.2", "panel-password")
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second login = %d, want 429 (forwarded header must not be trusted), body=%s",
			second.Code, second.Body.String())
	}
}

// R1(AC8):开启后登录限流按转发头里的真实客户端 IP 计数(取最右一段,由受信反代写入),
// 不同客户端之间互不牵连;这条同时证明 PanelConfig.TrustForwarded 确实接到了认证栈上
// ——若接线漏了,下面第二次请求会因为共用同一个 RemoteAddr 而被误锁成 429。
func TestAdminLoginHonorsTrustForwardedFromPanelConfig(t *testing.T) {
	server := newTrustForwardedTestServer(t, true)

	// 同一个反代(RemoteAddr 相同)后面的两个不同客户端。
	if first := loginFrom(server, "192.0.2.9:1111", "198.51.100.1", "wrong"); first.Code != http.StatusUnauthorized {
		t.Fatalf("first login = %d, want 401, body=%s", first.Code, first.Body.String())
	}
	other := loginFrom(server, "192.0.2.9:2222", "198.51.100.2", "panel-password")
	if other.Code != http.StatusOK {
		t.Fatalf("second client login = %d, want 200 (lockout must be per real client IP), body=%s",
			other.Code, other.Body.String())
	}
	// 而同一个客户端 IP 重试则应命中锁定。
	same := loginFrom(server, "192.0.2.9:3333", "198.51.100.1", "panel-password")
	if same.Code != http.StatusTooManyRequests {
		t.Fatalf("same client retry = %d, want 429, body=%s", same.Code, same.Body.String())
	}
}

// R2:管理 server 必须有 Read/Idle 超时兜住慢速 body 与空闲 keep-alive 连接;
// 同时不能设 WriteTimeout(大响应的正常下载不该被误杀)。
func TestAdminServerHasReadAndIdleTimeouts(t *testing.T) {
	server := newTrustForwardedTestServer(t, false)

	if got := server.httpServer.ReadHeaderTimeout; got != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v, want 10s", got)
	}
	if got := server.httpServer.ReadTimeout; got != 30*time.Second {
		t.Fatalf("ReadTimeout = %v, want 30s", got)
	}
	if got := server.httpServer.IdleTimeout; got != 120*time.Second {
		t.Fatalf("IdleTimeout = %v, want 120s", got)
	}
	if got := server.httpServer.WriteTimeout; got != 0 {
		t.Fatalf("WriteTimeout = %v, want unset", got)
	}
}
