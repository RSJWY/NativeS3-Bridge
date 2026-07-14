package panel

import (
	"encoding/json"
	"fmt"

	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

// DesiredStateAuthority owns the panel's authoritative desired state for each
// node. It builds a controlproto.DesiredState from the panel's own tables
// (node_credentials, and node-scoped buckets/webhooks/limits), decrypting secret
// keys with the master-key cipher only at build time, and publishes new
// monotonic versions. Only the latest version is retained (design §2.2): a new
// publish overwrites the single desired_configs row for the node.
type DesiredStateAuthority struct {
	db     *gorm.DB
	cipher *SecretCipher
}

// NewDesiredStateAuthority builds the authority over the panel DB and cipher.
func NewDesiredStateAuthority(db *gorm.DB, cipher *SecretCipher) *DesiredStateAuthority {
	return &DesiredStateAuthority{db: db, cipher: cipher}
}

// Build assembles the desired state for a node from panel tables. Secret keys
// are decrypted here (and only here) so the plaintext exists transiently in the
// state that is pushed over mTLS; it is never persisted in plaintext on the
// panel. A decryption failure aborts the build (fail-closed): the panel will not
// push a desired state with missing/garbled secrets.
func (a *DesiredStateAuthority) Build(nodeID uint) (controlproto.DesiredState, error) {
	var state controlproto.DesiredState

	var creds []NodeCredential
	if err := a.db.Where("node_id = ?", nodeID).Order("access_key ASC").Find(&creds).Error; err != nil {
		return state, err
	}
	for _, c := range creds {
		secret, err := a.cipher.Decrypt(c.SecretKeyCipher)
		if err != nil {
			return state, fmt.Errorf("decrypt secret for %q: %w", c.AccessKey, err)
		}
		status := c.Status
		if status == "" {
			status = "enabled"
		}
		state.Credentials = append(state.Credentials, controlproto.DesiredCredential{
			AccessKey:  c.AccessKey,
			SecretKey:  secret,
			Name:       c.Name,
			Bucket:     c.Bucket,
			Status:     status,
			QuotaBytes: c.QuotaBytes,
		})
	}

	var buckets []NodeBucket
	if err := a.db.Where("node_id = ?", nodeID).Order("name ASC").Find(&buckets).Error; err != nil {
		return state, err
	}
	for _, b := range buckets {
		acl := b.ACL
		if acl == "" {
			acl = "private"
		}
		state.Buckets = append(state.Buckets, controlproto.DesiredBucket{Name: b.Name, ACL: acl})
	}

	var webhooks []NodeWebhook
	if err := a.db.Where("node_id = ?", nodeID).Order("url ASC").Find(&webhooks).Error; err != nil {
		return state, err
	}
	for _, h := range webhooks {
		state.Webhooks = append(state.Webhooks, controlproto.DesiredWebhook{URL: h.URL, Events: h.Events, Enabled: h.Enabled})
	}

	var limit NodeRateLimit
	err := a.db.Where("node_id = ?", nodeID).First(&limit).Error
	if err == nil {
		state.RateLimit = &controlproto.DesiredRateLimit{
			AnonymousRPS:   limit.AnonymousRPS,
			AnonymousBurst: limit.AnonymousBurst,
			TrustForwarded: limit.TrustForwarded,
		}
	} else if err != gorm.ErrRecordNotFound {
		return state, err
	}

	return state, nil
}

// Publish rebuilds the desired state for a node, bumps its version, and writes
// the single latest desired_configs row (upsert). It returns the new version and
// the content hash. The plaintext secrets in the built state are NOT persisted:
// only the content JSON (which does contain plaintext secrets by necessity, so
// the desired_configs row is as sensitive as the push) — see note below.
//
// NOTE: the stored ContentJSON includes plaintext secret keys because the node
// needs them for SigV4. This row is written to the panel DB. To keep the master
// key meaningful we store the content WITHOUT plaintext secrets and re-decrypt on
// push; see PublishMasked.
func (a *DesiredStateAuthority) Publish(nodeID uint, updatedBy string) (int64, string, error) {
	state, err := a.Build(nodeID)
	if err != nil {
		return 0, "", err
	}
	// Persist a masked copy (no plaintext secrets) so the panel DB alone never
	// yields plaintext S3 secrets (design §2.3, §7.3). The hash is computed over
	// the REAL state (with secrets) so the node's applied hash matches.
	realHash := state.ContentHash()
	masked := maskSecrets(state)
	maskedJSON, err := json.Marshal(masked)
	if err != nil {
		return 0, "", fmt.Errorf("marshal desired state: %w", err)
	}

	var version int64
	err = a.db.Transaction(func(tx *gorm.DB) error {
		var existing DesiredConfig
		err := tx.Where("node_id = ?", nodeID).First(&existing).Error
		if err == nil {
			version = existing.Version + 1
			return tx.Model(&DesiredConfig{}).Where("node_id = ?", nodeID).Updates(map[string]any{
				"version":      version,
				"content_json": string(maskedJSON),
				"content_hash": realHash,
				"updated_by":   updatedBy,
				"updated_at":   nowUTC(),
			}).Error
		}
		if err != gorm.ErrRecordNotFound {
			return err
		}
		version = 1
		return tx.Create(&DesiredConfig{
			NodeID:      nodeID,
			Version:     version,
			ContentJSON: string(maskedJSON),
			ContentHash: realHash,
			UpdatedBy:   updatedBy,
			UpdatedAt:   nowUTC(),
		}).Error
	})
	if err != nil {
		return 0, "", err
	}
	return version, realHash, nil
}

// BuildPushable returns the desired state WITH plaintext secrets and the current
// version/hash for pushing to a node. It is used by the transport push path so
// the node receives usable secrets while the panel DB retains only masked copies.
func (a *DesiredStateAuthority) BuildPushable(nodeID uint) (controlproto.DesiredStatePayload, error) {
	var cfg DesiredConfig
	if err := a.db.Where("node_id = ?", nodeID).First(&cfg).Error; err != nil {
		return controlproto.DesiredStatePayload{}, err
	}
	state, err := a.Build(nodeID)
	if err != nil {
		return controlproto.DesiredStatePayload{}, err
	}
	return controlproto.DesiredStatePayload{
		Version:     cfg.Version,
		ContentHash: cfg.ContentHash,
		Content:     state,
	}, nil
}

// maskSecrets returns a copy of state with credential secret keys blanked, for
// safe persistence in the panel DB. The content hash must be computed on the
// UNMASKED state so the node (which applies real secrets) computes the same hash.
func maskSecrets(state controlproto.DesiredState) controlproto.DesiredState {
	out := state
	out.Credentials = make([]controlproto.DesiredCredential, len(state.Credentials))
	copy(out.Credentials, state.Credentials)
	for i := range out.Credentials {
		out.Credentials[i].SecretKey = ""
	}
	return out
}

// nodeConfigVersion returns the current desired version for a node, or 0 if none.
func nodeConfigVersion(db *gorm.DB, nodeID uint) int64 {
	var cfg DesiredConfig
	if err := db.Where("node_id = ?", nodeID).First(&cfg).Error; err != nil {
		return 0
	}
	return cfg.Version
}
