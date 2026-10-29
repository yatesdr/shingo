package service

import (
	"time"

	"shingocore/store"
	"shingocore/store/audit"
	"shingocore/store/inventory"
)

// InventoryService exposes the aggregated inventory view used by the
// inventory page and API. Handlers call InventoryService instead of
// reaching through engine passthroughs to *store.DB.
//
// Absorbed from engine_db_methods.go as part of the Phase 3a closeout
// (PR 3a.6). Kept as a standalone service because inventory rollups
// cross bins / payloads / nodes and don't cleanly belong to any one
// entity-specific service.
//
// The db dependency is InventoryQueryStore (see inventory_query_store.go);
// *store.DB satisfies it structurally.
type InventoryService struct {
	db InventoryQueryStore
}

func NewInventoryService(db InventoryQueryStore) *InventoryService {
	return &InventoryService{db: db}
}

// List returns the aggregated inventory rows (bin type + payload +
// node counts).
func (s *InventoryService) List() ([]inventory.Row, error) {
	return s.db.ListInventory()
}

// ListLinesideBuckets returns every lineside_buckets row joined to
// node hierarchy so the operator-facing inventory page renders
// bucket parts alongside the bins table. Read-only: the rows are a
// mirror of the Edge's piles, and Core never writes a pile.
func (s *InventoryService) ListLinesideBuckets() ([]inventory.BucketRow, error) {
	return s.db.ListLinesideBuckets()
}

// OpenReportDivergences lists every open report_divergence episode, oldest
// first: a carrier or bucket where the Edge's lineside report and Core's
// replica still disagree (see lineside_divergence.go). Blank on a good day.
// Read by the /inventory ledger panels. Never nil on success, so the page's
// JSON carries [] rather than null.
func (s *InventoryService) OpenReportDivergences() ([]store.OpenReportDivergence, error) {
	out, err := s.db.ListOpenReportDivergences()
	if err == nil && out == nil {
		out = []store.OpenReportDivergence{}
	}
	return out, err
}

// HoldDropRunGap is the quiet that ends a run of hold drops: a line running
// full drops a part a stroke, strokes come under a minute apart, and a gap
// longer than this is the line stopped or a bin come and gone.
const HoldDropRunGap = 15 * time.Minute

// HoldDropRuns lists the produce holds the Edges dropped since a time as runs
// (audit.HoldDropRun), latest first: each a seat, part, reason and station
// with its drops, parts and span. Read by the /inventory ledger panels. Never
// nil on success.
func (s *InventoryService) HoldDropRuns(since time.Time, limit int) ([]audit.HoldDropRun, error) {
	return s.db.ListHoldDropRuns(since, HoldDropRunGap, limit)
}

// DeclaredFullStamps lists the bins a station declared full below their
// count since a time, newest first: each a release_declared_full row with its
// seat, bin, counted and declared counts and who declared it. Read by the
// /inventory ledger panels. Never nil on success.
func (s *InventoryService) DeclaredFullStamps(since time.Time, limit int) ([]audit.DeclaredFullStamp, error) {
	return s.db.ListDeclaredFullStamps(since, limit)
}
