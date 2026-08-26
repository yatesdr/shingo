package service

import (
	"time"

	"shingocore/domain"
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

// ListBinUOPByBin exposes the read side of bin_uop_ledger for the audit UI,
// so the handler can render a per-bin timeline without composing SQL.
//
// The per-operator and per-station-override readers that used to sit beside it
// were removed on 2026-08-22: their routes had no caller in any page or script,
// and they were the only queries filtering the ledger on `actor` — a column
// with no index and 99.2% of its rows under a single value.
func (s *AuditService) ListBinUOPByBin(binID int64, limit, offset int) ([]audit.BinUOPRow, error) {
	return audit.ListBinUOPByBin(s.db.DB, binID, limit, offset)
}

// ListBinUOPDiscrepancies exposes the discrepancy ledger — a view over
// bin_uop_ledger UNION bin_uop_exception (dropped stale ticks, negative
// crossings from the exceptions ledger, and release-empties that still
// carried counted parts). See audit.ListBinUOPDiscrepancies for the arms.
func (s *AuditService) ListBinUOPDiscrepancies(limit, offset int) ([]audit.BinUOPRow, error) {
	return audit.ListBinUOPDiscrepancies(s.db.DB, limit, offset)
}

// ListCycleEvents exposes the truth-path delta rows the cycle-time surface
// (5.10) computes its distributions from. Returns the events oldest-first plus
// whether the row cap bit — the page has to be able to say its window was
// narrowed, because a distribution over a silently shortened window misreports
// its own n.
func (s *AuditService) ListCycleEvents(since time.Time, limit int) ([]domain.CycleEvent, bool, error) {
	return audit.ListCycleEvents(s.db.DB, since, limit)
}
