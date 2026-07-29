package store

// Phase 5 delegate file: edge-registry CRUD lives in store/registry/.
// This file preserves the *store.DB method surface so external callers
// don't need to change.

import (
	"time"

	"shingocore/store/registry"
)

// EnrollEdge mints a station: a Core-owned uid plus an operator-owned display
// name. INSERT-only — see registry.Enroll for why minting and updating are
// separate functions rather than one upsert.
func (db *DB) EnrollEdge(uid, displayName, stationID string) (*registry.Edge, error) {
	return registry.Enroll(db.DB, uid, displayName, stationID)
}

// RegisterEdge records that an enrolled station is up and reports a binding
// conflict (nil when there is none). UPDATE-only: an unknown uid returns
// registry.ErrUnknownStation and writes nothing.
//
// The conflict is returned rather than swallowed so the caller can put it
// somewhere the operator will see; it is already logged by registry.Register,
// so a caller that has nothing better to do with it may ignore it.
func (db *DB) RegisterEdge(uid, hostname, instance, version string) (*registry.Conflict, error) {
	return registry.Register(db.DB, uid, hostname, instance, version)
}

// RebindEdgeHostname moves a station's binding to a new machine and clears its
// conflict record. Reports whether the station existed.
func (db *DB) RebindEdgeHostname(uid, hostname string) (bool, error) {
	return registry.Rebind(db.DB, uid, hostname)
}

// RenameEdge sets the operator-facing display name. Safe by construction: it
// is not an identifier anywhere.
func (db *DB) RenameEdge(uid, displayName string) (bool, error) {
	return registry.SetDisplayName(db.DB, uid, displayName)
}

// UpdateHeartbeat marks an enrolled station alive. found=false means no
// enrolled station carries the uid, which is the signal to ask it to register.
func (db *DB) UpdateHeartbeat(uid string) (found bool, err error) {
	return registry.UpdateHeartbeat(db.DB, uid)
}

func (db *DB) ListEdges() ([]registry.Edge, error) { return registry.List(db.DB) }

// GetEdgeByUID is the read the hardware-replacement procedure runs: the new Pi
// needs the EXISTING uid, and Core is the only thing that still holds it.
func (db *DB) GetEdgeByUID(uid string) (*registry.Edge, error) {
	return registry.GetByUID(db.DB, uid)
}

func (db *DB) MarkStaleEdges(threshold time.Duration) ([]string, error) {
	return registry.MarkStale(db.DB, threshold)
}
