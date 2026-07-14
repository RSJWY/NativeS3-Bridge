package nodeagent

import (
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
)

// CredentialInvalidator lets the executor evict cached credentials whose secret
// or status changed as part of applying a desired state. The auth credential
// cache implements this; a nil invalidator is tolerated (no cache to flush).
type CredentialInvalidator interface {
	Invalidate(accessKey string)
}

// Executor applies panel-authoritative desired state to the node-local database
// and reads the current local state back for drift detection. Applying is
// transactional and idempotent: a failed apply rolls back and leaves the node's
// existing, already-serving configuration untouched (design §3, safety net for
// "apply failure must not break a node's usable config").
type Executor struct {
	db          *gorm.DB
	invalidator CredentialInvalidator
}

// NewExecutor builds an executor over the node-local DB. invalidator may be nil.
func NewExecutor(gdb *gorm.DB, invalidator CredentialInvalidator) *Executor {
	return &Executor{db: gdb, invalidator: invalidator}
}

// Apply reconciles the node-local DB to match the desired state within a single
// transaction. On success it returns the content hash of the state that is now
// persisted (recomputed from the DB, so it reflects exactly what was applied).
// On any error the transaction rolls back and the DB is unchanged.
func (e *Executor) Apply(state controlproto.DesiredState) (string, error) {
	// Collect access keys whose cache entries must be flushed after commit.
	var flushed []string

	err := e.db.Transaction(func(tx *gorm.DB) error {
		if err := applyBuckets(tx, state.Buckets); err != nil {
			return fmt.Errorf("apply buckets: %w", err)
		}
		keys, err := applyCredentials(tx, state.Credentials)
		if err != nil {
			return fmt.Errorf("apply credentials: %w", err)
		}
		flushed = keys
		if err := applyWebhooks(tx, state.Webhooks); err != nil {
			return fmt.Errorf("apply webhooks: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	// Cache invalidation happens only after a successful commit so a rolled-back
	// apply never disturbs the serving credential cache.
	if e.invalidator != nil {
		for _, key := range flushed {
			e.invalidator.Invalidate(key)
		}
	}

	return e.LocalContentHash()
}

// LocalContentHash reads the current node-local business config and returns the
// content hash computed with the SAME canonical algorithm the panel uses. The
// node reports this in hello so the panel can detect drift (local DB edited out
// of band) by comparing hashes at equal versions.
func (e *Executor) LocalContentHash() (string, error) {
	state, err := e.LocalState()
	if err != nil {
		return "", err
	}
	return state.ContentHash(), nil
}

// LocalState materializes the node-local business config as a controlproto
// DesiredState so it can be hashed/compared against the panel's desired state.
func (e *Executor) LocalState() (controlproto.DesiredState, error) {
	var state controlproto.DesiredState

	var creds []dbpkg.Credential
	if err := e.db.Order("access_key ASC").Find(&creds).Error; err != nil {
		return state, err
	}
	for _, c := range creds {
		state.Credentials = append(state.Credentials, controlproto.DesiredCredential{
			AccessKey:  c.AccessKey,
			SecretKey:  c.SecretKey,
			Name:       c.Name,
			Bucket:     c.Bucket,
			Status:     c.Status,
			QuotaBytes: c.QuotaBytes,
		})
	}

	var buckets []dbpkg.Bucket
	if err := e.db.Order("name ASC").Find(&buckets).Error; err != nil {
		return state, err
	}
	for _, b := range buckets {
		state.Buckets = append(state.Buckets, controlproto.DesiredBucket{Name: b.Name, ACL: b.ACL})
	}

	var hooks []dbpkg.HookConfig
	if err := e.db.Order("url ASC").Find(&hooks).Error; err != nil {
		return state, err
	}
	for _, h := range hooks {
		state.Webhooks = append(state.Webhooks, controlproto.DesiredWebhook{URL: h.URL, Events: h.Events, Enabled: h.Enabled})
	}

	return state, nil
}

// applyCredentials upserts desired credentials and deletes any node-local
// credential not present in the desired set. It returns the access keys that
// were touched (created/updated/deleted) so their caches can be flushed.
// UsedBytes is observed state owned by the node and is never overwritten here.
func applyCredentials(tx *gorm.DB, desired []controlproto.DesiredCredential) ([]string, error) {
	wanted := make(map[string]controlproto.DesiredCredential, len(desired))
	for _, d := range desired {
		wanted[d.AccessKey] = d
	}

	var existing []dbpkg.Credential
	if err := tx.Find(&existing).Error; err != nil {
		return nil, err
	}
	existingByKey := make(map[string]dbpkg.Credential, len(existing))
	for _, c := range existing {
		existingByKey[c.AccessKey] = c
	}

	touched := make([]string, 0, len(desired))

	// Delete credentials the panel no longer declares.
	for _, c := range existing {
		if _, ok := wanted[c.AccessKey]; !ok {
			if err := tx.Where("access_key = ?", c.AccessKey).Delete(&dbpkg.Credential{}).Error; err != nil {
				return nil, err
			}
			touched = append(touched, c.AccessKey)
		}
	}

	// Upsert desired credentials, preserving node-owned UsedBytes.
	for _, d := range desired {
		status := d.Status
		if status == "" {
			status = "enabled"
		}
		if prior, ok := existingByKey[d.AccessKey]; ok {
			updates := map[string]any{
				"secret_key":  d.SecretKey,
				"name":        d.Name,
				"bucket":      d.Bucket,
				"status":      status,
				"quota_bytes": d.QuotaBytes,
			}
			if err := tx.Model(&dbpkg.Credential{}).Where("access_key = ?", d.AccessKey).Updates(updates).Error; err != nil {
				return nil, err
			}
			if credentialChanged(prior, d, status) {
				touched = append(touched, d.AccessKey)
			}
			continue
		}
		cred := dbpkg.Credential{
			AccessKey:  d.AccessKey,
			SecretKey:  d.SecretKey,
			Name:       d.Name,
			Bucket:     d.Bucket,
			Status:     status,
			QuotaBytes: d.QuotaBytes,
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "access_key"}},
			DoUpdates: clause.AssignmentColumns([]string{"secret_key", "name", "bucket", "status", "quota_bytes"}),
		}).Create(&cred).Error; err != nil {
			return nil, err
		}
		touched = append(touched, d.AccessKey)
	}

	return touched, nil
}

func credentialChanged(prior dbpkg.Credential, d controlproto.DesiredCredential, status string) bool {
	return prior.SecretKey != d.SecretKey ||
		prior.Name != d.Name ||
		prior.Bucket != d.Bucket ||
		prior.Status != status ||
		prior.QuotaBytes != d.QuotaBytes
}

// applyBuckets upserts desired buckets (name + ACL) and removes node-local
// bucket rows the panel no longer declares. It only reconciles the bucket
// metadata rows; it does not create or delete on-disk object directories, which
// are node-owned observed state (objects are never migrated by the control
// plane, design §8.3).
func applyBuckets(tx *gorm.DB, desired []controlproto.DesiredBucket) error {
	wanted := make(map[string]controlproto.DesiredBucket, len(desired))
	for _, d := range desired {
		wanted[d.Name] = d
	}

	var existing []dbpkg.Bucket
	if err := tx.Find(&existing).Error; err != nil {
		return err
	}
	for _, b := range existing {
		if _, ok := wanted[b.Name]; !ok {
			if err := tx.Where("name = ?", b.Name).Delete(&dbpkg.Bucket{}).Error; err != nil {
				return err
			}
		}
	}

	for _, d := range desired {
		acl := d.ACL
		if acl == "" {
			acl = "private"
		}
		bucket := dbpkg.Bucket{Name: d.Name, ACL: acl}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "name"}},
			DoUpdates: clause.AssignmentColumns([]string{"acl"}),
		}).Create(&bucket).Error; err != nil {
			return err
		}
	}
	return nil
}

// applyWebhooks replaces the node-local hook config set with the desired set.
// Hook configs have no natural unique key beyond URL+events in this schema, so
// the reconciliation is a full replace within the transaction.
func applyWebhooks(tx *gorm.DB, desired []controlproto.DesiredWebhook) error {
	// Clear existing hook rows, then insert the desired set. Done inside the
	// caller's transaction so a failure rolls back to the prior config.
	if err := tx.Where("1 = 1").Delete(&dbpkg.HookConfig{}).Error; err != nil {
		return err
	}
	for _, d := range desired {
		hook := dbpkg.HookConfig{URL: d.URL, Events: d.Events, Enabled: d.Enabled}
		if err := tx.Create(&hook).Error; err != nil {
			return err
		}
	}
	return nil
}
