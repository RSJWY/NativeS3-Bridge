package panel

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
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

	ca, err := LoadIntermediateCA(certPath, keyPath, time.Now())
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

func TestActivateCertRevokesOthers(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC()

	// Issue two certs for the same node.
	signed1, _ := ca.SignNodeCSR(newNodeCSR(t), node.ID, 0, now)
	signed2, _ := ca.SignNodeCSR(newNodeCSR(t), node.ID, 0, now)
	cert1 := NodeCert{NodeID: node.ID, Fingerprint: signed1.Fingerprint, Serial: signed1.Serial, NotBefore: signed1.NotBefore, NotAfter: signed1.NotAfter}
	cert2 := NodeCert{NodeID: node.ID, Fingerprint: signed2.Fingerprint, Serial: signed2.Serial, NotBefore: signed2.NotBefore, NotAfter: signed2.NotAfter}
	if err := gdb.Create(&cert1).Error; err != nil {
		t.Fatalf("store cert1: %v", err)
	}
	if err := gdb.Create(&cert2).Error; err != nil {
		t.Fatalf("store cert2: %v", err)
	}

	// Activate cert2: cert1 should be revoked, cert2 should not.
	if err := ActivateCert(gdb, signed2.Fingerprint, node.ID, now); err != nil {
		t.Fatalf("activate cert2: %v", err)
	}

	var row1, row2 NodeCert
	gdb.Where("fingerprint = ?", signed1.Fingerprint).First(&row1)
	gdb.Where("fingerprint = ?", signed2.Fingerprint).First(&row2)
	if !row1.Revoked {
		t.Fatal("old cert (cert1) must be revoked after activating cert2")
	}
	if row2.Revoked {
		t.Fatal("new cert (cert2) must not be revoked after activation")
	}
	if row2.ActivatedAt == nil {
		t.Fatal("new cert (cert2) must have activated_at set")
	}
}

func TestActivateCertIdempotent(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC()

	signed1, _ := ca.SignNodeCSR(newNodeCSR(t), node.ID, 0, now)
	signed2, _ := ca.SignNodeCSR(newNodeCSR(t), node.ID, 0, now)
	gdb.Create(&NodeCert{NodeID: node.ID, Fingerprint: signed1.Fingerprint, Serial: signed1.Serial, NotBefore: signed1.NotBefore, NotAfter: signed1.NotAfter})
	gdb.Create(&NodeCert{NodeID: node.ID, Fingerprint: signed2.Fingerprint, Serial: signed2.Serial, NotBefore: signed2.NotBefore, NotAfter: signed2.NotAfter})

	// Activate cert2 three times.
	for i := 0; i < 3; i++ {
		if err := ActivateCert(gdb, signed2.Fingerprint, node.ID, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("activate cert2 attempt %d: %v", i, err)
		}
	}

	// cert1 should be revoked exactly once (no double-revoke issues).
	var row1 NodeCert
	gdb.Where("fingerprint = ?", signed1.Fingerprint).First(&row1)
	if !row1.Revoked {
		t.Fatal("cert1 should be revoked")
	}

	// cert2's activated_at should not change after the first call.
	var row2 NodeCert
	gdb.Where("fingerprint = ?", signed2.Fingerprint).First(&row2)
	if row2.ActivatedAt == nil {
		t.Fatal("cert2 should have activated_at set")
	}
	firstActivated := *row2.ActivatedAt

	// Re-activate and verify it didn't change.
	_ = ActivateCert(gdb, signed2.Fingerprint, node.ID, now.Add(time.Hour))
	gdb.Where("fingerprint = ?", signed2.Fingerprint).First(&row2)
	if !row2.ActivatedAt.Equal(firstActivated) {
		t.Fatalf("activated_at changed after re-activation: %v -> %v", firstActivated, *row2.ActivatedAt)
	}
}

func TestActivateCertNoOldCerts(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC()

	// Only one cert, no old certs to revoke.
	signed, _ := ca.SignNodeCSR(newNodeCSR(t), node.ID, 0, now)
	gdb.Create(&NodeCert{NodeID: node.ID, Fingerprint: signed.Fingerprint, Serial: signed.Serial, NotBefore: signed.NotBefore, NotAfter: signed.NotAfter})

	if err := ActivateCert(gdb, signed.Fingerprint, node.ID, now); err != nil {
		t.Fatalf("activate: %v", err)
	}
	var row NodeCert
	gdb.Where("fingerprint = ?", signed.Fingerprint).First(&row)
	if row.Revoked {
		t.Fatal("sole cert must not be revoked")
	}
	if row.ActivatedAt == nil {
		t.Fatal("sole cert must have activated_at set")
	}
}

// writeTestCAFiles writes a self-signed CA cert+key to dir with the given
// NotBefore/NotAfter, returning the cert and key file paths.
func writeTestCAFiles(t *testing.T, dir string, notBefore, notAfter time.Time) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ca key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-intermediate-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	certPath = filepath.Join(dir, "ca.crt")
	keyPath = filepath.Join(dir, "ca.key")
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
	return certPath, keyPath
}

func TestLoadIntermediateCAExpired(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	notBefore := now.Add(-48 * time.Hour)
	notAfter := now.Add(-time.Hour)
	certPath, keyPath := writeTestCAFiles(t, dir, notBefore, notAfter)

	_, err := LoadIntermediateCA(certPath, keyPath, now)
	if err == nil {
		t.Fatal("expected error for expired CA")
	}
	if !strings.Contains(err.Error(), "intermediate CA expired") {
		t.Fatalf("error should mention CA expiry, got: %v", err)
	}
}

func TestLoadIntermediateCANotYetValid(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	notBefore := now.Add(time.Hour)
	notAfter := now.Add(48 * time.Hour)
	certPath, keyPath := writeTestCAFiles(t, dir, notBefore, notAfter)

	_, err := LoadIntermediateCA(certPath, keyPath, now)
	if err == nil {
		t.Fatal("expected error for not-yet-valid CA")
	}
	if !strings.Contains(err.Error(), "not yet valid") {
		t.Fatalf("error should mention CA not yet valid, got: %v", err)
	}
}

func TestLoadIntermediateCAValidNoError(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	notBefore := now.Add(-time.Hour)
	notAfter := now.Add(365 * 24 * time.Hour)
	certPath, keyPath := writeTestCAFiles(t, dir, notBefore, notAfter)

	ca, err := LoadIntermediateCA(certPath, keyPath, now)
	if err != nil {
		t.Fatalf("expected success for valid CA, got: %v", err)
	}
	if ca == nil {
		t.Fatal("CA should not be nil")
	}
}

func TestLoadIntermediateCANearExpiry(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	// 30 days remaining: inside the 90-day warn window but not expired.
	notBefore := now.Add(-24 * time.Hour)
	notAfter := now.Add(30 * 24 * time.Hour)
	certPath, keyPath := writeTestCAFiles(t, dir, notBefore, notAfter)

	// Capture slog output to assert the near-expiry warning fires.
	var buf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prevLogger)

	// Near expiry must NOT block startup: load succeeds with a non-nil CA.
	ca, err := LoadIntermediateCA(certPath, keyPath, now)
	if err != nil {
		t.Fatalf("near-expiry CA should load successfully, got: %v", err)
	}
	if ca == nil {
		t.Fatal("CA should not be nil")
	}
	if !strings.Contains(buf.String(), "intermediate CA is approaching expiry") {
		t.Fatalf("expected near-expiry warning in log, got: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "remaining_days") {
		t.Fatalf("expected remaining_days in warning, got: %q", buf.String())
	}
}
