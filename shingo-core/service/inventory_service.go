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
