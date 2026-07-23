package panel

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	"github.com/RSJWY/NativeS3-Bridge/pkg/managedconfig"
)

// In-place migration adopts an existing single-node deployment under the panel
// (design §8.3). The flow is strictly read-then-confirm:
//
//  1. Admin triggers an import for an online node.
//  2. Panel sends import_request; the node replies import_report with its current
//     local business config (read-only; the node is never mutated by this).
//  3. Panel encrypts the reported secret keys immediately and holds the import as
//     PENDING (nothing is written to the node's authoritative panel tables yet).
//  4. Admin reviews the summary and confirms; only then does the panel adopt the
//     config into node_credentials/node_buckets/... and publish version=1.
//  5. Aborting discards the pending import; the node keeps serving S3 unchanged.
//
// The red line: before the admin confirms, the panel must not write any business
// config to the node. Import is read-only until confirmation, and confirmation
// only writes to the PANEL's own tables — the panel never pushes to the node
// until the admin later publishes/pushes desired state.

// ErrNoPendingImport is returned when confirming/aborting an import that does not
// exist (never requested, already confirmed, or already aborted).
var ErrNoPendingImport = errors.New("no pending import for node")

// ErrImportTimeout is returned when the node does not report within the wait.
var ErrImportTimeout = errors.New("timed out waiting for node import report")

// ErrAlreadyManaged is returned when confirming an import for a node that
// already has panel-side business config. Adoption only applies to fresh nodes;
// re-adopting would clobber managed state, so it is refused.
var ErrAlreadyManaged = errors.New("node already has managed config; refusing to overwrite")

var (
	ErrImportPending    = errors.New("node already has a pending import")
	ErrImportInProgress = errors.New("node import operation is already in progress")
)

// pendingImport is an encrypted, not-yet-adopted snapshot of a node's reported
// local config. Secrets are stored as ciphertext from the moment of receipt so
// plaintext never rests in panel memory beyond the encryption call.
type pendingImport struct {
	nodeID      uint
	credentials []pendingCredential
	buckets     []controlproto.DesiredBucket
	webhooks    []controlproto.DesiredWebhook
	rateLimit   *controlproto.DesiredRateLimit
	contentHash string
}

type pendingCredential struct {
	accessKey       string
	secretKeyCipher string
	name            string
	bucket          string
	status          string
	quotaBytes      int64
}

// ImportSummary is the admin-facing summary of a pending import. It intentionally
// carries only counts and non-sensitive identifiers — never secret keys.
type ImportSummary struct {
	NodeID              uint     `json:"node_id"`
	CredentialCount     int      `json:"credential_count"`
	BucketCount         int      `json:"bucket_count"`
	WebhookCount        int      `json:"webhook_count"`
	AccessKeys          []string `json:"access_keys"`
	BucketNames         []string `json:"bucket_names"`
	ContentHash         string   `json:"content_hash"`
	RateLimitConfigured bool     `json:"rate_limit_configured"`
}

// MigrationCoordinator drives the import lifecycle. It holds pending imports in
// memory (they are cheap to re-request and must not persist plaintext), and
// commits confirmed imports into the panel's authoritative tables.
type MigrationCoordinator struct {
	db      *gorm.DB
	cipher  *SecretCipher
	desired *DesiredStateAuthority
	audit   *Auditor

	mu         sync.Mutex
	pending    map[uint]*pendingImport
	waiters    map[uint]chan *pendingImport
	confirming map[uint]bool
}

// NewMigrationCoordinator builds the coordinator over its collaborators.
func NewMigrationCoordinator(db *gorm.DB, cipher *SecretCipher, desired *DesiredStateAuthority, audit *Auditor) *MigrationCoordinator {
	return &MigrationCoordinator{
		db:         db,
		cipher:     cipher,
		desired:    desired,
		audit:      audit,
		pending:    make(map[uint]*pendingImport),
		waiters:    make(map[uint]chan *pendingImport),
		confirming: make(map[uint]bool),
	}
}

// ingestReport is called from the transport layer when a node sends its
// import_report. It encrypts the reported secrets immediately and records the
// pending import, waking any admin request waiting on the report.
func (m *MigrationCoordinator) ingestReport(nodeID uint, report controlproto.ImportReportPayload) error {
	if m.cipher == nil {
		return ErrMasterKeyMissing
	}
	if err := managedconfig.ValidateDesiredState(report.State); err != nil {
		return fmt.Errorf("validate imported state: %w", err)
	}
	computedHash := report.State.ContentHash()
	if report.LocalContentHash == "" || report.LocalContentHash != computedHash {
		return fmt.Errorf("imported state hash mismatch")
	}
	pi := &pendingImport{
		nodeID:      nodeID,
		buckets:     report.State.Buckets,
		webhooks:    report.State.Webhooks,
		rateLimit:   report.State.RateLimit,
		contentHash: report.LocalContentHash,
	}
	for _, c := range report.State.Credentials {
		// Encrypt the reported plaintext secret immediately; plaintext is not
		// retained past this call (design §2.3 — panel stores only ciphertext).
		ciphertext, err := m.cipher.Encrypt(c.SecretKey)
		if err != nil {
			return fmt.Errorf("encrypt imported secret for %q: %w", c.AccessKey, err)
		}
		status := c.Status
		if status == "" {
			status = "enabled"
		}
		pi.credentials = append(pi.credentials, pendingCredential{
			accessKey:       c.AccessKey,
			secretKeyCipher: ciphertext,
			name:            c.Name,
			bucket:          c.Bucket,
			status:          status,
			quotaBytes:      c.QuotaBytes,
		})
	}

	m.mu.Lock()
	if m.confirming[nodeID] {
		m.mu.Unlock()
		return ErrImportInProgress
	}
	if _, exists := m.pending[nodeID]; exists {
		m.mu.Unlock()
		return ErrImportPending
	}
	m.pending[nodeID] = pi
	waiter := m.waiters[nodeID]
	delete(m.waiters, nodeID)
	m.mu.Unlock()

	if waiter != nil {
		// Non-blocking: the waiter buffers one value.
		select {
		case waiter <- pi:
		default:
		}
	}
	return nil
}

// RequestImport sends an import_request to an online node and waits for the
// import_report to arrive (or ctx/timeout). It returns the admin-facing summary.
// It writes nothing to the node and nothing to the panel's authoritative tables:
// the result is a PENDING import awaiting confirmation.
func (m *MigrationCoordinator) RequestImport(ctx context.Context, hub *Hub, nodeID uint) (ImportSummary, error) {
	managed, err := nodeAlreadyManaged(m.db, nodeID)
	if err != nil {
		return ImportSummary{}, err
	}
	if managed {
		return ImportSummary{}, ErrAlreadyManaged
	}
	conn, ok := hub.Get(nodeID)
	if !ok {
		return ImportSummary{}, ErrNodeOffline
	}

	waiter := make(chan *pendingImport, 1)
	m.mu.Lock()
	if _, exists := m.pending[nodeID]; exists {
		m.mu.Unlock()
		return ImportSummary{}, ErrImportPending
	}
	if _, exists := m.waiters[nodeID]; exists {
		m.mu.Unlock()
		return ImportSummary{}, ErrImportInProgress
	}
	m.waiters[nodeID] = waiter
	m.mu.Unlock()

	if err := conn.sendMessage(ctx, controlproto.TypeImportRequest, "", nil); err != nil {
		m.mu.Lock()
		delete(m.waiters, nodeID)
		m.mu.Unlock()
		return ImportSummary{}, fmt.Errorf("send import request: %w", err)
	}

	select {
	case <-ctx.Done():
		m.mu.Lock()
		delete(m.waiters, nodeID)
		m.mu.Unlock()
		return ImportSummary{}, ErrImportTimeout
	case pi := <-waiter:
		if m.audit != nil {
			m.audit.Write(AuditEntry{Action: "import_report", TargetNode: nodeID, Result: "received", Source: "control-plane"})
		}
		return summarize(pi), nil
	}
}

// PendingSummary returns the summary of a node's pending import, if any.
func (m *MigrationCoordinator) PendingSummary(nodeID uint) (ImportSummary, bool) {
	m.mu.Lock()
	pi, ok := m.pending[nodeID]
	m.mu.Unlock()
	if !ok {
		return ImportSummary{}, false
	}
	return summarize(pi), true
}

// Confirm adopts a pending import into the panel's authoritative tables and
// publishes the version=1 desired-state baseline. It is the ONLY step that
// writes business config, and it writes only to panel tables — never to the
// node. After confirmation the panel is authoritative; the admin must
// publish/push desired state for the node to reconcile against it.
func (m *MigrationCoordinator) Confirm(nodeID uint, adminIdentity string) (int64, string, error) {
	unlock := lockNodeDraft(nodeID)
	defer unlock()

	m.mu.Lock()
	if m.confirming[nodeID] {
		m.mu.Unlock()
		return 0, "", ErrImportInProgress
	}
	if _, waiting := m.waiters[nodeID]; waiting {
		m.mu.Unlock()
		return 0, "", ErrImportInProgress
	}
	pi, ok := m.pending[nodeID]
	if ok {
		m.confirming[nodeID] = true
	}
	m.mu.Unlock()
	if !ok {
		return 0, "", ErrNoPendingImport
	}
	defer func() {
		m.mu.Lock()
		delete(m.confirming, nodeID)
		m.mu.Unlock()
	}()

	var version int64
	var hash string
	err := m.db.Transaction(func(tx *gorm.DB) error {
		// Adopt only if the node has no panel-side config yet (fresh adoption). This
		// guards against clobbering an already-managed node.
		managed, err := nodeAlreadyManaged(tx, nodeID)
		if err != nil {
			return err
		}
		if managed {
			return ErrAlreadyManaged
		}
		for _, c := range pi.credentials {
			cred := NodeCredential{
				NodeID:          nodeID,
				AccessKey:       c.accessKey,
				SecretKeyCipher: c.secretKeyCipher,
				Name:            c.name,
				Bucket:          c.bucket,
				Status:          c.status,
				QuotaBytes:      c.quotaBytes,
			}
			if err := tx.Create(&cred).Error; err != nil {
				return err
			}
		}
		for _, b := range pi.buckets {
			acl := b.ACL
			if acl == "" {
				acl = "private"
			}
			if err := tx.Create(&NodeBucket{NodeID: nodeID, Name: b.Name, ACL: acl}).Error; err != nil {
				return err
			}
		}
		for _, h := range pi.webhooks {
			webhook := NodeWebhook{NodeID: nodeID, URL: h.URL, Events: h.Events, Enabled: h.Enabled}
			if err := insertNodeWebhook(tx, &webhook); err != nil {
				return err
			}
		}
		if pi.rateLimit != nil {
			if err := tx.Create(&NodeRateLimit{
				NodeID:         nodeID,
				AnonymousRPS:   pi.rateLimit.AnonymousRPS,
				AnonymousBurst: pi.rateLimit.AnonymousBurst,
				TrustForwarded: pi.rateLimit.TrustForwarded,
			}).Error; err != nil {
				return err
			}
		}
		version, hash, err = m.desired.PublishTx(tx, nodeID, adminIdentity)
		return err
	})
	if err != nil {
		return 0, "", fmt.Errorf("adopt imported config: %w", err)
	}

	m.mu.Lock()
	if m.pending[nodeID] == pi {
		delete(m.pending, nodeID)
	}
	m.mu.Unlock()

	if m.audit != nil {
		m.audit.Write(AuditEntry{Action: "import_confirm", TargetNode: nodeID, Result: "v" + fmt.Sprint(version), Source: adminIdentity})
	}
	return version, hash, nil
}

// Abort discards a node's pending import without writing anything. The node
// keeps serving S3 from its own local DB unchanged (the migration can be retried
// later, or the adoption abandoned entirely).
func (m *MigrationCoordinator) Abort(nodeID uint, adminIdentity string) error {
	m.mu.Lock()
	if m.confirming[nodeID] {
		m.mu.Unlock()
		return ErrImportInProgress
	}
	if _, waiting := m.waiters[nodeID]; waiting {
		m.mu.Unlock()
		return ErrImportInProgress
	}
	_, ok := m.pending[nodeID]
	delete(m.pending, nodeID)
	m.mu.Unlock()
	if !ok {
		return ErrNoPendingImport
	}
	if m.audit != nil {
		m.audit.Write(AuditEntry{Action: "import_abort", TargetNode: nodeID, Result: "aborted", Source: adminIdentity})
	}
	return nil
}

func summarize(pi *pendingImport) ImportSummary {
	s := ImportSummary{
		NodeID:              pi.nodeID,
		CredentialCount:     len(pi.credentials),
		BucketCount:         len(pi.buckets),
		WebhookCount:        len(pi.webhooks),
		ContentHash:         pi.contentHash,
		RateLimitConfigured: pi.rateLimit != nil,
		AccessKeys:          []string{},
		BucketNames:         []string{},
	}
	for _, c := range pi.credentials {
		s.AccessKeys = append(s.AccessKeys, c.accessKey)
	}
	for _, b := range pi.buckets {
		s.BucketNames = append(s.BucketNames, b.Name)
	}
	return s
}

func nodeAlreadyManaged(db *gorm.DB, nodeID uint) (bool, error) {
	models := []any{&NodeCredential{}, &NodeBucket{}, &NodeWebhook{}, &NodeRateLimit{}, &DesiredConfig{}}
	for _, model := range models {
		var count int64
		if err := db.Model(model).Where("node_id = ?", nodeID).Count(&count).Error; err != nil {
			return false, err
		}
		if count > 0 {
			return true, nil
		}
	}
	return false, nil
}
