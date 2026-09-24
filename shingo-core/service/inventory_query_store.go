package service

import (
	"context"
	"database/sql"

	"shingocore/store"
	"shingocore/store/inventory"
)

// InventoryQueryStore is the narrow DB surface InventoryService depends on.
// No transactions; its one mutation is DeleteLinesideBucket.
//
// The four QueryContext call sites use dynamically-built IN-clause SQL
// (the placeholder construction is the query's business logic). Exposing
// QueryContext rather than wrapping each query as a typed method is the
// honest reflection of that shape.
type InventoryQueryStore interface {
	// Typed wrapper for the aggregated inventory rollup.
	ListInventory() ([]inventory.Row, error)

	// Typed wrapper for the lineside_buckets read-side listing.
	ListLinesideBuckets() ([]inventory.BucketRow, error)

	// DeleteLinesideBucket removes one bucket row + its matching
	// inventory_delta_dedup row. Powers the Round-3 Obs 10
	// admin clear-orphan capability. Returns the number of bucket
	// rows deleted (0 or 1).
	DeleteLinesideBucket(id int64) (int, error)

	// ListOpenReportDivergences lists the open report_divergence episodes —
	// where the Edge's lineside report and Core's replica still disagree.
	ListOpenReportDivergences() ([]store.OpenReportDivergence, error)

	// Raw SQL pass-through for the preflight / system-count / system-uop
	// queries. Each one builds its own IN (...) placeholder list at
	// runtime; abstracting that would just hide the actual query logic.
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Compile-time check.
var _ InventoryQueryStore = (*store.DB)(nil)
