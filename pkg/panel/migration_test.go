package panel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	"gorm.io/gorm"
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

func TestImportConfirmRefusesAnyManagedResource(t *testing.T) {
	cases := []struct {
		name string
		seed func(*MigrationCoordinator) error
	}{
		{name: "bucket", seed: func(m *MigrationCoordinator) error {
			return m.db.Create(&NodeBucket{NodeID: 1, Name: "managed-bucket", ACL: "private"}).Error
		}},
		{name: "webhook", seed: func(m *MigrationCoordinator) error {
			return m.db.Create(&NodeWebhook{NodeID: 1, URL: "https://hooks.example.test", Events: "ObjectCreated", Enabled: true}).Error
		}},
		{name: "rate limit", seed: func(m *MigrationCoordinator) error {
			return m.db.Create(&NodeRateLimit{NodeID: 1, AnonymousRPS: 1, AnonymousBurst: 1}).Error
		}},
		{name: "desired config", seed: func(m *MigrationCoordinator) error {
			return m.db.Create(&DesiredConfig{NodeID: 1, Version: 1, ContentJSON: `{}`, ContentHash: "hash"}).Error
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestMigration(t)
			if err := tc.seed(m); err != nil {
				t.Fatal(err)
			}
			if err := m.ingestReport(1, sampleReport()); err != nil {
				t.Fatal(err)
			}
			if _, _, err := m.Confirm(1, "admin"); !errors.Is(err, ErrAlreadyManaged) {
				t.Fatalf("Confirm error = %v", err)
			}
		})
	}
}

func TestImportRequestRejectsManagedNodeBeforeOnlineCheck(t *testing.T) {
	m, _ := newTestMigration(t)
	if err := m.db.Create(&NodeBucket{NodeID: 1, Name: "managed-bucket", ACL: "private"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := m.RequestImport(context.Background(), NewHub(), 1); !errors.Is(err, ErrAlreadyManaged) {
		t.Fatalf("RequestImport error = %v", err)
	}
}

func TestImportConfirmRollsBackAdoptionWhenPublishFails(t *testing.T) {
	m, _ := newTestMigration(t)
	if err := m.ingestReport(1, sampleReport()); err != nil {
		t.Fatal(err)
	}
	const callbackName = "test:fail_desired_snapshot_create"
	if err := m.db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "desired_configs" {
			tx.AddError(errors.New("injected desired snapshot failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer m.db.Callback().Create().Remove(callbackName)

	if _, _, err := m.Confirm(1, "admin"); err == nil {
		t.Fatal("Confirm unexpectedly succeeded")
	}
	for _, model := range []any{&NodeCredential{}, &NodeBucket{}, &NodeWebhook{}, &NodeRateLimit{}, &DesiredConfig{}} {
		var count int64
		if err := m.db.Model(model).Where("node_id = ?", 1).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("rollback left %T rows: %d", model, count)
		}
	}
	if _, ok := m.PendingSummary(1); !ok {
		t.Fatal("failed confirmation cleared pending import")
	}
}

func sampleReport() controlproto.ImportReportPayload {
	state := controlproto.DesiredState{
		Credentials: []controlproto.DesiredCredential{
			{AccessKey: "AKIMPORT", SecretKey: "plainsecret", Status: "enabled", QuotaBytes: 100},
		},
		Buckets:  []controlproto.DesiredBucket{{Name: "bucket-one", ACL: "private"}},
		Webhooks: []controlproto.DesiredWebhook{{URL: "https://hooks.example.test/import", Events: "ObjectCreated", Enabled: false}},
	}
	return controlproto.ImportReportPayload{
		State:            state,
		CredentialCount:  1,
		BucketCount:      1,
		WebhookCount:     1,
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
	if hash != sampleReport().LocalContentHash {
		t.Fatalf("published hash = %q, want imported hash %q", hash, sampleReport().LocalContentHash)
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
	var desired DesiredConfig
	if err := m.db.Where("node_id = ?", nodeID).First(&desired).Error; err != nil {
		t.Fatalf("published baseline missing: %v", err)
	}
	if strings.Contains(desired.ContentJSON, "plainsecret") {
		t.Fatal("published baseline leaked plaintext secret")
	}
	var webhook NodeWebhook
	if err := m.db.Where("node_id = ?", nodeID).First(&webhook).Error; err != nil {
		t.Fatalf("adopted webhook missing: %v", err)
	}
	if webhook.Enabled {
		t.Fatal("disabled imported webhook was enabled by database default")
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

func TestImportLifecycleRejectsConcurrentOperationsAndPendingOverwrite(t *testing.T) {
	m, _ := newTestMigration(t)
	const nodeID = 1

	m.mu.Lock()
	m.waiters[nodeID] = make(chan *pendingImport, 1)
	m.mu.Unlock()
	if _, _, err := m.Confirm(nodeID, "admin"); !errors.Is(err, ErrImportInProgress) {
		t.Fatalf("Confirm while request is waiting = %v", err)
	}
	if err := m.Abort(nodeID, "admin"); !errors.Is(err, ErrImportInProgress) {
		t.Fatalf("Abort while request is waiting = %v", err)
	}
	m.mu.Lock()
	delete(m.waiters, nodeID)
	m.mu.Unlock()

	first := sampleReport()
	if err := m.ingestReport(nodeID, first); err != nil {
		t.Fatal(err)
	}
	second := sampleReport()
	second.State.Credentials[0].AccessKey = "AKREPLACEMENT"
	second.LocalContentHash = second.State.ContentHash()
	if err := m.ingestReport(nodeID, second); !errors.Is(err, ErrImportPending) {
		t.Fatalf("second report = %v, want ErrImportPending", err)
	}
	summary, ok := m.PendingSummary(nodeID)
	if !ok || len(summary.AccessKeys) != 1 || summary.AccessKeys[0] != "AKIMPORT" {
		t.Fatalf("pending summary was overwritten: %+v ok=%v", summary, ok)
	}

	m.mu.Lock()
	m.confirming[nodeID] = true
	m.mu.Unlock()
	if err := m.Abort(nodeID, "admin"); !errors.Is(err, ErrImportInProgress) {
		t.Fatalf("Abort while confirm is running = %v", err)
	}
	m.mu.Lock()
	delete(m.confirming, nodeID)
	m.mu.Unlock()
}
