package service

import (
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
// bucket parts alongside the bins table. Phase: read-only view —
// the admin Delete path below is the Round-3 Obs 10 escape hatch
// for cleaning up Core-only orphans without a corresponding Edge row.
func (s *InventoryService) ListLinesideBuckets() ([]inventory.BucketRow, error) {
	return s.db.ListLinesideBuckets()
}

// DeleteLinesideBucket removes one Core-side bucket row by primary key
// along with its dedup row. Round-3 Obs 10: this is the admin
// recovery action for Core-only orphan buckets that pre-Obs-8
// cross-namespace bugs created. After Obs 8 lands, the applier
// rejects unresolvable core_node_name values, so new orphans
// shouldn't be createable; this stays as the cleanup hatch for the
// existing wedge plus any future operator-corrected drift.
func (s *InventoryService) DeleteLinesideBucket(id int64) (int, error) {
	return s.db.DeleteLinesideBucket(id)
}
