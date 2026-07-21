package nodeagent

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
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
