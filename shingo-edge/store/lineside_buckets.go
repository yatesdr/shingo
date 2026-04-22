package store

// Delegate file: lineside bucket CRUD lives in store/lineside/. This
// file preserves the *store.DB method surface so external callers do
// not need to change.

import "shingoedge/store/lineside"

// LinesideBucket is one row of node_lineside_bucket.
type LinesideBucket = lineside.Bucket

// Bucket states re-exported for callers outside the store package.
const (
	LinesideStateActive   = lineside.StateActive
	LinesideStateInactive = lineside.StateInactive
)

// GetActiveLinesideBucket returns the active bucket for (node, style,
// part) or sql.ErrNoRows if none exists.
func (db *DB) GetActiveLinesideBucket(nodeID, styleID int64, partNumber string) (*LinesideBucket, error) {
	return lineside.GetActive(db.DB, nodeID, styleID, partNumber)
}

// FindLinesideBucket returns any bucket (active or inactive) for
// (node, style, part) or sql.ErrNoRows.
func (db *DB) FindLinesideBucket(nodeID, styleID int64, partNumber string) (*LinesideBucket, error) {
	return lineside.Find(db.DB, nodeID, styleID, partNumber)
}

// GetLinesideBucket returns one bucket by id.
func (db *DB) GetLinesideBucket(id int64) (*LinesideBucket, error) {
	return lineside.GetByID(db.DB, id)
}

// ListLinesideBuckets returns every bucket on a node, active-first.
func (db *DB) ListLinesideBuckets(nodeID int64) ([]LinesideBucket, error) {
	return lineside.ListForNode(db.DB, nodeID)
}

// ListActiveLinesideBuckets returns only the active buckets on a node.
func (db *DB) ListActiveLinesideBuckets(nodeID int64) ([]LinesideBucket, error) {
	return lineside.ListActiveForNode(db.DB, nodeID)
}

// ListInactiveLinesideBuckets returns only the stranded buckets on a
// node (the ones that render as stacked chips).
func (db *DB) ListInactiveLinesideBuckets(nodeID int64) ([]LinesideBucket, error) {
	return lineside.ListInactiveForNode(db.DB, nodeID)
}

// ListLinesideBucketsForPair returns every bucket keyed to a pair.
func (db *DB) ListLinesideBucketsForPair(pairKey string) ([]LinesideBucket, error) {
	return lineside.ListForPair(db.DB, pairKey)
}

// CaptureLinesideBucket records parts pulled to lineside for (node,
// style, part). Merges into an existing bucket when present (reactivating
// an inactive one) or creates a fresh active bucket otherwise. Zero qty
// is a no-op.
func (db *DB) CaptureLinesideBucket(nodeID int64, pairKey string, styleID int64, partNumber string, qty int) (*LinesideBucket, error) {
	return lineside.Capture(db.DB, nodeID, pairKey, styleID, partNumber, qty)
}

// DeactivateOtherLinesideStyles flips any other active buckets on the
// node (different style) to inactive. Call inside the same transaction
// as CaptureLinesideBucket.
func (db *DB) DeactivateOtherLinesideStyles(nodeID, keepStyleID int64) error {
	return lineside.DeactivateOtherStyles(db.DB, nodeID, keepStyleID)
}

// DrainLinesideBucket decrements the active bucket for (node, style,
// part) by up to delta. Returns the amount actually drained; caller
// passes the remainder to the node-level RemainingUOP decrement.
func (db *DB) DrainLinesideBucket(nodeID, styleID int64, partNumber string, delta int) (int, error) {
	return lineside.Drain(db.DB, nodeID, styleID, partNumber, delta)
}

// DeleteLinesideBucket removes a bucket by id (used by scrap/repack/
// recall actions).
func (db *DB) DeleteLinesideBucket(id int64) error {
	return lineside.Delete(db.DB, id)
}
