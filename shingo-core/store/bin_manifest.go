package store

// Stage 2D delegate file: bin manifest Set/Confirm/Clear/Get + FIFO lookup
// live in store/bins/. SetBinManifestFromTemplate and FindStorageDestination
// are cross-aggregate composition methods that stay here.

import (
	"encoding/json"
	"fmt"

	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
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

// FindStorageDestination finds the best storage node for a bin. Prefers nodes
// with existing bins of the same payload code, then empty nodes. Cross-aggregate
// composition (bins ↔ nodes) so it stays at this level.
func (db *DB) FindStorageDestination(payloadCode string) (*nodes.Node, error) {
	// Try consolidation: storage nodes that already have bins of the same payload code
	row := db.QueryRow(fmt.Sprintf(`
		SELECT %s %s WHERE n.id = (
			SELECT sn.id
			FROM nodes sn
			JOIN bins match_b ON match_b.node_id = sn.id AND match_b.payload_code = $1
			WHERE sn.enabled = true AND sn.is_synthetic = false
			ORDER BY sn.name
			LIMIT 1
		)`, nodes.SelectCols, nodes.FromClause), payloadCode)
	n, err := nodes.ScanNode(row)
	if err == nil {
		return n, nil
	}

	// Fall back to any empty enabled physical node
	row = db.QueryRow(fmt.Sprintf(`
		SELECT %s %s WHERE n.id = (
			SELECT sn.id
			FROM nodes sn
			LEFT JOIN bins sb ON sb.node_id = sn.id
			WHERE sn.enabled = true AND sn.is_synthetic = false
			GROUP BY sn.id
			HAVING COUNT(sb.id) = 0
			ORDER BY sn.name
			LIMIT 1
		)`, nodes.SelectCols, nodes.FromClause))
	return nodes.ScanNode(row)
}

// SetBinManifestFromTemplate sets a bin's manifest from a payload template's
// manifest items. Cross-aggregate composition (payloads ↔ bins).
func (db *DB) SetBinManifestFromTemplate(binID int64, payloadCode string, uopCapacity int) error {
	p, err := payloads.GetByCode(db.DB, payloadCode)
	if err != nil {
		return fmt.Errorf("payload template %q: %w", payloadCode, err)
	}

	items, err := payloads.ListManifest(db.DB, p.ID)
	if err != nil {
		return fmt.Errorf("payload manifest: %w", err)
	}

	manifest := bins.Manifest{Items: make([]bins.ManifestEntry, len(items))}
	for i, item := range items {
		manifest.Items[i] = bins.ManifestEntry{
			CatID:    item.PartNumber,
			Quantity: item.Quantity,
		}
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	uop := uopCapacity
	if uop == 0 {
		uop = p.UOPCapacity
	}

	return bins.SetManifest(db.DB, binID, string(manifestJSON), payloadCode, uop)
}
