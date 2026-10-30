package engine

import (
	"time"

	"shingocore/domain"
)

// ── Ledger-integrity reads (Phase 4.6) ───────────────────────────────────
//
// Read-side only: every value comes from bins and bin_uop_ledger, which are
// already on disk. See store/bins/ledger_integrity.go for what each answers
// and why the negatives are deliberately not clamped away.

// OpenNegativeBins lists bins whose UOP ledger is negative right now — the
// exception list, blank on a good day.
func (e *Engine) OpenNegativeBins() ([]domain.OpenNegativeBin, error) {
	return e.db.OpenNegativeBins()
}

// NegativeLedgerPayloads maps payload code to its negative plant-wide bin
// total: the payloads whose replenishment is currently being decided from a
// count known to be wrong. Ordering continues for them — see
// store/bins/ledger_integrity.go.
func (e *Engine) NegativeLedgerPayloads() (map[string]int, error) {
	return e.db.NegativeLedgerPayloads()
}

// DeltaIntegrityByPayload reports dropped deltas per payload since `since`,
// each set beside that payload's current ledger total. The panel that says
// "and here is the mechanism" next to the one that says "this is negative".
func (e *Engine) DeltaIntegrityByPayload(since time.Time) ([]domain.DeltaIntegrity, error) {
	return e.db.DeltaIntegrityByPayload(since)
}

// CarrierBindings lists every carrier, the payload ShinGo believes it holds and
// when that belief last started. The read behind 5.11's stale-binding candidates
// (/material-flags), and it selects nothing — the candidate rule is a pure
// function in www so it can be tested at its boundary and can include the rows
// whose binding age is unknowable.
func (e *Engine) CarrierBindings() ([]domain.CarrierBinding, error) {
	return e.db.CarrierBindings()
}

// NegativeLedgerExcursions returns zero-crossings since `since` with the delta
// that caused each one.
func (e *Engine) NegativeLedgerExcursions(since time.Time, releaseWindow time.Duration, limit int) ([]domain.NegativeExcursion, error) {
	return e.db.NegativeLedgerExcursions(since, releaseWindow, limit)
}

// InventoryRecordAccuracy reports count staleness and correction magnitude.
func (e *Engine) InventoryRecordAccuracy(since time.Time, staleAfter time.Duration) (*domain.RecordAccuracy, error) {
	return e.db.InventoryRecordAccuracy(since, staleAfter)
}
