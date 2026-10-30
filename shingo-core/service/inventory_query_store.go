package service

import (
	"context"
	"database/sql"
	"time"

	"shingocore/store"
	"shingocore/store/audit"
	"shingocore/store/inventory"
	"shingocore/store/orders"
	"shingocore/store/sourceability"
)

// InventoryQueryStore is the narrow DB surface InventoryService depends on.
// Read-only: no transactions and no mutations.
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

	// ListOpenReportDivergences lists the open report_divergence episodes —
	// where the Edge's lineside report and Core's replica still disagree.
	ListOpenReportDivergences() ([]store.OpenReportDivergence, error)

	// ListHoldDropRuns and ListDeclaredFullStamps are the strict-pack lists: the
	// Edge's dropped produce holds, as runs, and the stamps of a station's
	// release declared full.
	ListHoldDropRuns(since time.Time, gap time.Duration, limit int) ([]audit.HoldDropRun, error)
	ListDeclaredFullStamps(since time.Time, limit int) ([]audit.DeclaredFullStamp, error)

	// The /inventory page's reads (inventory_loop.go): the monitored
	// bindings, the bins in the loop, the running claims, the lines' node
	// rates, the live orders not yet delivered, and the calculator's inputs
	// for every part at once.
	LoopBindings(start, end time.Time) ([]inventory.Binding, error)
	LoopBins(outOfLoop string) ([]inventory.LoopBin, error)
	LoopClaims() ([]inventory.LoopClaim, error)
	LoopNodeRates(rateWindow time.Duration) (sourceability.NodeRates, error)
	LoopLiveOrders() ([]inventory.LiveOrder, error)
	LeadTimesByPayload(r orders.LeadTimeRange) (map[string]orders.LeadTimeInputs, error)

	// Raw SQL pass-through for the preflight / system-uop
	// queries. Each one builds its own IN (...) placeholder list at
	// runtime; abstracting that would just hide the actual query logic.
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Compile-time check.
var _ InventoryQueryStore = (*store.DB)(nil)
