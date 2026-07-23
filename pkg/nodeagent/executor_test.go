package nodeagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
	"gorm.io/gorm"
)

type recordingInvalidator struct{ values []string }

func (r *recordingInvalidator) Invalidate(value string) { r.values = append(r.values, value) }

type recordingHooks struct{ configs []dbpkg.HookConfig }

func (r *recordingHooks) ReplaceConfigs(configs []dbpkg.HookConfig) {
	r.configs = append([]dbpkg.HookConfig(nil), configs...)
}

type recordingRateLimit struct{ config config.RateLimitConfig }

func (r *recordingRateLimit) Update(cfg config.RateLimitConfig) { r.config = cfg }

// openNodeDB opens a temp-file SQLite node DB with the base schema plus the
// additive agent state tables.
func openNodeDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := dbpkg.Open("sqlite", filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatalf("open node db: %v", err)
	}
	if err := dbpkg.Migrate(gdb); err != nil {
		t.Fatalf("migrate base schema: %v", err)
	}
	if err := MigrateState(gdb); err != nil {
		t.Fatalf("migrate agent state: %v", err)
	}
	return gdb
}

func TestApplyCreatesUpdatesAndDeletes(t *testing.T) {
	gdb := openNodeDB(t)
	ex := NewExecutor(gdb, nil)

	// Seed a pre-existing credential the panel will delete and a bucket it keeps.
	if err := gdb.Create(&dbpkg.Credential{AccessKey: "OLD", SecretKey: "s", Status: "enabled"}).Error; err != nil {
		t.Fatalf("seed old cred: %v", err)
	}

	state := controlproto.DesiredState{
		Credentials: []controlproto.DesiredCredential{
			{AccessKey: "AK1", SecretKey: "sk1", Name: "one", Status: "enabled", QuotaBytes: 100},
		},
		Buckets: []controlproto.DesiredBucket{
			{Name: "bucket-one", ACL: "private"},
		},
		Webhooks: []controlproto.DesiredWebhook{
			{URL: "https://hook.example.test/events", Events: "ObjectCreated", Enabled: true},
		},
	}
	hash, err := ex.Apply(state)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if hash == "" {
		t.Fatal("expected non-empty content hash")
	}

	// OLD deleted, AK1 present.
	var count int64
	gdb.Model(&dbpkg.Credential{}).Where("access_key = ?", "OLD").Count(&count)
	if count != 0 {
		t.Fatal("OLD credential should have been deleted")
	}
	var ak1 dbpkg.Credential
	if err := gdb.Where("access_key = ?", "AK1").First(&ak1).Error; err != nil {
		t.Fatalf("AK1 should exist: %v", err)
	}
	if ak1.SecretKey != "sk1" || ak1.QuotaBytes != 100 {
		t.Fatalf("AK1 not applied correctly: %+v", ak1)
	}

	// The applied hash must equal the panel's hash for the same logical content.
	if want := state.ContentHash(); want != hash {
		t.Fatalf("local hash %s != panel hash %s", hash, want)
	}
}

func TestApplyPreservesUsedBytes(t *testing.T) {
	gdb := openNodeDB(t)
	ex := NewExecutor(gdb, nil)

	// Existing credential with observed UsedBytes (node-owned observed state).
	if err := gdb.Create(&dbpkg.Credential{AccessKey: "AK1", SecretKey: "old", Status: "enabled", UsedBytes: 4096}).Error; err != nil {
		t.Fatalf("seed cred: %v", err)
	}

	// Panel pushes a new secret for the same key; UsedBytes must be preserved.
	_, err := ex.Apply(controlproto.DesiredState{
		Credentials: []controlproto.DesiredCredential{
			{AccessKey: "AK1", SecretKey: "rotated", Status: "enabled", QuotaBytes: 999},
		},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var ak1 dbpkg.Credential
	if err := gdb.Where("access_key = ?", "AK1").First(&ak1).Error; err != nil {
		t.Fatalf("load AK1: %v", err)
	}
	if ak1.SecretKey != "rotated" {
		t.Fatalf("secret not rotated: %q", ak1.SecretKey)
	}
	if ak1.UsedBytes != 4096 {
		t.Fatalf("UsedBytes should be preserved, got %d", ak1.UsedBytes)
	}
}

func TestLocalContentHashMatchesPanel(t *testing.T) {
	gdb := openNodeDB(t)
	ex := NewExecutor(gdb, nil)

	state := controlproto.DesiredState{
		Credentials: []controlproto.DesiredCredential{
			{AccessKey: "AK2", SecretKey: "s2", Status: "enabled"},
			{AccessKey: "AK1", SecretKey: "s1", Status: "enabled"},
		},
		Buckets: []controlproto.DesiredBucket{{Name: "bucket-one", ACL: "private"}},
	}
	applied, err := ex.Apply(state)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Re-reading local state and hashing must be stable/equal.
	reread, err := ex.LocalContentHash()
	if err != nil {
		t.Fatalf("local hash: %v", err)
	}
	if reread != applied {
		t.Fatalf("hash not stable: %s != %s", reread, applied)
	}
	if reread != state.ContentHash() {
		t.Fatalf("local hash %s != panel hash %s", reread, state.ContentHash())
	}
}

func TestSaveAndLoadMeta(t *testing.T) {
	gdb := openNodeDB(t)
	meta, err := LoadMeta(gdb)
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}
	if meta.AppliedVersion != 0 {
		t.Fatalf("fresh applied version = %d, want 0", meta.AppliedVersion)
	}
	if err := SaveMeta(gdb, 5, "hash5"); err != nil {
		t.Fatalf("save meta: %v", err)
	}
	meta, err = LoadMeta(gdb)
	if err != nil {
		t.Fatalf("reload meta: %v", err)
	}
	if meta.AppliedVersion != 5 || meta.ContentHash != "hash5" {
		t.Fatalf("meta not persisted: %+v", meta)
	}
}

func TestApplyDesiredStateCommitsMetaAndSwapsRuntime(t *testing.T) {
	gdb := openNodeDB(t)
	root := t.TempDir()
	credentialCache := &recordingInvalidator{}
	bucketCache := &recordingInvalidator{}
	hooks := &recordingHooks{}
	rateLimit := &recordingRateLimit{}
	executor := NewManagedExecutor(gdb, ExecutorRuntime{
		CredentialInvalidator: credentialCache,
		BucketInvalidator:     bucketCache,
		WebhookReplacer:       hooks,
		RateLimitUpdater:      rateLimit,
		DataRoot:              root,
	})
	state := controlproto.DesiredState{
		Buckets: []controlproto.DesiredBucket{{Name: "bucket-one", ACL: "public-read"}},
		Credentials: []controlproto.DesiredCredential{{
			AccessKey: "AKMANAGED", SecretKey: "managed-secret", Name: "app", Bucket: "bucket-one", Status: "enabled", QuotaBytes: 1024,
		}},
		Webhooks:  []controlproto.DesiredWebhook{{URL: "https://hooks.example.test/events", Events: "ObjectCreated", Enabled: false}},
		RateLimit: &controlproto.DesiredRateLimit{AnonymousRPS: 3, AnonymousBurst: 5, TrustForwarded: true},
	}
	payload := controlproto.DesiredStatePayload{Version: 7, ContentHash: state.ContentHash(), Content: state}
	if _, err := executor.ApplyDesiredState(payload); err != nil {
		t.Fatalf("ApplyDesiredState: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bucket-one")); err != nil {
		t.Fatalf("bucket directory: %v", err)
	}
	meta, err := LoadMeta(gdb)
	if err != nil || meta.AppliedVersion != 7 || meta.ContentHash != payload.ContentHash {
		t.Fatalf("meta = %+v err=%v", meta, err)
	}
	local, err := executor.LocalState()
	if err != nil || local.ContentHash() != payload.ContentHash || local.RateLimit == nil {
		t.Fatalf("local state = %+v err=%v", local, err)
	}
	if len(credentialCache.values) != 1 || credentialCache.values[0] != "AKMANAGED" {
		t.Fatalf("credential invalidations = %v", credentialCache.values)
	}
	if len(bucketCache.values) != 1 || bucketCache.values[0] != "bucket-one" {
		t.Fatalf("bucket invalidations = %v", bucketCache.values)
	}
	if len(hooks.configs) != 1 || hooks.configs[0].Enabled {
		t.Fatalf("runtime hooks = %+v", hooks.configs)
	}
	if rateLimit.config.AnonymousRPS != 3 || rateLimit.config.AnonymousBurst != 5 || !rateLimit.config.TrustForwarded {
		t.Fatalf("runtime rate limit = %+v", rateLimit.config)
	}

	retainedObject := filepath.Join(root, "bucket-one", "retained.txt")
	if err := os.WriteFile(retainedObject, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := controlproto.DesiredState{Credentials: []controlproto.DesiredCredential{}, Buckets: []controlproto.DesiredBucket{}, Webhooks: []controlproto.DesiredWebhook{}}
	if _, err := executor.ApplyDesiredState(controlproto.DesiredStatePayload{Version: 8, ContentHash: empty.ContentHash(), Content: empty}); err != nil {
		t.Fatalf("delete declaration apply: %v", err)
	}
	if data, err := os.ReadFile(retainedObject); err != nil || string(data) != "keep" {
		t.Fatalf("retained object = %q err=%v", data, err)
	}
	var bucketCount int64
	gdb.Model(&dbpkg.Bucket{}).Where("name = ?", "bucket-one").Count(&bucketCount)
	if bucketCount != 0 {
		t.Fatal("deleted bucket declaration still exists")
	}
	if rateLimit.config.AnonymousRPS != config.DefaultAnonymousRPS || rateLimit.config.AnonymousBurst != config.DefaultAnonymousBurst || rateLimit.config.TrustForwarded {
		t.Fatalf("reset runtime rate limit = %+v", rateLimit.config)
	}
}

func TestApplyDesiredStateRejectsRetainedUndeclaredData(t *testing.T) {
	gdb := openNodeDB(t)
	root := t.TempDir()
	bucketDir := filepath.Join(root, "retained-bucket")
	if err := os.MkdirAll(bucketDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucketDir, "private-object-name.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	executor := NewManagedExecutor(gdb, ExecutorRuntime{DataRoot: root})
	state := controlproto.DesiredState{Buckets: []controlproto.DesiredBucket{{Name: "retained-bucket", ACL: "private"}}}
	_, err := executor.ApplyDesiredState(controlproto.DesiredStatePayload{Version: 1, ContentHash: state.ContentHash(), Content: state})
	if err == nil || !strings.Contains(err.Error(), "retained-bucket") {
		t.Fatalf("apply error = %v", err)
	}
	if strings.Contains(err.Error(), "private-object-name") {
		t.Fatalf("apply error exposed object name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bucketDir, "private-object-name.txt")); err != nil {
		t.Fatalf("retained object removed: %v", err)
	}
	var count int64
	gdb.Model(&dbpkg.Bucket{}).Where("name = ?", "retained-bucket").Count(&count)
	if count != 0 {
		t.Fatal("failed apply wrote bucket metadata")
	}
}

func TestApplyDesiredStatePreflightFailureCleansEarlierCreatedDirectories(t *testing.T) {
	gdb := openNodeDB(t)
	root := t.TempDir()
	retainedDir := filepath.Join(root, "retained-bucket")
	if err := os.MkdirAll(retainedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retainedDir, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	executor := NewManagedExecutor(gdb, ExecutorRuntime{DataRoot: root})
	state := controlproto.DesiredState{Buckets: []controlproto.DesiredBucket{
		{Name: "created-first", ACL: "private"},
		{Name: "retained-bucket", ACL: "private"},
	}}
	_, err := executor.ApplyDesiredState(controlproto.DesiredStatePayload{Version: 1, ContentHash: state.ContentHash(), Content: state})
	if err == nil {
		t.Fatal("preflight unexpectedly succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, "created-first")); !os.IsNotExist(err) {
		t.Fatalf("earlier preflight directory was not cleaned: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(retainedDir, "keep.txt")); err != nil || string(data) != "keep" {
		t.Fatalf("retained object = %q err=%v", data, err)
	}
}

func TestApplyDesiredStateHashMismatchDoesNotWrite(t *testing.T) {
	gdb := openNodeDB(t)
	if err := gdb.Create(&dbpkg.Credential{AccessKey: "OLD", SecretKey: "old-secret", Status: "enabled"}).Error; err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(gdb, nil)
	state := controlproto.DesiredState{Credentials: []controlproto.DesiredCredential{{AccessKey: "NEW", SecretKey: "new-secret", Status: "enabled"}}}
	if _, err := executor.ApplyDesiredState(controlproto.DesiredStatePayload{Version: 2, ContentHash: "wrong", Content: state}); err == nil {
		t.Fatal("hash mismatch unexpectedly applied")
	}
	var old dbpkg.Credential
	if err := gdb.Where("access_key = ?", "OLD").First(&old).Error; err != nil {
		t.Fatalf("old credential lost: %v", err)
	}
	var metaCount int64
	gdb.Model(&AgentMeta{}).Count(&metaCount)
	if metaCount != 0 {
		t.Fatal("hash mismatch wrote agent metadata")
	}
}

func TestApplyDesiredStateRejectsVersionRegression(t *testing.T) {
	gdb := openNodeDB(t)
	executor := NewExecutor(gdb, nil)
	current := controlproto.DesiredState{Credentials: []controlproto.DesiredCredential{{AccessKey: "CURRENT", SecretKey: "current-secret", Status: "enabled"}}}
	if _, err := executor.ApplyDesiredState(controlproto.DesiredStatePayload{Version: 5, ContentHash: current.ContentHash(), Content: current}); err != nil {
		t.Fatal(err)
	}
	stale := controlproto.DesiredState{Credentials: []controlproto.DesiredCredential{{AccessKey: "STALE", SecretKey: "stale-secret", Status: "enabled"}}}
	if _, err := executor.ApplyDesiredState(controlproto.DesiredStatePayload{Version: 4, ContentHash: stale.ContentHash(), Content: stale}); err == nil || !strings.Contains(err.Error(), "version regression") {
		t.Fatalf("stale apply error = %v", err)
	}
	var currentCount, staleCount int64
	gdb.Model(&dbpkg.Credential{}).Where("access_key = ?", "CURRENT").Count(&currentCount)
	gdb.Model(&dbpkg.Credential{}).Where("access_key = ?", "STALE").Count(&staleCount)
	meta, err := LoadMeta(gdb)
	if err != nil || currentCount != 1 || staleCount != 0 || meta.AppliedVersion != 5 || meta.ContentHash != current.ContentHash() {
		t.Fatalf("state after stale apply: current=%d stale=%d meta=%+v err=%v", currentCount, staleCount, meta, err)
	}
}

func TestApplyDesiredStateTransactionRollbackSkipsRuntimeSwaps(t *testing.T) {
	gdb := openNodeDB(t)
	if err := gdb.Create(&dbpkg.Credential{AccessKey: "OLD", SecretKey: "old-secret", Status: "enabled"}).Error; err != nil {
		t.Fatal(err)
	}
	credentialCache := &recordingInvalidator{}
	bucketCache := &recordingInvalidator{}
	hooks := &recordingHooks{}
	rateLimit := &recordingRateLimit{}
	executor := NewManagedExecutor(gdb, ExecutorRuntime{
		CredentialInvalidator: credentialCache, BucketInvalidator: bucketCache,
		WebhookReplacer: hooks, RateLimitUpdater: rateLimit,
	})
	const callbackName = "test:fail_hook_create"
	if err := gdb.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "hook_configs" {
			tx.AddError(os.ErrPermission)
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer gdb.Callback().Create().Remove(callbackName)
	state := controlproto.DesiredState{
		Buckets:     []controlproto.DesiredBucket{{Name: "bucket-one", ACL: "private"}},
		Credentials: []controlproto.DesiredCredential{{AccessKey: "NEW", SecretKey: "new-secret", Bucket: "bucket-one", Status: "enabled"}},
		Webhooks:    []controlproto.DesiredWebhook{{URL: "https://hooks.example.test/events", Events: "ObjectCreated", Enabled: true}},
	}
	if _, err := executor.ApplyDesiredState(controlproto.DesiredStatePayload{Version: 2, ContentHash: state.ContentHash(), Content: state}); err == nil {
		t.Fatal("injected transaction failure unexpectedly succeeded")
	}
	var old dbpkg.Credential
	if err := gdb.Where("access_key = ?", "OLD").First(&old).Error; err != nil {
		t.Fatalf("rollback lost old credential: %v", err)
	}
	var newCount, bucketCount, metaCount int64
	gdb.Model(&dbpkg.Credential{}).Where("access_key = ?", "NEW").Count(&newCount)
	gdb.Model(&dbpkg.Bucket{}).Where("name = ?", "bucket-one").Count(&bucketCount)
	gdb.Model(&AgentMeta{}).Count(&metaCount)
	if newCount != 0 || bucketCount != 0 || metaCount != 0 {
		t.Fatalf("rollback residue new=%d bucket=%d meta=%d", newCount, bucketCount, metaCount)
	}
	if len(credentialCache.values) != 0 || len(bucketCache.values) != 0 || hooks.configs != nil || rateLimit.config.AnonymousRPS != 0 {
		t.Fatalf("runtime changed after rollback: creds=%v buckets=%v hooks=%v rate=%+v", credentialCache.values, bucketCache.values, hooks.configs, rateLimit.config)
	}
}

func TestApplyDesiredStateReadbackMismatchRollsBack(t *testing.T) {
	gdb := openNodeDB(t)
	if err := gdb.Exec(`
		CREATE TRIGGER mutate_managed_credential
		AFTER INSERT ON credentials
		BEGIN
			UPDATE credentials SET name = 'database-mutated' WHERE id = NEW.id;
		END;
	`).Error; err != nil {
		t.Fatal(err)
	}
	credentialCache := &recordingInvalidator{}
	executor := NewManagedExecutor(gdb, ExecutorRuntime{CredentialInvalidator: credentialCache})
	state := controlproto.DesiredState{Credentials: []controlproto.DesiredCredential{{
		AccessKey: "AKREADBACK", SecretKey: "secret", Name: "expected", Status: "enabled",
	}}}
	_, err := executor.ApplyDesiredState(controlproto.DesiredStatePayload{Version: 1, ContentHash: state.ContentHash(), Content: state})
	if err == nil || !strings.Contains(err.Error(), "applied desired state hash mismatch") {
		t.Fatalf("apply error = %v", err)
	}
	var credentialCount, metaCount int64
	gdb.Model(&dbpkg.Credential{}).Where("access_key = ?", "AKREADBACK").Count(&credentialCount)
	gdb.Model(&AgentMeta{}).Count(&metaCount)
	if credentialCount != 0 || metaCount != 0 {
		t.Fatalf("readback mismatch committed credential=%d meta=%d", credentialCount, metaCount)
	}
	if len(credentialCache.values) != 0 {
		t.Fatalf("readback mismatch invalidated cache: %v", credentialCache.values)
	}
}

func TestApplyDesiredStateImmediatelyInvalidatesCredentialAndACLCache(t *testing.T) {
	gdb := openNodeDB(t)
	root := t.TempDir()
	bucketStore := storage.NewBucketStore(gdb, root, time.Hour)
	if err := bucketStore.Create("bucket-one"); err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&dbpkg.Credential{AccessKey: "AKCACHE", SecretKey: "old-secret", Bucket: "bucket-one", Status: "enabled"}).Error; err != nil {
		t.Fatal(err)
	}
	credentialStore := auth.NewCredentialStore(gdb, time.Hour)
	if _, err := credentialStore.Get("AKCACHE"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bucketStore.GetACL("bucket-one"); err != nil {
		t.Fatal(err)
	}
	executor := NewManagedExecutor(gdb, ExecutorRuntime{
		CredentialInvalidator: credentialStore,
		BucketInvalidator:     bucketStore,
		DataRoot:              root,
	})
	state := controlproto.DesiredState{
		Buckets: []controlproto.DesiredBucket{{Name: "bucket-one", ACL: "public-read"}},
		Credentials: []controlproto.DesiredCredential{{
			AccessKey: "AKCACHE", SecretKey: "new-secret", Bucket: "bucket-one", Status: "disabled",
		}},
	}
	if _, err := executor.ApplyDesiredState(controlproto.DesiredStatePayload{Version: 3, ContentHash: state.ContentHash(), Content: state}); err != nil {
		t.Fatal(err)
	}
	credential, err := credentialStore.Get("AKCACHE")
	if err != nil || credential.SecretKey != "new-secret" || credential.Status != "disabled" {
		t.Fatalf("credential cache = %+v err=%v", credential, err)
	}
	acl, exists, err := bucketStore.GetACL("bucket-one")
	if err != nil || !exists || acl != "public-read" {
		t.Fatalf("bucket cache = acl=%q exists=%v err=%v", acl, exists, err)
	}
}
