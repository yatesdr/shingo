package store

// Phase 5 delegate file: edge-registry CRUD lives in store/registry/.
// This file preserves the *store.DB method surface so external callers
// don't need to change.

import (
	"time"

	"shingocore/store/registry"
)

// RegisterEdge upserts an edge registration and reports a duplicate-identity
// conflict (nil when there is none). The conflict is returned rather than
// swallowed so the caller can put it somewhere the operator will see; it is
// already logged by registry.Register, so a caller that has nothing better to do
// with it may ignore it.
func (db *DB) RegisterEdge(stationID, hostname, version string, lineIDs []string) (*registry.Conflict, error) {
	return registry.Register(db.DB, stationID, hostname, version, lineIDs)
}

// RebindEdgeHostname moves a station's hostname binding to a new machine and
// clears its conflict record. Reports whether the station existed.
func (db *DB) RebindEdgeHostname(stationID, hostname string) (bool, error) {
	return registry.Rebind(db.DB, stationID, hostname)
}

func (db *DB) UpdateHeartbeat(stationID string) (isNew bool, err error) {
	return registry.UpdateHeartbeat(db.DB, stationID)
}

func (db *DB) ListEdges() ([]registry.Edge, error) { return registry.List(db.DB) }

func (db *DB) MarkStaleEdges(threshold time.Duration) ([]string, error) {
	return registry.MarkStale(db.DB, threshold)
}
