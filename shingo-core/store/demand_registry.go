package store

// Phase 5 delegate file: demand-registry persistence lives in
// store/demands/. This file preserves the *store.DB method surface so
// external callers don't need to change.

import "shingocore/store/demands"

func (db *DB) SyncDemandRegistry(stationID string, entries []demands.RegistryEntry) error {
	return demands.SyncRegistry(db.DB, stationID, entries)
}

func (db *DB) LookupDemandRegistry(payloadCode string) ([]demands.RegistryEntry, error) {
	return demands.LookupRegistry(db.DB, payloadCode)
}

func (db *DB) ListDemandRegistry() ([]demands.RegistryEntry, error) {
	return demands.ListRegistry(db.DB)
}
