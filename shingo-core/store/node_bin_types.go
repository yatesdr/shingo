package store

// Stage 2D delegate file: SetNodeBinTypes and ListBinTypesForNode live in
// store/bins/ (as the junction-table-driven queries return *bins.BinType).
// GetEffectiveBinTypes is a cross-aggregate composition method: it reads a
// node property to pick the resolution mode, then consults the bins aggregate.

import (
	"shingocore/store/bins"
)

// SetNodeBinTypes replaces all bin type assignments for a node.
func (db *DB) SetNodeBinTypes(nodeID int64, binTypeIDs []int64) error {
	return bins.SetNodeTypes(db.DB, nodeID, binTypeIDs)
}

// ListBinTypesForNode returns the directly assigned bin types for a node.
func (db *DB) ListBinTypesForNode(nodeID int64) ([]*bins.BinType, error) {
	return bins.ListTypesForNode(db.DB, nodeID)
}

// GetEffectiveBinTypes returns bin types for a node based on its
// bin_type_mode property:
//   - "all": no restrictions (returns nil)
//   - "specific": returns directly assigned bin types
//   - "" / "inherit": walks parent chain until a non-empty set is found
//
// Cross-aggregate because the mode is a node property and the result is a
// bin-types list.
func (db *DB) GetEffectiveBinTypes(nodeID int64) ([]*bins.BinType, error) {
	mode := db.GetNodeProperty(nodeID, "bin_type_mode")
	switch mode {
	case "all":
		return nil, nil
	case "specific":
		return bins.ListTypesForNode(db.DB, nodeID)
	default: // "" or "inherit"
		return bins.ListEffectiveTypesInherited(db.DB, nodeID)
	}
}
