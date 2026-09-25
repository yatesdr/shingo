package store

// Phase 5 delegate file: inventory listing lives in store/inventory/.
// This file preserves the *store.DB method surface so external callers
// don't need to change.

import (
	"fmt"

	"shingocore/store/inventory"
)

func (db *DB) ListInventory() ([]inventory.Row, error) {
	return inventory.List(db.DB)
}

// ListLinesideBuckets returns every authoritative lineside_buckets row
// joined to the node hierarchy. Surfaces the bucket data the
// /api/buckets endpoint and Core inventory page render against.
func (db *DB) ListLinesideBuckets() ([]inventory.BucketRow, error) {
	return inventory.ListLinesideBuckets(db.DB)
}

// DistinctStockedPayloads returns every payload_code with current stock on a
// bin or an active lineside pile. Feeds the Replenishment Health rollup so stocked but
// unmonitored payloads still appear.
func (db *DB) DistinctStockedPayloads() ([]string, error) {
	return inventory.DistinctStockedPayloads(db.DB)
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
