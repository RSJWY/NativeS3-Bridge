package panel

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// migrationModels is the panel's own model registry, deliberately independent of
// pkg/db's node-level registry. The panel DB and node DB migrate separately and
// never share tables.
var migrationModels = []any{
	&Node{},
	&NodeCert{},
	&RegistrationToken{},
	&DesiredConfig{},
	&NodeState{},
	&NodeCredential{},
	&NodeBucket{},
	&NodeWebhook{},
	&NodeRateLimit{},
	&Task{},
	&AuditLog{},
}

var expectedTables = []struct {
	name  string
	model any
}{
	{name: "nodes", model: &Node{}},
	{name: "node_certs", model: &NodeCert{}},
	{name: "registration_tokens", model: &RegistrationToken{}},
	{name: "desired_configs", model: &DesiredConfig{}},
	{name: "node_states", model: &NodeState{}},
	{name: "node_credentials", model: &NodeCredential{}},
	{name: "node_buckets", model: &NodeBucket{}},
	{name: "node_webhooks", model: &NodeWebhook{}},
	{name: "node_rate_limits", model: &NodeRateLimit{}},
	{name: "tasks", model: &Task{}},
	{name: "audit_logs", model: &AuditLog{}},
}

var expectedIndexes = []struct {
	table string
	name  string
	model any
}{
	{table: "node_certs", name: "idx_node_certs_fingerprint", model: &NodeCert{}},
	{table: "registration_tokens", name: "idx_registration_tokens_token_hash", model: &RegistrationToken{}},
	{table: "desired_configs", name: "idx_desired_configs_node_id", model: &DesiredConfig{}},
	{table: "node_states", name: "idx_node_states_node_id", model: &NodeState{}},
	{table: "node_credentials", name: "idx_node_access", model: &NodeCredential{}},
	{table: "node_buckets", name: "idx_node_bucket", model: &NodeBucket{}},
	{table: "node_webhooks", name: "idx_node_webhooks_node_id", model: &NodeWebhook{}},
	{table: "node_rate_limits", name: "idx_node_ratelimit", model: &NodeRateLimit{}},
	{table: "tasks", name: "idx_tasks_task_id", model: &Task{}},
}

// Migrate runs AutoMigrate for the panel schema and validates that the expected
// tables and key indexes exist afterward, mirroring pkg/db.Migrate. It is safe
// to call repeatedly.
func Migrate(gdb *gorm.DB) error {
	if gdb == nil {
		return errors.New("database handle is nil")
	}
	if err := gdb.AutoMigrate(migrationModels...); err != nil {
		return err
	}
	if err := validateSchema(gdb); err != nil {
		return fmt.Errorf("validate schema: %w", err)
	}
	return nil
}

func validateSchema(gdb *gorm.DB) error {
	for _, table := range expectedTables {
		if !gdb.Migrator().HasTable(table.model) {
			return fmt.Errorf("missing table %q", table.name)
		}
	}
	for _, index := range expectedIndexes {
		if !gdb.Migrator().HasIndex(index.model, index.name) {
			return fmt.Errorf("missing index %q on table %q", index.name, index.table)
		}
	}
	return nil
}
