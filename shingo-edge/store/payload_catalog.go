package store

// Phase 5b delegate file: payload-catalog CRUD lives in
// store/catalog/. (Phase 6.0c renamed the sub-package from payloads/
// to catalog/ — the package holds the catalog synced from core, not
// payload definitions themselves; the rename disambiguates from
// core's payloads/. The on-disk table name `payload_catalog` is
// unchanged.) This file preserves the *store.DB method surface so
// external callers do not need to change.

import "shingoedge/store/catalog"

// UpsertPayloadCatalog inserts or updates a payload_catalog row.
func (db *DB) UpsertPayloadCatalog(entry *catalog.CatalogEntry) error {
	return catalog.UpsertCatalog(db.DB, entry)
}

// ListPayloadCatalog returns every payload_catalog row sorted by name.
func (db *DB) ListPayloadCatalog() ([]*catalog.CatalogEntry, error) {
	return catalog.ListCatalog(db.DB)
}

// GetPayloadCatalogByCode returns a single payload_catalog row by code.
func (db *DB) GetPayloadCatalogByCode(code string) (*catalog.CatalogEntry, error) {
	return catalog.GetCatalogByCode(db.DB, code)
}

// DeleteStalePayloadCatalogEntries removes local catalog entries whose
// IDs are not in activeIDs. If activeIDs is empty, no entries are
// removed.
func (db *DB) DeleteStalePayloadCatalogEntries(activeIDs []int64) error {
	return catalog.DeleteStaleCatalogEntries(db.DB, activeIDs)
}
