package service

import (
	"shingoedge/store"
	"shingoedge/store/catalog"
)

// CatalogService owns the payload-catalog aggregate. The catalog is a
// read-through cache of payload definitions synced from core (which
// holds the canonical templates). Edge keeps a local copy so HMI
// renders don't have to round-trip to core for every lookup.
//
// Phase 6.2′ extracted this from named methods on *engine.Engine.
// Only ListCatalog is exposed via EngineAccess — the upsert / get-
// by-code / prune operations are called from engine sync loops via
// *store.DB directly.
type CatalogService struct {
	db *store.DB
}

// NewCatalogService constructs a CatalogService wrapping the shared
// *store.DB.
func NewCatalogService(db *store.DB) *CatalogService {
	return &CatalogService{db: db}
}

// List returns every payload_catalog row sorted by name.
func (s *CatalogService) List() ([]*catalog.CatalogEntry, error) {
	return s.db.ListPayloadCatalog()
}

// Upsert inserts or updates a payload_catalog row. Called by the
// catalog sync loop when core publishes an updated payload set.
func (s *CatalogService) Upsert(entry *catalog.CatalogEntry) error {
	return s.db.UpsertPayloadCatalog(entry)
}

// GetByCode returns a single payload_catalog row by code.
func (s *CatalogService) GetByCode(code string) (*catalog.CatalogEntry, error) {
	return s.db.GetPayloadCatalogByCode(code)
}

// DeleteStaleEntries removes catalog rows whose IDs are not in
// activeIDs. Empty activeIDs is a no-op (safety: never delete every
// row).
func (s *CatalogService) DeleteStaleEntries(activeIDs []int64) error {
	return s.db.DeleteStalePayloadCatalogEntries(activeIDs)
}
