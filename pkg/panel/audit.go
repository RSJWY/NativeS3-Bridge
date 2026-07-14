package panel

import (
	"log/slog"

	"gorm.io/gorm"
)

// Auditor writes management-action audit records. It centralizes the redaction
// contract (design §7.2): audit rows record time, action, target node, target
// resource, result, and source — and NEVER private keys, full secret keys,
// registration token plaintext, or session credentials. Callers pass only
// non-sensitive identifiers (access keys, node IDs, fingerprints); this type
// has no field for a secret and offers no way to log one.
type Auditor struct {
	db *gorm.DB
}

// NewAuditor builds an auditor over the panel DB.
func NewAuditor(db *gorm.DB) *Auditor { return &Auditor{db: db} }

// AuditEntry is the non-sensitive audit input. There is deliberately no field
// for a secret/token/session value.
type AuditEntry struct {
	Action         string
	TargetNode     uint
	TargetResource string // e.g. access key, cert fingerprint, task id — never a secret
	Result         string
	Source         string // admin identity or "control-plane"
}

// Write records an audit entry. It best-effort logs a failure but never blocks
// the caller's operation on audit-write errors.
func (a *Auditor) Write(e AuditEntry) {
	if a == nil || a.db == nil {
		return
	}
	source := e.Source
	if source == "" {
		source = "admin"
	}
	row := AuditLog{
		TS:             nowUTC(),
		Action:         e.Action,
		TargetNode:     e.TargetNode,
		TargetResource: redactResource(e.TargetResource),
		Result:         e.Result,
		Source:         source,
	}
	if err := a.db.Create(&row).Error; err != nil {
		slog.Error("write audit log failed", "action", e.Action, "error", err)
	}
}

// redactResource is a defense-in-depth guard: even though callers are expected
// to pass only non-sensitive identifiers, we cap length so an accidentally
// oversized value cannot bloat the row. It does not attempt to detect secrets;
// the type contract keeps secrets out entirely.
func redactResource(s string) string {
	const maxLen = 256
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}
