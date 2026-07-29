package engine

import (
	"context"
	"sort"
	"time"

	"shingocore/domain"
	"shingocore/service"
)

// PayloadHealth is one row of the inventory Replenishment Health rollup: a
// payload's DB-truth on-hand (bins + lineside split), whether a threshold
// binding watches it, and its configured threshold(s).
//
// OnHand is the DB truth (bin_uop + bucket_uop) — and now the ONLY number in
// play. The monitor no longer keeps a private tally, so there is no "cached
// belief" that can drift from OnHand; the drift chip and its counter were
// deleted with that tally (the drift failure category no longer exists).
type PayloadHealth struct {
	PayloadCode string           `json:"payload_code"`
	Description string           `json:"description"`
	BinUOP      int              `json:"bin_uop"`
	BucketUOP   int              `json:"bucket_uop"`
	OnHand      int              `json:"on_hand"` // bin_uop + bucket_uop (DB truth)
	Monitored   bool             `json:"monitored"`
	Threshold   int              `json:"threshold"` // representative (max binding); 0 = unset
	Bindings    []MonitorBinding `json:"bindings"`
	// SwapContradiction is true when a manual swap was requested for this
	// payload while the ledger read it as fully stocked (P2-C9) — surfaced as a
	// right-hand chip on the Replenishment Health row.
	SwapContradiction bool `json:"swap_contradiction"`
	// LedgerNegative is true when this payload's plant-wide bin total is
	// BELOW ZERO — physically impossible, so the count is wrong.
	//
	// It does NOT mean ordering has stopped. It used to: checkBindings
	// refused to signal on a negative total, which paired a number saying the
	// line is empty with a system that ordered nothing, and was the first link
	// in the 2026-07-21 chain. That suppression is gone — a count goes
	// negative for mundane reasons (a press overpacked, a fork truck delivered
	// parts off the books, a manual move) and none of them are a reason to
	// starve a line.
	//
	// What it means now: someone should recount these bins. Ordering carries
	// on from the best reading available meanwhile.
	LedgerNegative bool `json:"ledger_negative"`
	// LedgerTotal is the negative bin total, for the chip's tooltip. Only
	// meaningful when LedgerNegative.
	LedgerTotal int `json:"ledger_total,omitempty"`
}

// ReplenishmentHealth builds the per-payload rollup behind the inventory
// Replenishment Health section. It unions the monitored payloads (those with a
// threshold binding) with every stocked payload, so unmonitored-but-stocked
// payloads still surface (as "no threshold set"). For each it reports the DB
// truth (bin + bucket UOP) and the configured threshold(s). Rows come back
// sorted by payload code; the page applies its own worst-first ordering.
func (e *Engine) ReplenishmentHealth(ctx context.Context) ([]PayloadHealth, error) {
	var snap []MonitorSnapshotEntry
	if e.thresholdMonitor != nil {
		snap = e.thresholdMonitor.Snapshot()
	}
	byPayload := make(map[string]MonitorSnapshotEntry, len(snap))
	set := make(map[string]struct{}, len(snap))
	for _, s := range snap {
		byPayload[s.PayloadCode] = s
		set[s.PayloadCode] = struct{}{}
	}

	stocked, err := e.db.DistinctStockedPayloads()
	if err != nil {
		return nil, err
	}
	for _, p := range stocked {
		set[p] = struct{}{}
	}
	if len(set) == 0 {
		return []PayloadHealth{}, nil
	}

	payloads := make([]string, 0, len(set))
	for p := range set {
		payloads = append(payloads, p)
	}

	uop, err := e.inventoryService.SystemUOPForPayload(ctx, payloads)
	if err != nil {
		return nil, err
	}
	uopByCode := make(map[string]service.PayloadSystemUOP, len(uop.Counts))
	for _, c := range uop.Counts {
		uopByCode[c.PayloadCode] = c
	}

	// Payloads whose bin total is negative — the ones whose replenishment is
	// being decided from an untrustworthy count (it is not suppressed; that
	// behaviour was removed). One grouped read, not one per row.
	negative, err := e.db.NegativeLedgerPayloads()
	if err != nil {
		// Degrade to no chip rather than failing the page: the rest of the
		// rollup is still correct and useful without it.
		e.logFn("replenishment health: negative ledger payloads: %v", err)
		negative = map[string]int{}
	}

	descs, err := e.db.PayloadDescriptions()
	if err != nil {
		// Descriptions are cosmetic — degrade to codes only rather than fail the
		// whole page load.
		e.logFn("replenishment health: payload descriptions: %v", err)
		descs = map[string]string{}
	}

	out := make([]PayloadHealth, 0, len(payloads))
	for _, p := range payloads {
		u := uopByCode[p]
		row := PayloadHealth{
			PayloadCode: p,
			Description: descs[p],
			BinUOP:      u.BinUOP,
			BucketUOP:   u.BucketUOP,
			OnHand:      u.TotalUOP,
		}
		if total, neg := negative[p]; neg {
			row.LedgerNegative = true
			row.LedgerTotal = total
		}
		if s, ok := byPayload[p]; ok {
			row.Monitored = true
			row.Bindings = s.Bindings
			row.SwapContradiction = s.SwapContradiction
			for _, b := range s.Bindings {
				if b.Threshold > row.Threshold {
					row.Threshold = b.Threshold
				}
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PayloadCode < out[j].PayloadCode })
	return out, nil
}

// ── Ledger-integrity reads (Phase 4.6) ───────────────────────────────────
//
// Read-side only: every value comes from bins and bin_uop_audit, which are
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

// DeltaIntegrityDaily reports the same drops bucketed by plant-local day. The
// "when" axis: the per-payload split cannot distinguish an incident from a
// trend, and at Springfield the difference was 76% of a month in one day.
func (e *Engine) DeltaIntegrityDaily(since time.Time, tz string) ([]domain.DeltaDay, error) {
	return e.db.DeltaIntegrityDaily(since, tz)
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
