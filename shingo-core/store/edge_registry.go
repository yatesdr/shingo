package store

// Phase 5 delegate file: edge-registry CRUD lives in store/registry/.
// This file preserves the *store.DB method surface so external callers
// don't need to change.

import (
	"time"

	"shingocore/store/registry"
)

func (db *DB) RegisterEdge(stationID, hostname, version string, lineIDs []string) error {
	return registry.Register(db.DB, stationID, hostname, version, lineIDs)
}

func (db *DB) UpdateHeartbeat(stationID string) (isNew bool, err error) {
	return registry.UpdateHeartbeat(db.DB, stationID)
}

func (db *DB) ListEdges() ([]registry.Edge, error) { return registry.List(db.DB) }

func (db *DB) MarkStaleEdges(threshold time.Duration) ([]string, error) {
	return registry.MarkStale(db.DB, threshold)
}
