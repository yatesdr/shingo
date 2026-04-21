package service

import (
	"shingocore/store"
)

// AuditService centralizes audit-log writes and reads. Handlers call
// AuditService for append + list operations instead of reaching through
// engine passthroughs to *store.DB.
//
// Absorbed from engine_db_methods.go as part of the Phase 3a closeout
// (PR 3a.6). Methods remain thin delegates here; any non-trivial audit
// composition belongs alongside the service that owns the entity being
// audited (BinService, OrderService, etc.) rather than inside this
// generic logger.
type AuditService struct {
	db *store.DB
}

func NewAuditService(db *store.DB) *AuditService {
	return &AuditService{db: db}
}

// Append writes a single audit entry. entityType + entityID identify
// the subject; action is a short verb ("status", "moved", "locked",
// etc.); oldValue / newValue encode the transition; actor records the
// source ("ui", username, "system").
func (s *AuditService) Append(entityType string, entityID int64, action, oldValue, newValue, actor string) error {
	return s.db.AppendAudit(entityType, entityID, action, oldValue, newValue, actor)
}

// ListForEntity returns the audit trail for a single entity, most
// recent first.
func (s *AuditService) ListForEntity(entityType string, entityID int64) ([]*store.AuditEntry, error) {
	return s.db.ListEntityAudit(entityType, entityID)
}
