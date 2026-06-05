package hooks

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"gorm.io/gorm"
)

func TestManagerReloadFiltersDisabledHooks(t *testing.T) {
	gdb := testDB(t)
	if err := gdb.Create(&db.HookConfig{URL: "http://127.0.0.1:1/enabled", Events: "ObjectCreated,ObjectDeleted", Enabled: true}).Error; err != nil {
		t.Fatalf("create enabled hook: %v", err)
	}
	disabled := db.HookConfig{URL: "http://127.0.0.1:1/disabled", Events: "ObjectCreated", Enabled: true}
	if err := gdb.Create(&disabled).Error; err != nil {
		t.Fatalf("create disabled hook: %v", err)
	}
	if err := gdb.Model(&db.HookConfig{}).Where("id = ?", disabled.ID).Update("enabled", false).Error; err != nil {
		t.Fatalf("disable hook: %v", err)
	}

	m := NewManager(gdb, Config{Workers: 1, QueueSize: 1})
	if err := m.Reload(); err != nil {
		t.Fatalf("reload hooks: %v", err)
	}
	if len(m.hooks) != 1 {
		t.Fatalf("loaded hooks = %d, want 1", len(m.hooks))
	}
	if !m.hooks[0].Match(ObjectDeleted) {
		t.Fatal("enabled hook should match ObjectDeleted")
	}
}

func TestManagerRetriesDelivery(t *testing.T) {
	gdb := testDB(t)
	m := NewManager(gdb, Config{MaxRetry: 3})
	m.retryBaseDelay = time.Millisecond
	hook := &flakyHook{failures: 2}
	if err := m.deliverWithRetry(hook, Event{Type: ObjectCreated}); err != nil {
		t.Fatalf("deliver with retry: %v", err)
	}
	if hook.calls != 3 {
		t.Fatalf("calls = %d, want 3", hook.calls)
	}
}

func TestManagerExhaustsConfiguredRetries(t *testing.T) {
	gdb := testDB(t)
	m := NewManager(gdb, Config{MaxRetry: 3})
	m.retryBaseDelay = time.Millisecond
	hook := &flakyHook{failures: 10}
	if err := m.deliverWithRetry(hook, Event{Type: ObjectCreated}); err == nil {
		t.Fatal("expected exhausted retry error")
	}
	if hook.calls != 4 {
		t.Fatalf("calls = %d, want initial attempt plus 3 retries", hook.calls)
	}
}

type flakyHook struct {
	calls    int
	failures int
}

func (h *flakyHook) Match(EventType) bool { return true }

func (h *flakyHook) Deliver(Event) error {
	h.calls++
	if h.calls <= h.failures {
		return errors.New("temporary failure")
	}
	return nil
}

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := db.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(gdb); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return gdb
}
