package panel

import (
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

func newTestMigration(t *testing.T) (*MigrationCoordinator, *SecretCipher) {
	t.Helper()
	gdb := openTestDB(t)
	key := make([]byte, masterKeyLen)
	cipher, err := NewSecretCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	desired := NewDesiredStateAuthority(gdb, cipher)
	m := NewMigrationCoordinator(gdb, cipher, desired, NewAuditor(gdb))
	return m, cipher
}

func sampleReport() controlproto.ImportReportPayload {
	state := controlproto.DesiredState{
		Credentials: []controlproto.DesiredCredential{
			{AccessKey: "AKIMPORT", SecretKey: "plainsecret", Status: "enabled", QuotaBytes: 100},
		},
		Buckets: []controlproto.DesiredBucket{{Name: "b1", ACL: "private"}},
	}
	return controlproto.ImportReportPayload{
		State:            state,
		CredentialCount:  1,
		BucketCount:      1,
		LocalContentHash: state.ContentHash(),
	}
}

// TestImportIsReadOnlyBeforeConfirm is the migration red line: ingesting a
// report must NOT write any business config to the panel's authoritative tables.
func TestImportIsReadOnlyBeforeConfirm(t *testing.T) {
	m, _ := newTestMigration(t)
	const nodeID = 1

	if err := m.ingestReport(nodeID, sampleReport()); err != nil {
		t.Fatalf("ingestReport: %v", err)
	}

	// Nothing must be written to node_credentials / node_buckets yet.
	var credCount, bucketCount int64
	m.db.Model(&NodeCredential{}).Where("node_id = ?", nodeID).Count(&credCount)
	m.db.Model(&NodeBucket{}).Where("node_id = ?", nodeID).Count(&bucketCount)
	if credCount != 0 || bucketCount != 0 {
		t.Fatalf("import wrote config before confirm: creds=%d buckets=%d", credCount, bucketCount)
	}

	// A summary is available and carries no plaintext secret.
	summary, ok := m.PendingSummary(nodeID)
	if !ok || summary.CredentialCount != 1 {
		t.Fatalf("pending summary missing: %+v ok=%v", summary, ok)
	}
}

// TestImportConfirmAdoptsAndPublishes verifies confirm adopts config into panel
// tables (with encrypted secret) and publishes the version=1 baseline.
func TestImportConfirmAdoptsAndPublishes(t *testing.T) {
	m, cipher := newTestMigration(t)
	const nodeID = 1
	if err := m.ingestReport(nodeID, sampleReport()); err != nil {
		t.Fatalf("ingestReport: %v", err)
	}

	version, hash, err := m.Confirm(nodeID, "admin")
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if version != 1 {
		t.Fatalf("baseline version = %d, want 1", version)
	}
	if hash == "" {
		t.Fatal("empty content hash")
	}

	// The credential is adopted and the secret is stored encrypted (not plaintext).
	var cred NodeCredential
	if err := m.db.Where("node_id = ? AND access_key = ?", nodeID, "AKIMPORT").First(&cred).Error; err != nil {
		t.Fatalf("adopted credential missing: %v", err)
	}
	if cred.SecretKeyCipher == "plainsecret" {
		t.Fatal("secret stored in plaintext")
	}
	got, err := cipher.Decrypt(cred.SecretKeyCipher)
	if err != nil || got != "plainsecret" {
		t.Fatalf("decrypt adopted secret = %q, %v", got, err)
	}

	// Pending import is cleared after confirm.
	if _, ok := m.PendingSummary(nodeID); ok {
		t.Fatal("pending import should be cleared after confirm")
	}
}

// TestImportConfirmRefusesAlreadyManaged verifies a node that already has panel
// config cannot be re-adopted (guards against clobbering).
func TestImportConfirmRefusesAlreadyManaged(t *testing.T) {
	m, _ := newTestMigration(t)
	const nodeID = 1

	// Pre-existing managed credential.
	if err := m.db.Create(&NodeCredential{NodeID: nodeID, AccessKey: "EXISTING", SecretKeyCipher: "v1:x", Status: "enabled"}).Error; err != nil {
		t.Fatalf("seed managed cred: %v", err)
	}
	if err := m.ingestReport(nodeID, sampleReport()); err != nil {
		t.Fatalf("ingestReport: %v", err)
	}
	_, _, err := m.Confirm(nodeID, "admin")
	if err == nil {
		t.Fatal("expected refusal for already-managed node")
	}
}

// TestImportAbortDiscards verifies abort clears the pending import without writing.
func TestImportAbortDiscards(t *testing.T) {
	m, _ := newTestMigration(t)
	const nodeID = 1
	if err := m.ingestReport(nodeID, sampleReport()); err != nil {
		t.Fatalf("ingestReport: %v", err)
	}
	if err := m.Abort(nodeID, "admin"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, ok := m.PendingSummary(nodeID); ok {
		t.Fatal("pending import should be gone after abort")
	}
	if err := m.Abort(nodeID, "admin"); err != ErrNoPendingImport {
		t.Fatalf("second abort = %v, want ErrNoPendingImport", err)
	}
}
