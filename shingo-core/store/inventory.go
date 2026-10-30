package store

// Phase 5 delegate file: inventory listing lives in store/inventory/.
// This file preserves the *store.DB method surface so external callers
// don't need to change.

import (
	"fmt"
	"time"

	"shingocore/store/inventory"
	"shingocore/store/orders"
	"shingocore/store/sourceability"
)

func (db *DB) ListInventory() ([]inventory.Row, error) {
	return inventory.List(db.DB)
}

// ListLinesideBuckets returns every authoritative lineside_buckets row
// joined to the node hierarchy. Surfaces the bucket data the
// /api/buckets endpoint and the Core Bins page render against.
func (db *DB) ListLinesideBuckets() ([]inventory.BucketRow, error) {
	return inventory.ListLinesideBuckets(db.DB)
}

// SumBinUOP returns the signed total of bins.uop_remaining across all
// bin rows. Item 13 invariant probe; signed because the SME lock
// allows bins to go negative (overpack). Empty table returns 0.
func (db *DB) SumBinUOP() (int64, error) {
	var sum int64
	err := db.QueryRow(`SELECT COALESCE(SUM(uop_remaining), 0) FROM bins`).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("sum bins.uop_remaining: %w", err)
	}
	return sum, nil
}

// SumLinesideBuckets returns the total of lineside_buckets.qty across the
// ACTIVE piles, the ones on-hand counts; a stranded row is a count anomaly and
// is left out, so the invariant's bucket term agrees with SystemUOPForPayload.
// Always >= 0 by the schema's CHECK constraint.
func (db *DB) SumLinesideBuckets() (int64, error) {
	var sum int64
	err := db.QueryRow(`SELECT COALESCE(SUM(qty), 0) FROM lineside_buckets WHERE state = 'active'`).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("sum lineside_buckets.qty: %w", err)
	}
	return sum, nil
}

// LoopBindings, LoopBins, LoopClaims, LoopLiveOrders and LoopNodeRates are the
// /inventory page's reads (see store/inventory/loop.go).
func (db *DB) LoopBindings(start, end time.Time) ([]inventory.Binding, error) {
	return inventory.LoopBindings(db.DB, start, end)
}

// LoopBins lists the bins in the loop; outOfLoop is the in-loop sum's own predicate.
func (db *DB) LoopBins(outOfLoop string) ([]inventory.LoopBin, error) {
	return inventory.LoopBins(db.DB, outOfLoop)
}

// LoopClaims lists the running styles' claims.
func (db *DB) LoopClaims() ([]inventory.LoopClaim, error) {
	return inventory.LoopClaims(db.DB)
}

// LoopNodeRates reads sourceability's node rates over rateWindow, for the
// Lines table's time left.
func (db *DB) LoopNodeRates(rateWindow time.Duration) (sourceability.NodeRates, error) {
	return sourceability.ReadNodeRates(db.DB, rateWindow)
}

// LoopLiveOrders lists the live orders not yet delivered, for the Lines
// table's On the way.
func (db *DB) LoopLiveOrders() ([]inventory.LiveOrder, error) {
	return inventory.LoopLiveOrders(db.DB)
}

// LeadTimesByPayload is the calculator's input read for every payload at once.
func (db *DB) LeadTimesByPayload(r orders.LeadTimeRange) (map[string]orders.LeadTimeInputs, error) {
	return orders.LeadTimesByPayload(db.DB, r)
}
