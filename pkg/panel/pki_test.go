package panel

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestIntermediateCA writes a self-signed intermediate CA cert+key to a temp
// dir and returns a loaded *CA plus the file paths.
func newTestIntermediateCA(t *testing.T) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ca key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-intermediate-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write ca cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal ca key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write ca key: %v", err)
	}

	ca, err := LoadIntermediateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("load ca: %v", err)
	}
	return ca
}

// newNodeCSR generates a node private key + CSR PEM (the node keeps the key).
func newNodeCSR(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen node key: %v", err)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "node-request"}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestSignNodeCSRAndValidate(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC()

	csrPEM := newNodeCSR(t)
	signed, err := ca.SignNodeCSR(csrPEM, node.ID, 0, now)
	if err != nil {
		t.Fatalf("sign csr: %v", err)
	}
	if signed.Fingerprint == "" || len(signed.Fingerprint) != 64 {
		t.Fatalf("bad fingerprint %q", signed.Fingerprint)
	}

	// Persist the issued cert as the panel would.
	cert := NodeCert{
		NodeID:      node.ID,
		Fingerprint: signed.Fingerprint,
		Serial:      signed.Serial,
		NotBefore:   signed.NotBefore,
		NotAfter:    signed.NotAfter,
	}
	if err := gdb.Create(&cert).Error; err != nil {
		t.Fatalf("store cert: %v", err)
	}

	// A valid, unrevoked cert on an active node is accepted.
	nodeID, ok, err := IsCertValid(gdb, signed.Fingerprint, now)
	if err != nil || !ok {
		t.Fatalf("IsCertValid = (%d,%v,%v), want valid", nodeID, ok, err)
	}
	if nodeID != node.ID {
		t.Fatalf("nodeID = %d, want %d", nodeID, node.ID)
	}

	// Unknown fingerprint is rejected.
	if _, ok, _ := IsCertValid(gdb, "unknown", now); ok {
		t.Fatal("unknown fingerprint must be rejected")
	}

	// Expired cert is rejected.
	if _, ok, _ := IsCertValid(gdb, signed.Fingerprint, signed.NotAfter.Add(time.Hour)); ok {
		t.Fatal("expired cert must be rejected")
	}
}

func TestRevokedCertRejected(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC()
	signed, err := ca.SignNodeCSR(newNodeCSR(t), node.ID, 0, now)
	if err != nil {
		t.Fatalf("sign csr: %v", err)
	}
	cert := NodeCert{NodeID: node.ID, Fingerprint: signed.Fingerprint, Serial: signed.Serial, NotBefore: signed.NotBefore, NotAfter: signed.NotAfter}
	if err := gdb.Create(&cert).Error; err != nil {
		t.Fatalf("store cert: %v", err)
	}

	n, err := RevokeNodeCerts(gdb, node.ID, now)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n != 1 {
		t.Fatalf("revoked = %d, want 1", n)
	}
	if _, ok, _ := IsCertValid(gdb, signed.Fingerprint, now); ok {
		t.Fatal("revoked cert must be rejected")
	}
}

func TestRetiredNodeCertRejected(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC()
	signed, err := ca.SignNodeCSR(newNodeCSR(t), node.ID, 0, now)
	if err != nil {
		t.Fatalf("sign csr: %v", err)
	}
	cert := NodeCert{NodeID: node.ID, Fingerprint: signed.Fingerprint, Serial: signed.Serial, NotBefore: signed.NotBefore, NotAfter: signed.NotAfter}
	if err := gdb.Create(&cert).Error; err != nil {
		t.Fatalf("store cert: %v", err)
	}
	// Retire the node: its certs must no longer be accepted for control-plane.
	if err := gdb.Model(&node).Update("status", NodeStatusRetired).Error; err != nil {
		t.Fatalf("retire node: %v", err)
	}
	if _, ok, _ := IsCertValid(gdb, signed.Fingerprint, now); ok {
		t.Fatal("retired node cert must be rejected")
	}
}

func TestDisabledNodeCertCanResumeAfterReactivation(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC()
	signed, err := ca.SignNodeCSR(newNodeCSR(t), node.ID, 0, now)
	if err != nil {
		t.Fatalf("sign csr: %v", err)
	}
	cert := NodeCert{NodeID: node.ID, Fingerprint: signed.Fingerprint, Serial: signed.Serial, NotBefore: signed.NotBefore, NotAfter: signed.NotAfter}
	if err := gdb.Create(&cert).Error; err != nil {
		t.Fatalf("store cert: %v", err)
	}
	if err := gdb.Model(&node).Update("status", NodeStatusDisabled).Error; err != nil {
		t.Fatalf("disable node: %v", err)
	}
	if _, ok, _ := IsCertValid(gdb, signed.Fingerprint, now); ok {
		t.Fatal("disabled node cert must be rejected")
	}
	if err := gdb.Model(&node).Update("status", NodeStatusActive).Error; err != nil {
		t.Fatalf("reactivate node: %v", err)
	}
	if _, ok, err := IsCertValid(gdb, signed.Fingerprint, now); err != nil || !ok {
		t.Fatalf("reactivated node cert should be accepted: ok=%v err=%v", ok, err)
	}
}

func TestSignRejectsBadCSR(t *testing.T) {
	ca := newTestIntermediateCA(t)
	if _, err := ca.SignNodeCSR([]byte("not a csr"), 1, 0, time.Now()); err == nil {
		t.Fatal("expected error for malformed CSR")
	}
}
