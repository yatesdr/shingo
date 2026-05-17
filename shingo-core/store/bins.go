package store

// Stage 2D delegate file: bin CRUD/lock/stage/claim/count operations live in
// store/bins/. This file preserves the *store.DB method surface so external
// callers don't need to change. Cross-aggregate methods (ListOrdersByBin,
// UpdateOrderBinID, FindStorageDestination) live at the outer store/ level
// in their own files. SetBinManifestFromTemplate retired Item 19 — see
// service.BinManifestService.SetFromTemplate for the audit-bearing path.

import (
	"time"

	"shingocore/domain"
	"shingocore/store/bins"
)

func (db *DB) CreateBin(b *bins.Bin) error                     { return bins.Create(db.DB, b) }
func (db *DB) UpdateBin(b *bins.Bin) error                     { return bins.Update(db.DB, b) }
func (db *DB) DeleteBin(id int64) error                   { return bins.Delete(db.DB, id) }
func (db *DB) GetBin(id int64) (*bins.Bin, error)              { return bins.Get(db.DB, id) }
func (db *DB) GetBinByLabel(label string) (*bins.Bin, error)   { return bins.GetByLabel(db.DB, label) }
func (db *DB) ListBins() ([]*bins.Bin, error)                  { return bins.List(db.DB) }
func (db *DB) ListBinsByNode(nodeID int64) ([]*bins.Bin, error) { return bins.ListByNode(db.DB, nodeID) }
func (db *DB) ListBinsByClaim(orderID int64) ([]*bins.Bin, error) {
	return bins.ListByClaim(db.DB, orderID)
}
func (db *DB) ListAnomalousTransitBins() ([]*bins.Bin, error) {
	return bins.ListAnomalousTransitBins(db.DB)
}
func (db *DB) CountBinsByNode(nodeID int64) (int, error)  { return bins.CountByNode(db.DB, nodeID) }

// CountBinsByAllNodes returns a map of node_id -> bin count for all nodes
// that have bins.
func (db *DB) CountBinsByAllNodes() (map[int64]int, error) { return bins.CountByAllNodes(db.DB) }

// NodeTileStates returns per-node tile rendering state for all nodes that
// have bins.
func (db *DB) NodeTileStates() (map[int64]bins.NodeTileState, error) { return bins.NodeTileStates(db.DB) }

// MoveBin moves a bin to a new node. Returns an error if the bin is already
// at the destination.
func (db *DB) MoveBin(binID, toNodeID int64) error { return bins.Move(db.DB, binID, toNodeID) }

// ListAvailableBins returns bins with no manifest.
func (db *DB) ListAvailableBins() ([]*bins.Bin, error) { return bins.ListAvailable(db.DB) }

// ClaimBin marks a bin as claimed by an order.
func (db *DB) ClaimBin(binID, orderID int64) error { return bins.Claim(db.DB, binID, orderID) }

// UnclaimBin releases a bin from an order claim.
func (db *DB) UnclaimBin(binID int64) error { return bins.Unclaim(db.DB, binID) }

// UnclaimOrderBins releases all bins claimed by a specific order.
func (db *DB) UnclaimOrderBins(orderID int64) { bins.UnclaimByOrder(db.DB, orderID) }

// FindEmptyCompatibleBin finds an unclaimed, available bin compatible with
// the given payload code, preferring the given zone. excludeNodeID > 0
// skips bins at that node (pass destination to avoid same-node retrieve).
func (db *DB) FindEmptyCompatibleBin(payloadCode, preferZone string, excludeNodeID int64) (*bins.Bin, error) {
	return bins.FindEmptyCompatible(db.DB, payloadCode, preferZone, excludeNodeID)
}

// FindEmptyCompatibleBinInGroup is FindEmptyCompatibleBin scoped to descendants
// of a synthetic group node. See bins.FindEmptyCompatibleInGroup for the full
// rationale. Used by planRetrieveEmpty's source-group branch.
func (db *DB) FindEmptyCompatibleBinInGroup(payloadCode string, groupNodeID, excludeNodeID int64) (*bins.Bin, error) {
	return bins.FindEmptyCompatibleInGroup(db.DB, payloadCode, groupNodeID, excludeNodeID)
}

// UpdateBinStatus sets the status on a bin.
func (db *DB) UpdateBinStatus(binID int64, status domain.BinStatus) error {
	return bins.UpdateStatus(db.DB, binID, status)
}

// StageBin marks a bin as staged with expiry tracking.
func (db *DB) StageBin(binID int64, expiresAt *time.Time) error {
	return bins.Stage(db.DB, binID, expiresAt)
}

// ReleaseStagedBin clears the staged status on a single bin.
func (db *DB) ReleaseStagedBin(binID int64) error { return bins.ReleaseStaged(db.DB, binID) }

// ReleaseExpiredStagedBins releases staged bins whose expiry has passed.
func (db *DB) ReleaseExpiredStagedBins() (int, error) { return bins.ReleaseExpiredStaged(db.DB) }

// LockBin prevents automated claiming/movement of a bin.
func (db *DB) LockBin(binID int64, actor string) error { return bins.Lock(db.DB, binID, actor) }

// UnlockBin clears the lock on a bin.
func (db *DB) UnlockBin(binID int64) error { return bins.Unlock(db.DB, binID) }

// MoveBinToTransit moves a bin to the synthetic _TRANSIT node. Idempotent.
func (db *DB) MoveBinToTransit(binID, transitNodeID int64) error {
	return bins.MoveToTransit(db.DB, binID, transitNodeID)
}

// MarkBinAnomaly stamps bins.anomaly_at = NOW().
func (db *DB) MarkBinAnomaly(binID int64) error { return bins.MarkAnomaly(db.DB, binID) }

// ClearBinAnomaly clears bins.anomaly_at.
func (db *DB) ClearBinAnomaly(binID int64) error { return bins.ClearAnomaly(db.DB, binID) }

// RecoverBinToNode moves a bin to toNodeID and clears anomaly_at atomically.
func (db *DB) RecoverBinToNode(binID, toNodeID int64) error {
	return bins.RecoverToNode(db.DB, binID, toNodeID)
}

// RecordBinCount updates UOP and records the count timestamp.
func (db *DB) RecordBinCount(binID int64, actualUOP int, actor string) error {
	return bins.RecordCount(db.DB, binID, actualUOP, actor)
}

// UnconfirmBinManifest resets the manifest confirmation flag.
func (db *DB) UnconfirmBinManifest(binID int64) error { return bins.UnconfirmManifest(db.DB, binID) }

// BinHasNotes returns a map indicating which bins have audit log entries.
func (db *DB) BinHasNotes(binIDs []int64) (map[int64]bool, error) {
	return bins.HasNotes(db.DB, binIDs)
}
