package nodeagent

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"github.com/RSJWY/NativeS3-Bridge/pkg/managedconfig"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

type CredentialInvalidator interface {
	Invalidate(accessKey string)
}

type BucketInvalidator interface {
	Invalidate(name string)
}

type WebhookReplacer interface {
	ReplaceConfigs(configs []dbpkg.HookConfig)
}

type RateLimitUpdater interface {
	Update(config.RateLimitConfig)
}

type ExecutorRuntime struct {
	CredentialInvalidator CredentialInvalidator
	BucketInvalidator     BucketInvalidator
	WebhookReplacer       WebhookReplacer
	RateLimitUpdater      RateLimitUpdater
	DataRoot              string
}

// Executor applies a full authoritative snapshot to the node database and its
// live runtime views.
type Executor struct {
	db      *gorm.DB
	runtime ExecutorRuntime
}

// NewExecutor preserves the standalone test/legacy constructor. Managed node
// wiring uses NewManagedExecutor so bucket/runtime behavior is explicit.
func NewExecutor(gdb *gorm.DB, invalidator CredentialInvalidator) *Executor {
	return &Executor{db: gdb, runtime: ExecutorRuntime{CredentialInvalidator: invalidator}}
}

func NewManagedExecutor(gdb *gorm.DB, runtime ExecutorRuntime) *Executor {
	return &Executor{db: gdb, runtime: runtime}
}

// Apply is retained for direct unit callers. The control-plane client uses
// ApplyDesiredState so the version/hash are committed with the business rows.
func (e *Executor) Apply(state controlproto.DesiredState) (string, error) {
	return e.ApplyDesiredState(controlproto.DesiredStatePayload{
		ContentHash: state.ContentHash(), Content: state,
	})
}

func (e *Executor) ApplyDesiredState(payload controlproto.DesiredStatePayload) (string, error) {
	if payload.Version < 0 {
		return "", fmt.Errorf("desired version must be non-negative")
	}
	computedHash := payload.Content.ContentHash()
	if payload.ContentHash == "" || payload.ContentHash != computedHash {
		return "", fmt.Errorf("desired content hash mismatch")
	}
	if err := managedconfig.ValidateDesiredState(payload.Content); err != nil {
		return "", fmt.Errorf("validate desired state: %w", err)
	}
	if err := e.rejectVersionRegression(payload.Version); err != nil {
		return "", err
	}

	createdDirs, err := e.prepareBucketDirectories(payload.Content.Buckets)
	if err != nil {
		cleanupBucketDirectories(createdDirs)
		return "", err
	}

	hookConfigs := desiredHookConfigs(payload.Content.Webhooks)
	effectiveRateLimit := config.RateLimitConfig{
		AnonymousRPS: config.DefaultAnonymousRPS, AnonymousBurst: config.DefaultAnonymousBurst,
	}
	if payload.Content.RateLimit != nil {
		effectiveRateLimit = config.RateLimitConfig{
			AnonymousRPS:   payload.Content.RateLimit.AnonymousRPS,
			AnonymousBurst: payload.Content.RateLimit.AnonymousBurst,
			TrustForwarded: payload.Content.RateLimit.TrustForwarded,
		}
	}

	var credentialKeys []string
	var bucketNames []string
	appliedHash := ""
	err = e.db.Transaction(func(tx *gorm.DB) error {
		var err error
		bucketNames, err = applyBuckets(tx, payload.Content.Buckets)
		if err != nil {
			return fmt.Errorf("apply buckets: %w", err)
		}
		credentialKeys, err = applyCredentials(tx, payload.Content.Credentials)
		if err != nil {
			return fmt.Errorf("apply credentials: %w", err)
		}
		if err := applyWebhooks(tx, hookConfigs); err != nil {
			return fmt.Errorf("apply webhooks: %w", err)
		}
		if err := applyManagedRateLimit(tx, payload.Content.RateLimit); err != nil {
			return fmt.Errorf("apply rate limit: %w", err)
		}
		appliedState, err := localState(tx)
		if err != nil {
			return fmt.Errorf("read back applied state: %w", err)
		}
		appliedHash = appliedState.ContentHash()
		if appliedHash != computedHash {
			return fmt.Errorf("applied desired state hash mismatch")
		}
		if err := saveMetaTx(tx, payload.Version, appliedHash); err != nil {
			return fmt.Errorf("save applied metadata: %w", err)
		}
		return nil
	})
	if err != nil {
		cleanupBucketDirectories(createdDirs)
		return "", err
	}

	if e.runtime.CredentialInvalidator != nil {
		for _, accessKey := range uniqueStrings(credentialKeys) {
			e.runtime.CredentialInvalidator.Invalidate(accessKey)
		}
	}
	if e.runtime.BucketInvalidator != nil {
		for _, name := range uniqueStrings(bucketNames) {
			e.runtime.BucketInvalidator.Invalidate(name)
		}
	}
	if e.runtime.WebhookReplacer != nil {
		e.runtime.WebhookReplacer.ReplaceConfigs(hookConfigs)
	}
	if e.runtime.RateLimitUpdater != nil {
		e.runtime.RateLimitUpdater.Update(effectiveRateLimit)
	}
	return appliedHash, nil
}

func (e *Executor) rejectVersionRegression(version int64) error {
	var meta AgentMeta
	if err := e.db.First(&meta).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return fmt.Errorf("load applied metadata: %w", err)
	}
	if version < meta.AppliedVersion {
		return fmt.Errorf("desired version regression: current=%d received=%d", meta.AppliedVersion, version)
	}
	return nil
}

func cleanupBucketDirectories(paths []string) {
	for i := len(paths) - 1; i >= 0; i-- {
		// Remove only empty directories created by this apply attempt. If another
		// process has populated one in the meantime, preserve those bytes.
		_ = os.Remove(paths[i])
	}
}

func (e *Executor) prepareBucketDirectories(desired []controlproto.DesiredBucket) ([]string, error) {
	if e.runtime.DataRoot == "" {
		return nil, nil
	}
	var existing []dbpkg.Bucket
	if err := e.db.Find(&existing).Error; err != nil {
		return nil, fmt.Errorf("load managed buckets: %w", err)
	}
	existingNames := make(map[string]struct{}, len(existing))
	for _, bucket := range existing {
		existingNames[bucket.Name] = struct{}{}
	}

	created := make([]string, 0)
	for _, bucket := range desired {
		path, err := storage.ResolveBucketPath(e.runtime.DataRoot, bucket.Name)
		if err != nil {
			return created, fmt.Errorf("prepare bucket %q: %w", bucket.Name, err)
		}
		info, statErr := os.Stat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return created, fmt.Errorf("create bucket directory %q: %w", bucket.Name, err)
			}
			created = append(created, path)
			continue
		}
		if statErr != nil {
			return created, fmt.Errorf("inspect bucket directory %q: %w", bucket.Name, statErr)
		}
		if !info.IsDir() {
			return created, fmt.Errorf("bucket path %q is not a directory", bucket.Name)
		}
		if _, alreadyManaged := existingNames[bucket.Name]; alreadyManaged {
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return created, fmt.Errorf("inspect retained bucket %q: %w", bucket.Name, err)
		}
		if len(entries) > 0 {
			return created, fmt.Errorf("retained data prevents declaring bucket %q", bucket.Name)
		}
	}
	return created, nil
}

func (e *Executor) LocalContentHash() (string, error) {
	state, err := e.LocalState()
	if err != nil {
		return "", err
	}
	return state.ContentHash(), nil
}

func (e *Executor) LocalState() (controlproto.DesiredState, error) {
	return localState(e.db)
}

func localState(db *gorm.DB) (controlproto.DesiredState, error) {
	state := controlproto.DesiredState{
		Credentials: []controlproto.DesiredCredential{},
		Buckets:     []controlproto.DesiredBucket{},
		Webhooks:    []controlproto.DesiredWebhook{},
	}

	var credentials []dbpkg.Credential
	if err := db.Order("access_key ASC").Find(&credentials).Error; err != nil {
		return state, err
	}
	for _, credential := range credentials {
		state.Credentials = append(state.Credentials, controlproto.DesiredCredential{
			AccessKey: credential.AccessKey, SecretKey: credential.SecretKey, Name: credential.Name,
			Bucket: credential.Bucket, Status: credential.Status, QuotaBytes: credential.QuotaBytes,
		})
	}

	var buckets []dbpkg.Bucket
	if err := db.Order("name ASC").Find(&buckets).Error; err != nil {
		return state, err
	}
	for _, bucket := range buckets {
		state.Buckets = append(state.Buckets, controlproto.DesiredBucket{Name: bucket.Name, ACL: bucket.ACL})
	}

	var hooks []dbpkg.HookConfig
	if err := db.Order("url ASC, events ASC, id ASC").Find(&hooks).Error; err != nil {
		return state, err
	}
	for _, hook := range hooks {
		state.Webhooks = append(state.Webhooks, controlproto.DesiredWebhook{URL: hook.URL, Events: hook.Events, Enabled: hook.Enabled})
	}

	var rateLimit ManagedRateLimit
	if err := db.First(&rateLimit).Error; err == nil {
		state.RateLimit = &controlproto.DesiredRateLimit{
			AnonymousRPS: rateLimit.AnonymousRPS, AnonymousBurst: rateLimit.AnonymousBurst, TrustForwarded: rateLimit.TrustForwarded,
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return state, err
	}
	return state, nil
}

func applyCredentials(tx *gorm.DB, desired []controlproto.DesiredCredential) ([]string, error) {
	wanted := make(map[string]controlproto.DesiredCredential, len(desired))
	for _, credential := range desired {
		wanted[credential.AccessKey] = credential
	}

	var existing []dbpkg.Credential
	if err := tx.Find(&existing).Error; err != nil {
		return nil, err
	}
	existingByKey := make(map[string]dbpkg.Credential, len(existing))
	for _, credential := range existing {
		existingByKey[credential.AccessKey] = credential
	}

	touched := make([]string, 0, len(existing)+len(desired))
	for _, credential := range existing {
		if _, ok := wanted[credential.AccessKey]; !ok {
			if err := tx.Where("access_key = ?", credential.AccessKey).Delete(&dbpkg.Credential{}).Error; err != nil {
				return nil, err
			}
			touched = append(touched, credential.AccessKey)
		}
	}

	for _, desiredCredential := range desired {
		status := managedconfig.NormalizeCredentialStatus(desiredCredential.Status)
		if prior, ok := existingByKey[desiredCredential.AccessKey]; ok {
			if credentialChanged(prior, desiredCredential, status) {
				if err := tx.Model(&dbpkg.Credential{}).Where("access_key = ?", desiredCredential.AccessKey).Updates(map[string]any{
					"secret_key": desiredCredential.SecretKey, "name": desiredCredential.Name, "bucket": desiredCredential.Bucket,
					"status": status, "quota_bytes": desiredCredential.QuotaBytes,
				}).Error; err != nil {
					return nil, err
				}
				touched = append(touched, desiredCredential.AccessKey)
			}
			continue
		}
		credential := dbpkg.Credential{
			AccessKey: desiredCredential.AccessKey, SecretKey: desiredCredential.SecretKey, Name: desiredCredential.Name,
			Bucket: desiredCredential.Bucket, Status: status, QuotaBytes: desiredCredential.QuotaBytes,
		}
		if err := tx.Create(&credential).Error; err != nil {
			return nil, err
		}
		touched = append(touched, desiredCredential.AccessKey)
	}
	return touched, nil
}

func credentialChanged(prior dbpkg.Credential, desired controlproto.DesiredCredential, status string) bool {
	return prior.SecretKey != desired.SecretKey || prior.Name != desired.Name || prior.Bucket != desired.Bucket ||
		prior.Status != status || prior.QuotaBytes != desired.QuotaBytes
}

func applyBuckets(tx *gorm.DB, desired []controlproto.DesiredBucket) ([]string, error) {
	wanted := make(map[string]controlproto.DesiredBucket, len(desired))
	for _, bucket := range desired {
		wanted[bucket.Name] = bucket
	}
	var existing []dbpkg.Bucket
	if err := tx.Find(&existing).Error; err != nil {
		return nil, err
	}
	existingByName := make(map[string]dbpkg.Bucket, len(existing))
	for _, bucket := range existing {
		existingByName[bucket.Name] = bucket
	}
	touched := make([]string, 0, len(existing)+len(desired))
	for _, bucket := range existing {
		if _, ok := wanted[bucket.Name]; !ok {
			if err := tx.Where("name = ?", bucket.Name).Delete(&dbpkg.Bucket{}).Error; err != nil {
				return nil, err
			}
			touched = append(touched, bucket.Name)
		}
	}
	for _, desiredBucket := range desired {
		if prior, ok := existingByName[desiredBucket.Name]; ok {
			if prior.ACL != desiredBucket.ACL {
				if err := tx.Model(&dbpkg.Bucket{}).Where("name = ?", desiredBucket.Name).Update("acl", desiredBucket.ACL).Error; err != nil {
					return nil, err
				}
				touched = append(touched, desiredBucket.Name)
			}
			continue
		}
		if err := tx.Create(&dbpkg.Bucket{Name: desiredBucket.Name, ACL: desiredBucket.ACL}).Error; err != nil {
			return nil, err
		}
		touched = append(touched, desiredBucket.Name)
	}
	return touched, nil
}

func desiredHookConfigs(desired []controlproto.DesiredWebhook) []dbpkg.HookConfig {
	configs := make([]dbpkg.HookConfig, 0, len(desired))
	for _, webhook := range desired {
		configs = append(configs, dbpkg.HookConfig{URL: webhook.URL, Events: webhook.Events, Enabled: webhook.Enabled})
	}
	return configs
}

func applyWebhooks(tx *gorm.DB, desired []dbpkg.HookConfig) error {
	if err := tx.Where("1 = 1").Delete(&dbpkg.HookConfig{}).Error; err != nil {
		return err
	}
	for _, hook := range desired {
		row := hook
		row.Enabled = true
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		if !hook.Enabled {
			if err := tx.Model(&dbpkg.HookConfig{}).Where("id = ?", row.ID).Update("enabled", false).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func applyManagedRateLimit(tx *gorm.DB, desired *controlproto.DesiredRateLimit) error {
	if desired == nil {
		return tx.Where("1 = 1").Delete(&ManagedRateLimit{}).Error
	}
	row := ManagedRateLimit{
		ID: 1, AnonymousRPS: desired.AnonymousRPS, AnonymousBurst: desired.AnonymousBurst,
		TrustForwarded: desired.TrustForwarded, UpdatedAt: time.Now().UTC(),
	}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"anonymous_rps": desired.AnonymousRPS, "anonymous_burst": desired.AnonymousBurst,
			"trust_forwarded": desired.TrustForwarded, "updated_at": row.UpdatedAt,
		}),
	}).Create(&row).Error
}

func saveMetaTx(tx *gorm.DB, appliedVersion int64, contentHash string) error {
	var meta AgentMeta
	if err := tx.First(&meta).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return tx.Create(&AgentMeta{AppliedVersion: appliedVersion, ContentHash: contentHash, UpdatedAt: time.Now().UTC()}).Error
		}
		return err
	}
	return tx.Model(&AgentMeta{}).Where("id = ?", meta.ID).Updates(map[string]any{
		"applied_version": appliedVersion, "content_hash": contentHash, "updated_at": time.Now().UTC(),
	}).Error
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
