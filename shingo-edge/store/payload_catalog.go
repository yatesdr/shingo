package store

// Phase 5b delegate file: payload-catalog CRUD now lives in
// store/payloads/. This file preserves the *store.DB method surface so
// external callers do not need to change.

import "shingoedge/store/payloads"

// PayloadCatalogEntry is one payload_catalog row.
type PayloadCatalogEntry = payloads.CatalogEntry

// UpsertPayloadCatalog inserts or updates a payload_catalog row.
func (db *DB) UpsertPayloadCatalog(entry *PayloadCatalogEntry) error {
	return payloads.UpsertCatalog(db.DB, entry)
}

// ListPayloadCatalog returns every payload_catalog row sorted by name.
func (db *DB) ListPayloadCatalog() ([]*PayloadCatalogEntry, error) {
	return payloads.ListCatalog(db.DB)
}

// GetPayloadCatalogByCode returns a single payload_catalog row by code.
func (db *DB) GetPayloadCatalogByCode(code string) (*PayloadCatalogEntry, error) {
	return payloads.GetCatalogByCode(db.DB, code)
}

// DeleteStalePayloadCatalogEntries removes local catalog entries whose
// IDs are not in activeIDs. If activeIDs is empty, no entries are
// removed.
func (db *DB) DeleteStalePayloadCatalogEntries(activeIDs []int64) error {
	return payloads.DeleteStaleCatalogEntries(db.DB, activeIDs)
}
