package panel

import (
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	"github.com/RSJWY/NativeS3-Bridge/pkg/managedconfig"
)

const desiredSnapshotSchemaVersion = 1

var (
	// ErrDesiredSnapshotRepublishRequired is returned for the legacy masked
	// controlproto JSON format. Its missing credential secrets make the original
	// version impossible to reconstruct exactly, so pushes must fail closed.
	ErrDesiredSnapshotRepublishRequired = errors.New("published desired snapshot must be republished")
	ErrDesiredSnapshotHashMismatch      = errors.New("published desired snapshot hash mismatch")
)

type persistedDesiredSnapshot struct {
	SchemaVersion int                            `json:"schema_version"`
	Credentials   []persistedDesiredCredential   `json:"credentials"`
	Buckets       []controlproto.DesiredBucket   `json:"buckets"`
	Webhooks      []controlproto.DesiredWebhook  `json:"webhooks"`
	RateLimit     *controlproto.DesiredRateLimit `json:"rate_limit,omitempty"`
}

type persistedDesiredCredential struct {
	AccessKey       string `json:"access_key"`
	SecretKeyCipher string `json:"secret_key_cipher"`
	Name            string `json:"name,omitempty"`
	Bucket          string `json:"bucket,omitempty"`
	Status          string `json:"status"`
	QuotaBytes      int64  `json:"quota_bytes"`
}

// DesiredStateAuthority owns both sides of the draft -> published boundary.
// Draft rows are read only during an explicit publish. Every push is rebuilt
// solely from the encrypted, schema-versioned DesiredConfig snapshot.
type DesiredStateAuthority struct {
	db     *gorm.DB
	cipher *SecretCipher
}

func NewDesiredStateAuthority(db *gorm.DB, cipher *SecretCipher) *DesiredStateAuthority {
	return &DesiredStateAuthority{db: db, cipher: cipher}
}

// Build materializes the current editable draft. It is intentionally not used
// by any push path.
func (a *DesiredStateAuthority) Build(nodeID uint) (controlproto.DesiredState, error) {
	snapshot, err := a.loadDraftSnapshot(a.db, nodeID)
	if err != nil {
		return controlproto.DesiredState{}, err
	}
	state, err := a.decryptSnapshot(snapshot)
	if err != nil {
		return controlproto.DesiredState{}, err
	}
	if err := managedconfig.ValidateDesiredState(state); err != nil {
		return controlproto.DesiredState{}, fmt.Errorf("validate desired draft: %w", err)
	}
	return state, nil
}

func (a *DesiredStateAuthority) loadDraftSnapshot(db *gorm.DB, nodeID uint) (persistedDesiredSnapshot, error) {
	snapshot := persistedDesiredSnapshot{
		SchemaVersion: desiredSnapshotSchemaVersion,
		Credentials:   []persistedDesiredCredential{},
		Buckets:       []controlproto.DesiredBucket{},
		Webhooks:      []controlproto.DesiredWebhook{},
	}

	var credentials []NodeCredential
	if err := db.Where("node_id = ?", nodeID).Order("access_key ASC").Find(&credentials).Error; err != nil {
		return snapshot, err
	}
	for _, credential := range credentials {
		status := managedconfig.NormalizeCredentialStatus(credential.Status)
		snapshot.Credentials = append(snapshot.Credentials, persistedDesiredCredential{
			AccessKey:       credential.AccessKey,
			SecretKeyCipher: credential.SecretKeyCipher,
			Name:            credential.Name,
			Bucket:          credential.Bucket,
			Status:          status,
			QuotaBytes:      credential.QuotaBytes,
		})
	}

	var buckets []NodeBucket
	if err := db.Where("node_id = ?", nodeID).Order("name ASC").Find(&buckets).Error; err != nil {
		return snapshot, err
	}
	for _, bucket := range buckets {
		acl := bucket.ACL
		if acl == "" {
			acl = "private"
		}
		snapshot.Buckets = append(snapshot.Buckets, controlproto.DesiredBucket{Name: bucket.Name, ACL: acl})
	}

	var webhooks []NodeWebhook
	if err := db.Where("node_id = ?", nodeID).Order("url ASC, events ASC, id ASC").Find(&webhooks).Error; err != nil {
		return snapshot, err
	}
	for _, webhook := range webhooks {
		snapshot.Webhooks = append(snapshot.Webhooks, controlproto.DesiredWebhook{
			URL: webhook.URL, Events: webhook.Events, Enabled: webhook.Enabled,
		})
	}

	var limit NodeRateLimit
	if err := db.Where("node_id = ?", nodeID).First(&limit).Error; err == nil {
		snapshot.RateLimit = &controlproto.DesiredRateLimit{
			AnonymousRPS: limit.AnonymousRPS, AnonymousBurst: limit.AnonymousBurst, TrustForwarded: limit.TrustForwarded,
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return snapshot, err
	}
	return snapshot, nil
}

func (a *DesiredStateAuthority) decryptSnapshot(snapshot persistedDesiredSnapshot) (controlproto.DesiredState, error) {
	if a.cipher == nil {
		return controlproto.DesiredState{}, ErrMasterKeyMissing
	}
	state := controlproto.DesiredState{
		Credentials: make([]controlproto.DesiredCredential, 0, len(snapshot.Credentials)),
		Buckets:     append([]controlproto.DesiredBucket(nil), snapshot.Buckets...),
		Webhooks:    append([]controlproto.DesiredWebhook(nil), snapshot.Webhooks...),
		RateLimit:   snapshot.RateLimit,
	}
	for _, credential := range snapshot.Credentials {
		if credential.SecretKeyCipher == "" {
			return controlproto.DesiredState{}, fmt.Errorf("decrypt secret for %q: ciphertext is empty", credential.AccessKey)
		}
		secret, err := a.cipher.Decrypt(credential.SecretKeyCipher)
		if err != nil {
			return controlproto.DesiredState{}, fmt.Errorf("decrypt secret for %q: %w", credential.AccessKey, err)
		}
		state.Credentials = append(state.Credentials, controlproto.DesiredCredential{
			AccessKey: credential.AccessKey, SecretKey: secret, Name: credential.Name,
			Bucket: credential.Bucket, Status: managedconfig.NormalizeCredentialStatus(credential.Status), QuotaBytes: credential.QuotaBytes,
		})
	}
	return state, nil
}

func encodePersistedDesiredSnapshot(snapshot persistedDesiredSnapshot) ([]byte, error) {
	return json.Marshal(snapshot)
}

func decodePersistedDesiredSnapshot(raw string) (persistedDesiredSnapshot, error) {
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal([]byte(raw), &header); err != nil {
		return persistedDesiredSnapshot{}, fmt.Errorf("decode published desired snapshot: %w", err)
	}
	if header.SchemaVersion != desiredSnapshotSchemaVersion {
		return persistedDesiredSnapshot{}, ErrDesiredSnapshotRepublishRequired
	}
	var snapshot persistedDesiredSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return persistedDesiredSnapshot{}, fmt.Errorf("decode published desired snapshot: %w", err)
	}
	if snapshot.Credentials == nil {
		snapshot.Credentials = []persistedDesiredCredential{}
	}
	if snapshot.Buckets == nil {
		snapshot.Buckets = []controlproto.DesiredBucket{}
	}
	if snapshot.Webhooks == nil {
		snapshot.Webhooks = []controlproto.DesiredWebhook{}
	}
	return snapshot, nil
}

func (a *DesiredStateAuthority) Publish(nodeID uint, updatedBy string) (int64, string, error) {
	unlock := lockNodeDraft(nodeID)
	defer unlock()

	var version int64
	var hash string
	err := a.db.Transaction(func(tx *gorm.DB) error {
		var err error
		version, hash, err = a.PublishTx(tx, nodeID, updatedBy)
		return err
	})
	if err != nil {
		return 0, "", err
	}
	return version, hash, nil
}

// PublishTx creates the exact encrypted snapshot using the supplied transaction.
// Import confirmation uses this primitive so draft adoption and baseline publish
// either commit together or roll back together.
func (a *DesiredStateAuthority) PublishTx(tx *gorm.DB, nodeID uint, updatedBy string) (int64, string, error) {
	snapshot, err := a.loadDraftSnapshot(tx, nodeID)
	if err != nil {
		return 0, "", err
	}
	state, err := a.decryptSnapshot(snapshot)
	if err != nil {
		return 0, "", err
	}
	if err := managedconfig.ValidateDesiredState(state); err != nil {
		return 0, "", fmt.Errorf("validate desired draft: %w", err)
	}
	encoded, err := encodePersistedDesiredSnapshot(snapshot)
	if err != nil {
		return 0, "", fmt.Errorf("encode desired snapshot: %w", err)
	}
	hash := state.ContentHash()

	var existing DesiredConfig
	err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("node_id = ?", nodeID).First(&existing).Error
	if err == nil {
		version := existing.Version + 1
		if err := tx.Model(&DesiredConfig{}).Where("node_id = ?", nodeID).Updates(map[string]any{
			"version": version, "content_json": string(encoded), "content_hash": hash,
			"updated_by": updatedBy, "updated_at": nowUTC(),
		}).Error; err != nil {
			return 0, "", err
		}
		return version, hash, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, "", err
	}
	row := DesiredConfig{
		NodeID: nodeID, Version: 1, ContentJSON: string(encoded), ContentHash: hash,
		UpdatedBy: updatedBy, UpdatedAt: nowUTC(),
	}
	if err := tx.Create(&row).Error; err != nil {
		return 0, "", err
	}
	return 1, hash, nil
}

// BuildPushable decrypts only the persisted published snapshot and verifies its
// plaintext-derived hash. It never reads any editable draft table.
func (a *DesiredStateAuthority) BuildPushable(nodeID uint) (controlproto.DesiredStatePayload, error) {
	var config DesiredConfig
	if err := a.db.Where("node_id = ?", nodeID).First(&config).Error; err != nil {
		return controlproto.DesiredStatePayload{}, err
	}
	snapshot, err := decodePersistedDesiredSnapshot(config.ContentJSON)
	if err != nil {
		return controlproto.DesiredStatePayload{}, err
	}
	state, err := a.decryptSnapshot(snapshot)
	if err != nil {
		return controlproto.DesiredStatePayload{}, err
	}
	if err := managedconfig.ValidateDesiredState(state); err != nil {
		return controlproto.DesiredStatePayload{}, fmt.Errorf("validate published desired snapshot: %w", err)
	}
	hash := state.ContentHash()
	if hash != config.ContentHash {
		return controlproto.DesiredStatePayload{}, fmt.Errorf("%w: stored=%s computed=%s", ErrDesiredSnapshotHashMismatch, config.ContentHash, hash)
	}
	return controlproto.DesiredStatePayload{Version: config.Version, ContentHash: hash, Content: state}, nil
}

// DraftStatus reports whether current draft rows differ from the latest exact
// published snapshot and whether the stored snapshot is legacy/unpushable.
func (a *DesiredStateAuthority) DraftStatus(nodeID uint) (dirty bool, publishRequired bool, err error) {
	draft, err := a.loadDraftSnapshot(a.db, nodeID)
	if err != nil {
		return false, false, err
	}
	var config DesiredConfig
	if err := a.db.Where("node_id = ?", nodeID).First(&config).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Absence of a published snapshot is itself an unpublished state,
			// including when the intended authoritative configuration is empty.
			// This keeps the first explicit publish available so an administrator
			// can intentionally establish (or clear to) an empty baseline.
			return true, false, nil
		}
		return false, false, err
	}
	published, err := decodePersistedDesiredSnapshot(config.ContentJSON)
	if errors.Is(err, ErrDesiredSnapshotRepublishRequired) {
		return false, true, nil
	}
	if err != nil {
		return false, true, nil
	}
	draftJSON, err := encodePersistedDesiredSnapshot(draft)
	if err != nil {
		return false, false, err
	}
	publishedJSON, err := encodePersistedDesiredSnapshot(published)
	if err != nil {
		return false, false, err
	}
	return string(draftJSON) != string(publishedJSON), false, nil
}
