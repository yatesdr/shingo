package store

// Phase 5 delegate file: demand-registry persistence lives in
// store/demands/. This file preserves the *store.DB method surface so
// external callers don't need to change.

import "shingocore/store/demands"

// SyncDemandRegistry replaces a station's demand_registry rows
// atomically. The returned slice lists every (loader, payload) row
// whose replenish_uop_threshold value changed (including new and
// deleted rows); threshold_monitor consumes it to reset its in-memory
// debounce timers so the new threshold takes effect immediately.
// A nil/empty result means no threshold values shifted.
func (db *DB) SyncDemandRegistry(stationID string, entries []demands.RegistryEntry) ([]demands.RegistryChange, error) {
	return demands.SyncRegistry(db.DB, stationID, entries)
}

func (db *DB) LookupDemandRegistry(payloadCode string) ([]demands.RegistryEntry, error) {
	return demands.LookupRegistry(db.DB, payloadCode)
}

// LookupDemandThresholdsByPayload returns demand_registry entries
// for the given payload whose replenish_uop_threshold > 0 — the
// monitored set for C-push.
func (db *DB) LookupDemandThresholdsByPayload(payloadCode string) ([]demands.RegistryEntry, error) {
	return demands.LookupThresholdsByPayload(db.DB, payloadCode)
}

// ListDemandThresholds returns every active monitored binding
// across all payloads. Used by the threshold-monitor startup sweep.
func (db *DB) ListDemandThresholds() ([]demands.RegistryEntry, error) {
	return demands.ListThresholds(db.DB)
}

func (db *DB) ListDemandRegistry() ([]demands.RegistryEntry, error) {
	return demands.ListRegistry(db.DB)
}
