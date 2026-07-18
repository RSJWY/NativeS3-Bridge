package nodeagent

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
