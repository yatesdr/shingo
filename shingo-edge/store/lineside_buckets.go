package store

// Delegate file: lineside pile persistence lives in store/lineside/. This
// file keeps the *store.DB method surface so callers outside the store do not
// import the sub-package for the common calls.

import "shingoedge/store/lineside"

// Pile states re-exported for callers outside the store package.
const (
	LinesideStateActive   = lineside.StateActive
	LinesideStateStranded = lineside.StateStranded
)

// GetLinesideBucket returns one pile by id.
func (db *DB) GetLinesideBucket(id int64) (*lineside.Bucket, error) {
	return lineside.GetByID(db.DB, id)
}

// ListLinesideBuckets returns every pile on a node, active-first.
func (db *DB) ListLinesideBuckets(nodeID int64) ([]lineside.Bucket, error) {
	return lineside.ListForNode(db.DB, nodeID)
}

// CaptureLinesideBucket adds qty to the node's active pile of the payload
// (creating it) and returns the pile's new qty. Never touches a stranded row.
func (db *DB) CaptureLinesideBucket(nodeID int64, payloadCode string, qty int) (int, error) {
	return lineside.Capture(db.DB, nodeID, payloadCode, qty)
}

// DrainLinesideBucket decrements the node's active pile of the payload by up
// to delta and returns what it took; the caller passes the remainder to the
// node's bin count.
func (db *DB) DrainLinesideBucket(nodeID int64, payloadCode string, delta int) (int, error) {
	return lineside.Drain(db.DB, nodeID, payloadCode, delta)
}

// DeleteLinesideBucket removes one pile by id (the admin Clear).
func (db *DB) DeleteLinesideBucket(id int64) error {
	return lineside.DeleteByID(db.DB, id)
}

// StrandLinesidePiles folds every active pile at the process's nodes into
// its stranded row, in one transaction. See lineside.StrandProcess.
func (db *DB) StrandLinesidePiles(processID int64) ([]lineside.Stranded, error) {
	return lineside.StrandProcess(db.DB, processID)
}

// ListLinesidePileKeys returns the Key of every pile row (the boot resend).
func (db *DB) ListLinesidePileKeys() ([]lineside.Key, error) {
	return lineside.ListKeys(db.DB)
}

// ListLinesidePileKeysForProcess returns the Key of every pile row at the
// process's nodes.
func (db *DB) ListLinesidePileKeysForProcess(processID int64) ([]lineside.Key, error) {
	return lineside.ListKeysForProcess(db.DB, processID)
}

// LinesidePileLevel returns the summed qty Core mirrors for one
// (core node, payload, state). See lineside.Level.
func (db *DB) LinesidePileLevel(coreNodeName, payloadCode, state string) (int, error) {
	return lineside.Level(db.DB, coreNodeName, payloadCode, state)
}
