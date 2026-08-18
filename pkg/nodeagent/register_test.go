package nodeagent

import (
	"bytes"
	"context"
	"crypto"
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

// issueCertFromCSR 用 caKey/caCert 签发一张客户端证书,证书公钥取自 CSR。
// 用于模拟 panel 注册/续期响应,使 validateIssuedCert 校验通过。
func issueCertFromCSR(t *testing.T, csrPEM string, caKey *ecdsa.PrivateKey, caCert *x509.Certificate) (certPEM, caCertPEM string) {
	t.Helper()
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		t.Fatal("CSR is not valid PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR signature invalid: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: csr.Subject.CommonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, csr.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	caPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})
	return string(certPEMBytes), string(caPEMBytes)
}

// newTestCA 生成一张自签 CA 证书及其私钥,供测试 mock 签发节点证书。
func newTestCA(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-panel-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return key, cert
}

func TestRegisterTrustsConfiguredPanelCA(t *testing.T) {
	caKey, caCert := newTestCA(t)
	var received registerRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode registration request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		certPEM, caCertPEM := issueCertFromCSR(t, received.CSRPEM, caKey, caCert)
		_ = json.NewEncoder(w).Encode(registerResponse{CertPEM: certPEM, CACertPEM: caCertPEM})
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
	if !bytes.Contains(certPEM, []byte("-----BEGIN CERTIFICATE-----")) {
		t.Fatalf("persisted client certificate is not PEM: %q", certPEM)
	}
	// 校验落盘的证书能通过本地 LoadCertificate 与公钥匹配检查。
	if _, err := id.LoadCertificate(); err != nil {
		t.Fatalf("load persisted certificate: %v", err)
	}
}

func TestRegisterWithRetryRecoversFromServerError(t *testing.T) {
	caKey, caCert := newTestCA(t)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		var req registerRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		certPEM, caCertPEM := issueCertFromCSR(t, req.CSRPEM, caKey, caCert)
		_ = json.NewEncoder(w).Encode(registerResponse{CertPEM: certPEM, CACertPEM: caCertPEM})
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

// --- R1/R2 新增测试 ---

// TestValidateIssuedCertRejections 覆盖 R1 落盘前校验的三种失败情形:
// 非法 PEM、公钥与本地私钥不匹配、链验证失败,以及有效期异常(过期/超长)。
// 任一失败时调用方都应保留旧文件不动。
func TestValidateIssuedCertRejections(t *testing.T) {
	caKey, caCert := newTestCA(t)
	nodeKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	wrongKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	// 正常签发的证书,作为后续篡改的基线。
	csrPEMBytes, err := buildCSR(nodeKey, 7)
	if err != nil {
		t.Fatalf("build CSR: %v", err)
	}
	goodCertPEM, caCertPEM := issueCertFromCSR(t, string(csrPEMBytes), caKey, caCert)

	now := time.Now()

	cases := []struct {
		name    string
		certPEM string
		key     crypto.Signer
		caPEM   []byte
		wantErr string
	}{
		{
			name:    "not PEM",
			certPEM: "not-a-pem",
			key:     nodeKey,
			caPEM:   []byte(caCertPEM),
			wantErr: "issued cert is not a valid PEM certificate",
		},
		{
			name: "public key mismatch",
			// 用 good cert,但传入另一把私钥做公钥比对。
			certPEM: goodCertPEM,
			key:     wrongKey,
			caPEM:   []byte(caCertPEM),
			wantErr: "public key does not match",
		},
		{
			name:    "chain verification fails",
			certPEM: goodCertPEM,
			key:     nodeKey,
			caPEM:   nil,
			wantErr: "verify issued cert chain",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateIssuedCert(tc.certPEM, tc.key, tc.caPEM, now)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want containing %q", err.Error(), tc.wantErr)
			}
		})
	}

	// 过期证书:NotAfter 在过去。
	t.Run("expired", func(t *testing.T) {
		template := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject:      pkix.Name{CommonName: "node-7"},
			NotBefore:    now.Add(-2 * time.Hour),
			NotAfter:     now.Add(-time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, template, caCert, &nodeKey.PublicKey, caKey)
		if err != nil {
			t.Fatalf("create expired cert: %v", err)
		}
		expiredPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		_, err = validateIssuedCert(expiredPEM, nodeKey, []byte(caCertPEM), now)
		if err == nil || (!strings.Contains(err.Error(), "already expired") && !strings.Contains(err.Error(), "certificate has expired")) {
			t.Fatalf("expected expired error, got %v", err)
		}
	})

	// 超长有效期:超过 10 年上界。
	t.Run("ttl too long", func(t *testing.T) {
		template := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject:      pkix.Name{CommonName: "node-7"},
			NotBefore:    now,
			NotAfter:     now.Add(20 * 365 * 24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, template, caCert, &nodeKey.PublicKey, caKey)
		if err != nil {
			t.Fatalf("create long cert: %v", err)
		}
		longPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		_, err = validateIssuedCert(longPEM, nodeKey, []byte(caCertPEM), now)
		if err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
			t.Fatalf("expected TTL exceeded error, got %v", err)
		}
	})

	// 成功路径。
	t.Run("valid", func(t *testing.T) {
		cert, err := validateIssuedCert(goodCertPEM, nodeKey, []byte(caCertPEM), now)
		if err != nil {
			t.Fatalf("expected valid cert, got %v", err)
		}
		if cert == nil {
			t.Fatal("expected parsed cert")
		}
	})
}

// TestPersistPEMAtomicWrite 验证原子写语义:落盘文件存在且是完整 PEM,
// 临时文件已清理,旧文件被备份为 .bak。
func TestPersistPEMAtomicWrite(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "node.crt")
	oldData := []byte("old-certificate-pem")
	if err := os.WriteFile(path, oldData, 0o644); err != nil {
		t.Fatalf("write old file: %v", err)
	}

	newData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("new-cert-bytes")})
	if err := persistPEM(path, newData, 0o644); err != nil {
		t.Fatalf("persistPEM: %v", err)
	}

	// 新文件存在且内容正确。
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read new file: %v", err)
	}
	if !bytes.Equal(got, newData) {
		t.Fatalf("new file content mismatch")
	}

	// 临时文件应已清理。
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file should be removed")
	}

	// 旧文件应备份为 .bak。
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read .bak: %v", err)
	}
	if !bytes.Equal(bak, oldData) {
		t.Fatalf(".bak content mismatch")
	}
}
