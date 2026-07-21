package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"golang.org/x/crypto/bcrypt"
)

func TestAdminServerUsesPanelModeAndPanelRoutes(t *testing.T) {
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
		AdminAddr: "127.0.0.1:0",
		WebAdmin: config.WebAdminConfig{
			PasswordHash:      string(passwordHash),
			SessionSecret:     "test-panel-session-secret",
			SessionTTLMinutes: 10,
		},
	}
	hub := NewHub()
	creds := NewPanelCredentialStore(gdb, cipher)
	desired := NewDesiredStateAuthority(gdb, cipher)
	audit := NewAuditor(gdb)
	tasks := NewTaskOrchestrator(gdb, hub, 0)
	transport := NewTransportServer(TransportDeps{DB: gdb, Hub: hub, Cipher: cipher})
	migration := NewMigrationCoordinator(gdb, cipher, desired, audit)

	server, err := NewAdminServer(AdminServerDeps{
		Config: cfg, DB: gdb, Hub: hub, Creds: creds, Desired: desired,
		Tasks: tasks, Transport: transport, Migration: migration, Audit: audit,
	})
	if err != nil {
		t.Fatalf("new admin server: %v", err)
	}

	settings := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(settings, httptest.NewRequest(http.MethodGet, "/api/admin/auth-settings", nil))
	if settings.Code != http.StatusOK {
		t.Fatalf("auth settings status = %d, body=%s", settings.Code, settings.Body.String())
	}
	var settingsBody map[string]any
	if err := json.Unmarshal(settings.Body.Bytes(), &settingsBody); err != nil {
		t.Fatalf("decode auth settings: %v", err)
	}
	if settingsBody["service_mode"] != string("panel") {
		t.Fatalf("service_mode = %v, want panel", settingsBody["service_mode"])
	}

	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodPost, "/api/admin/login", bytes.NewBufferString(`{"password":"panel-password"}`))
	loginRequest.Header.Set("Content-Type", "application/json")
	server.httpServer.Handler.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a session cookie")
	}

	serveAuthenticated := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookies[0])
		rr := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(rr, req)
		return rr
	}

	nodes := serveAuthenticated("/api/admin/nodes")
	if nodes.Code != http.StatusOK || nodes.Body.String() != "[]\n" {
		t.Fatalf("panel nodes status = %d, body=%s", nodes.Code, nodes.Body.String())
	}
	standaloneRoute := serveAuthenticated("/api/admin/dashboard/summary")
	if standaloneRoute.Code != http.StatusNotFound || !bytes.Contains(standaloneRoute.Body.Bytes(), []byte(`"not found"`)) {
		t.Fatalf("standalone route status = %d, body=%s", standaloneRoute.Code, standaloneRoute.Body.String())
	}
}
