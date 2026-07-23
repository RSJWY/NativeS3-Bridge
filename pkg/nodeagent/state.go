// Package nodeagent implements the node-side control-plane agent: first-boot
// registration (local key + CSR), the mTLS WebSocket client (hello, heartbeat,
// reconnect with backoff), and the desired-state executor that applies the
// panel's authoritative business config to the node-local database.
//
// The agent's own persistent state (last applied desired-state version, content
// hash, and task-idempotency records) lives in additive tables in the SAME
// node database as credentials/buckets/request_stats. These tables are strictly
// additive (safety net C, design §8.3): they never modify or delete existing
// node structures, so downgrading to the pre-multinode binary simply ignores
// them. They are migrated by MigrateState, NOT by pkg/db.Migrate, so the
// deprecated standalone binary is unaffected.
package nodeagent

import (
	"errors"
	"fmt"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"gorm.io/gorm"
)

// AgentMeta is a single-row table holding the last successfully applied
// desired-state version and its content hash. The node reports these in the
// hello handshake so the panel can decide whether reconciliation is needed.
type AgentMeta struct {
	ID             uint   `gorm:"primaryKey"`
	AppliedVersion int64  `gorm:"not null;default:0"`
	ContentHash    string `gorm:"size:64"`
	UpdatedAt      time.Time
}

// AppliedTask records a one-shot task the node has already executed, keyed by
// the panel's task ID. It is the node-side idempotency ledger: a duplicate task
// delivery is detected here and the cached result is re-sent instead of
// re-executing (critical for high-risk reconcile-apply tasks).
type AppliedTask struct {
	ID         uint   `gorm:"primaryKey"`
	TaskID     string `gorm:"uniqueIndex;size:64;not null"`
	Type       string `gorm:"size:32;not null"`
	State      string `gorm:"size:16;not null"`
	ResultJSON string `gorm:"type:text"`
	CreatedAt  time.Time
}

// ManagedRateLimit is the last successfully applied anonymous rate-limit
// policy. Absence of the singleton row means the built-in defaults are active.
type ManagedRateLimit struct {
	ID             uint    `gorm:"primaryKey"`
	AnonymousRPS   float64 `gorm:"not null"`
	AnonymousBurst int     `gorm:"not null"`
	TrustForwarded bool    `gorm:"not null;default:false"`
	UpdatedAt      time.Time
}

var stateModels = []any{&AgentMeta{}, &AppliedTask{}, &ManagedRateLimit{}}

var stateExpectedTables = []struct {
	name  string
	model any
}{
	{name: "agent_meta", model: &AgentMeta{}},
	{name: "applied_tasks", model: &AppliedTask{}},
	{name: "managed_rate_limits", model: &ManagedRateLimit{}},
}

func LoadManagedRateLimit(gdb *gorm.DB) (config.RateLimitConfig, bool, error) {
	var row ManagedRateLimit
	if err := gdb.First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return config.RateLimitConfig{
				AnonymousRPS: config.DefaultAnonymousRPS, AnonymousBurst: config.DefaultAnonymousBurst,
			}, false, nil
		}
		return config.RateLimitConfig{}, false, err
	}
	return config.RateLimitConfig{
		AnonymousRPS: row.AnonymousRPS, AnonymousBurst: row.AnonymousBurst, TrustForwarded: row.TrustForwarded,
	}, true, nil
}

var stateExpectedIndexes = []struct {
	table string
	name  string
	model any
}{
	{table: "applied_tasks", name: "idx_applied_tasks_task_id", model: &AppliedTask{}},
}

// MigrateState additively migrates the node-agent state tables into the node
// database. It must be called AFTER pkg/db.MigrateConfigured and only from
// cmd/node — never folded into pkg/db.Migrate — so existing base tables and the
// deprecated standalone binary are untouched (safety net C). It is idempotent.
func MigrateState(gdb *gorm.DB) error {
	if gdb == nil {
		return errors.New("database handle is nil")
	}
	if err := gdb.AutoMigrate(stateModels...); err != nil {
		return fmt.Errorf("migrate agent state: %w", err)
	}
	for _, table := range stateExpectedTables {
		if !gdb.Migrator().HasTable(table.model) {
			return fmt.Errorf("missing table %q", table.name)
		}
	}
	for _, index := range stateExpectedIndexes {
		if !gdb.Migrator().HasIndex(index.model, index.name) {
			return fmt.Errorf("missing index %q on table %q", index.name, index.table)
		}
	}
	return nil
}

// LoadMeta returns the agent meta row, creating a zero row on first use so the
// node reports applied_version=0 before it has ever synced.
func LoadMeta(gdb *gorm.DB) (AgentMeta, error) {
	var meta AgentMeta
	err := gdb.First(&meta).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		meta = AgentMeta{AppliedVersion: 0, UpdatedAt: time.Now().UTC()}
		if err := gdb.Create(&meta).Error; err != nil {
			return AgentMeta{}, fmt.Errorf("init agent meta: %w", err)
		}
		return meta, nil
	}
	if err != nil {
		return AgentMeta{}, err
	}
	return meta, nil
}

// SaveMeta persists the applied version and content hash after a successful
// desired-state apply. It upserts the single meta row.
func SaveMeta(gdb *gorm.DB, appliedVersion int64, contentHash string) error {
	return gdb.Transaction(func(tx *gorm.DB) error {
		var meta AgentMeta
		err := tx.First(&meta).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return tx.Create(&AgentMeta{
				AppliedVersion: appliedVersion,
				ContentHash:    contentHash,
				UpdatedAt:      time.Now().UTC(),
			}).Error
		}
		if err != nil {
			return err
		}
		return tx.Model(&AgentMeta{}).Where("id = ?", meta.ID).Updates(map[string]any{
			"applied_version": appliedVersion,
			"content_hash":    contentHash,
			"updated_at":      time.Now().UTC(),
		}).Error
	})
}
