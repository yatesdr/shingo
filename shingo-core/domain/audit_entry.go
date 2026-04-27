package domain

import "time"

// AuditEntry is a single row in the audit log. The audit log records
// state-changing actions across entities (bins, orders, nodes, etc.)
// — the EntityType + EntityID pair points to the row that was
// touched, OldValue/NewValue describe the diff, and Actor records who
// performed the action.
//
// Stage 2A.2 lifted this struct out of shingo-core/store/audit so
// www handlers can build response shapes that include audit entries
// without importing the persistence sub-package. The store/audit
// package re-exports the type via `type Entry = domain.AuditEntry`.
type AuditEntry struct {
	ID         int64     `json:"id"`
	EntityType string    `json:"entity_type"`
	EntityID   int64     `json:"entity_id"`
	Action     string    `json:"action"`
	OldValue   string    `json:"old_value"`
	NewValue   string    `json:"new_value"`
	Actor      string    `json:"actor"`
	CreatedAt  time.Time `json:"created_at"`
}
