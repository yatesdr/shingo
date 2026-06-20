package service

import (
	"shingocore/store"
	"shingocore/store/audit"
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
func (s *AuditService) ListForEntity(entityType string, entityID int64) ([]*audit.Entry, error) {
	return s.db.ListEntityAudit(entityType, entityID)
}

// ListBinUOPByBin / ListBinUOPByOperator / ListBinUOPOverridesByStation
// expose the read side of bin_uop_audit for the Item 10 audit UI.
// Handlers call these directly so the UI can render per-bin timelines,
// per-operator activity, and per-station override-pattern reports
// without composing SQL in the handler layer.
func (s *AuditService) ListBinUOPByBin(binID int64, limit, offset int) ([]audit.BinUOPRow, error) {
	return audit.ListBinUOPByBin(s.db.DB, binID, limit, offset)
}

func (s *AuditService) ListBinUOPByOperator(actor string, limit, offset int) ([]audit.BinUOPRow, error) {
	return audit.ListBinUOPByOperator(s.db.DB, actor, limit, offset)
}

func (s *AuditService) ListBinUOPOverridesByStation(station string, limit, offset int) ([]audit.BinUOPRow, error) {
	return audit.ListBinUOPOverridesByStation(s.db.DB, station, limit, offset)
}

// ListBinUOPDiscrepancies exposes the discrepancy ledger — a read-only
// view over bin_uop_audit (dropped stale ticks, negative remaining, and
// release-empties that still carried counted parts). No separate table.
func (s *AuditService) ListBinUOPDiscrepancies(limit, offset int) ([]audit.BinUOPRow, error) {
	return audit.ListBinUOPDiscrepancies(s.db.DB, limit, offset)
}
