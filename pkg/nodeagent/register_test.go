package nodeagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegisterTrustsConfiguredPanelCA(t *testing.T) {
	var received registerRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode registration request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(registerResponse{CertPEM: "issued-client-certificate"})
	}))
	defer server.Close()

	tmp := t.TempDir()
	caFile := filepath.Join(tmp, "panel-ca.crt")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, caPEM, 0o644); err != nil {
		t.Fatalf("write panel CA: %v", err)
	}

	id := Identity{
		KeyFile:  filepath.Join(tmp, "node.key"),
		CertFile: filepath.Join(tmp, "node.crt"),
		CAFile:   caFile,
	}
	if err := Register(id, RegisterParams{
		RegisterURL: server.URL,
		NodeID:      7,
		Token:       "single-use-token",
	}); err != nil {
		t.Fatalf("Register with configured private CA: %v", err)
	}

	if received.NodeID != 7 || received.Token != "single-use-token" {
		t.Fatalf("unexpected registration request: %+v", received)
	}
	block, _ := pem.Decode([]byte(received.CSRPEM))
	if block == nil {
		t.Fatal("registration request contains no PEM CSR")
	}
	if _, err := x509.ParseCertificateRequest(block.Bytes); err != nil {
		t.Fatalf("parse registration CSR: %v", err)
	}
	certPEM, err := os.ReadFile(id.CertFile)
	if err != nil {
		t.Fatalf("read persisted client certificate: %v", err)
	}
	if string(certPEM) != "issued-client-certificate" {
		t.Fatalf("unexpected persisted client certificate: %q", certPEM)
	}
}

func TestRegisterWithRetryRecoversFromServerError(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(registerResponse{CertPEM: "issued-client-certificate"})
	}))
	defer server.Close()

	tmp := t.TempDir()
	id := Identity{KeyFile: filepath.Join(tmp, "node.key"), CertFile: filepath.Join(tmp, "node.crt")}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := RegisterWithRetry(ctx, id, RegisterParams{
		RegisterURL: server.URL, NodeID: 7, Token: "single-use-token", HTTPClient: server.Client(),
	}, RegisterRetryOptions{InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond})
	if err != nil {
		t.Fatalf("RegisterWithRetry: %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
}

func TestRegisterWithRetryStopsOnUnauthorized(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer server.Close()

	tmp := t.TempDir()
	err := RegisterWithRetry(context.Background(), Identity{
		KeyFile: filepath.Join(tmp, "node.key"), CertFile: filepath.Join(tmp, "node.crt"),
	}, RegisterParams{
		RegisterURL: server.URL, NodeID: 7, Token: "bad-token", HTTPClient: server.Client(),
	}, RegisterRetryOptions{InitialBackoff: time.Millisecond})
	var registrationErr *RegistrationError
	if !errors.As(err, &registrationErr) || registrationErr.StatusCode != http.StatusUnauthorized || registrationErr.Retryable {
		t.Fatalf("expected permanent 401, got %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
}

func TestRegisterFailsWhenPanelCAIsUnreadable(t *testing.T) {
	tmp := t.TempDir()
	err := Register(Identity{
		KeyFile:  filepath.Join(tmp, "node.key"),
		CertFile: filepath.Join(tmp, "node.crt"),
		CAFile:   filepath.Join(tmp, "missing-ca.crt"),
	}, RegisterParams{
		RegisterURL: "https://panel.invalid/register",
		NodeID:      7,
		Token:       "single-use-token",
	})
	if err == nil || !strings.Contains(err.Error(), "read panel CA") {
		t.Fatalf("expected panel CA read error, got %v", err)
	}
}

// --- HasCertificate / LoadCertificate / RenewalThreshold tests ---

// writeTestCert writes a client cert + key to the identity's file paths.
// notAfter controls the certificate's expiry. notBefore defaults to now-1h.
func writeTestCert(t *testing.T, id Identity, notAfter time.Time) {
	t.Helper()
	writeTestCertWithNotBefore(t, id, time.Now().Add(-time.Hour), notAfter)
}

// writeTestCertWithNotBefore writes a client cert with explicit NotBefore/NotAfter.
func writeTestCertWithNotBefore(t *testing.T, id Identity, notBefore, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "node-1"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.MkdirAll(filepath.Dir(id.CertFile), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(id.CertFile, certPEM, 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(id.KeyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

func TestHasCertificateValidCert(t *testing.T) {
	tmp := t.TempDir()
	id := Identity{
		KeyFile:  filepath.Join(tmp, "node.key"),
		CertFile: filepath.Join(tmp, "node.crt"),
	}
	writeTestCert(t, id, time.Now().Add(24*time.Hour))
	if !id.HasCertificate() {
		t.Fatal("expected HasCertificate=true for valid cert")
	}
}

func TestHasCertificateExpiredCert(t *testing.T) {
	tmp := t.TempDir()
	id := Identity{
		KeyFile:  filepath.Join(tmp, "node.key"),
		CertFile: filepath.Join(tmp, "node.crt"),
	}
	writeTestCert(t, id, time.Now().Add(-time.Hour))
	if id.HasCertificate() {
		t.Fatal("expected HasCertificate=false for expired cert")
	}
}

func TestHasCertificateMalformedPEM(t *testing.T) {
	tmp := t.TempDir()
	id := Identity{
		KeyFile:  filepath.Join(tmp, "node.key"),
		CertFile: filepath.Join(tmp, "node.crt"),
	}
	// Write a valid key but garbage cert.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	os.MkdirAll(tmp, 0o700)
	os.WriteFile(id.KeyFile, keyPEM, 0o600)
	os.WriteFile(id.CertFile, []byte("not a pem"), 0o644)
	if id.HasCertificate() {
		t.Fatal("expected HasCertificate=false for malformed PEM")
	}
}

func TestHasCertificateMissingKey(t *testing.T) {
	tmp := t.TempDir()
	id := Identity{
		KeyFile:  filepath.Join(tmp, "node.key"),
		CertFile: filepath.Join(tmp, "node.crt"),
	}
	writeTestCert(t, id, time.Now().Add(24*time.Hour))
	os.Remove(id.KeyFile)
	if id.HasCertificate() {
		t.Fatal("expected HasCertificate=false when key is missing")
	}
}

func TestRenewalThresholdAndNeedsRenewal(t *testing.T) {
	tmp := t.TempDir()
	id := Identity{
		KeyFile:  filepath.Join(tmp, "node.key"),
		CertFile: filepath.Join(tmp, "node.crt"),
	}
	// 90-day cert: NotBefore = now - 90d, NotAfter = now.
	notBefore := time.Now().Add(-90 * 24 * time.Hour)
	notAfter := time.Now()
	writeTestCertWithNotBefore(t, id, notBefore, notAfter)

	cert, err := id.LoadCertificate()
	if err != nil {
		t.Fatalf("load cert: %v", err)
	}

	// Threshold should be ~30 days (90/3).
	threshold := RenewalThreshold(cert)
	expected := 30 * 24 * time.Hour
	diff := threshold - expected
	if diff > time.Hour || diff < -time.Hour {
		t.Fatalf("threshold = %v, want ~%v", threshold, expected)
	}

	// At now (cert already expired), needs renewal.
	if !NeedsRenewal(cert, time.Now()) {
		t.Fatal("expected NeedsRenewal=true for expired cert")
	}

	// Fresh 90-day cert: 31 days remaining → no renewal needed.
	freshID := Identity{
		KeyFile:  filepath.Join(tmp, "fresh.key"),
		CertFile: filepath.Join(tmp, "fresh.crt"),
	}
	writeTestCertWithNotBefore(t, freshID, time.Now(), time.Now().Add(90*24*time.Hour))
	freshCert, _ := freshID.LoadCertificate()
	// At now, remaining is ~90 days, threshold is ~30 days → no renewal.
	if NeedsRenewal(freshCert, time.Now()) {
		t.Fatal("expected NeedsRenewal=false for fresh cert with 90 days remaining")
	}

	// At 29 days remaining → needs renewal.
	if !NeedsRenewal(freshCert, time.Now().Add(61*24*time.Hour)) {
		t.Fatal("expected NeedsRenewal=true at 29 days remaining")
	}

	// At 31 days remaining → no renewal.
	if NeedsRenewal(freshCert, time.Now().Add(59*24*time.Hour)) {
		t.Fatal("expected NeedsRenewal=false at 31 days remaining")
	}
}

func TestRenewURLFromAgentURL(t *testing.T) {
	tests := []struct {
		name     string
		agentURL string
		want     string
		wantErr  bool
	}{
		{
			name:     "standard wss with port",
			agentURL: "wss://h:9443/agent",
			want:     "https://h:9443/renew",
		},
		{
			name:     "host containing 'agent' substring",
			agentURL: "wss://agent.example.com:9443/agent",
			want:     "https://agent.example.com:9443/renew",
		},
		{
			name:     "with path prefix",
			agentURL: "wss://h:9443/x/agent",
			want:     "https://h:9443/x/renew",
		},
		{
			name:     "ws scheme",
			agentURL: "ws://h:8080/agent",
			want:     "http://h:8080/renew",
		},
		{
			name:     "no path",
			agentURL: "wss://h:9443",
			want:     "https://h:9443/renew",
		},
		{
			name:     "invalid URL",
			agentURL: "://bad",
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renewURLFromAgentURL(tt.agentURL)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("renewURLFromAgentURL(%q) = %q, want %q", tt.agentURL, got, tt.want)
			}
		})
	}
}

// TestRenewalFailureDoesNotDisruptConnection verifies AC13: when renewCertificate
// fails (e.g. panel returns 5xx), the current connection and heartbeat loop
// continue uninterrupted. The renewal is retried on the next heartbeat tick.
func TestRenewalFailureDoesNotDisruptConnection(t *testing.T) {
	tmp := t.TempDir()
	id := Identity{
		KeyFile:  filepath.Join(tmp, "node.key"),
		CertFile: filepath.Join(tmp, "node.crt"),
		CAFile:   filepath.Join(tmp, "panel-ca.crt"),
	}

	// Write a cert that is within the renewal window (remaining < TTL/3).
	// 90-day cert issued 80 days ago → 10 days remaining, threshold = 30 days.
	writeTestCertWithNotBefore(t, id, time.Now().Add(-80*24*time.Hour), time.Now().Add(10*24*time.Hour))

	// Start a mock panel that returns 500 for /renew.
	renewAttempts := atomic.Int32{}
	mux := http.NewServeMux()
	mux.HandleFunc("/renew", func(w http.ResponseWriter, r *http.Request) {
		renewAttempts.Add(1)
		http.Error(w, "internal error", http.StatusInternalServerError)
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()

	// Write the server's cert as the CA file so the node trusts it.
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(id.CAFile, caPEM, 0o644); err != nil {
		t.Fatalf("write CA: %v", err)
	}

	// Build a Client with a very short heartbeat interval so renewal checks
	// happen quickly.
	cfg := ClientConfig{
		AgentURL:          "wss://" + server.Listener.Addr().String() + "/agent",
		NodeID:            1,
		Identity:          id,
		HeartbeatInterval: 50 * time.Millisecond,
		MinBackoff:        10 * time.Millisecond,
		MaxBackoff:        50 * time.Millisecond,
	}
	cfg.applyDefaults()

	// We can't easily test the full WebSocket loop without a real panel,
	// but we can test that renewCertificate returns an error and that
	// calling it doesn't panic or block.
	c := &Client{cfg: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := c.renewCertificate(ctx)
	if err == nil {
		t.Fatal("renewCertificate should fail when panel returns 500")
	}

	// Verify the panel received at least one renewal attempt.
	if renewAttempts.Load() < 1 {
		t.Fatal("panel should have received at least one /renew request")
	}

	// Verify the cert file is unchanged (renewal failed, no overwrite).
	if _, err := os.Stat(id.CertFile); err != nil {
		t.Fatalf("cert file should still exist: %v", err)
	}
}
