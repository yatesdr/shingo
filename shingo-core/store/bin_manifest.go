package store

// Stage 2D delegate file: bin manifest Set/Confirm/Clear/Get + the FIFO source
// lookup (FindSourceBinFIFO) live at this level, delegating to store/bins/.
// (Item 19 retired SetBinManifestFromTemplate from this surface — production
// callers route through service.BinManifestService.SetFromTemplate for audit.)

import (
	"shingocore/store/bins"
)

// SetBinManifest populates a bin's contents from a payload template.
func (db *DB) SetBinManifest(binID int64, manifestJSON string, payloadCode string, uopRemaining int) error {
	return bins.SetManifest(db.DB, binID, manifestJSON, payloadCode, uopRemaining)
}

// ConfirmBinManifest marks a bin's manifest as confirmed by an operator.
func (db *DB) ConfirmBinManifest(binID int64, producedAt string) error {
	return bins.ConfirmManifest(db.DB, binID, producedAt)
}

// ClearBinManifest empties a bin's manifest.
func (db *DB) ClearBinManifest(binID int64) error { return bins.ClearManifest(db.DB, binID) }

// GetBinManifest fetches a bin and parses its manifest.
func (db *DB) GetBinManifest(binID int64) (*bins.Manifest, error) {
	return bins.GetManifest(db.DB, binID)
}

// FindSourceBinFIFO finds the best unclaimed bin at an enabled storage node
// matching the given payload code, using FIFO ordering. excludeNodeID > 0
// skips bins at that node (pass destination to avoid same-node retrieve).
func (db *DB) FindSourceBinFIFO(payloadCode string, excludeNodeID int64) (*bins.Bin, error) {
	return bins.FindSourceFIFO(db.DB, payloadCode, excludeNodeID)
}

// (Item 19 of the bin-as-truth refactor: SetBinManifestFromTemplate
// removed from the public *store.DB surface. Production callers
// route through BinManifestService.SetFromTemplate so the bin write
// audits via bin_uop_audit. The deleted function bypassed audit;
// post-Item-10 the audit timeline UI requires every manifest write
// to surface in bin_uop_audit. Test helpers that need an
// audit-bypass write path should call bins.SetManifest directly.)
