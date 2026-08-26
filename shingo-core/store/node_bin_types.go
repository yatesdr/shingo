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

// NodesAcceptingNothing names every node configured to accept SPECIFIC bin
// types with none assigned — a node whose config says "accepts nothing".
//
// That combination is almost never what anybody meant, and it is invisible:
// dispatch's binTypeAllowed reads an empty list as UNRESTRICTED, so the config
// reads as permissive everywhere it is consulted, while the stranded-bin
// placement gate (the one reader that distinguishes the modes) refuses. One
// startup line is how a half-finished config gets finished; Springfield's
// SNF2 Lineside Market has been in this state since it was created.
func (db *DB) NodesAcceptingNothing() ([]string, error) {
	rows, err := db.DB.Query(`
		SELECT n.name FROM nodes n
		JOIN node_properties p ON p.node_id = n.id
		WHERE p.key = 'bin_type_mode' AND p.value = 'specific'
		  AND NOT EXISTS (SELECT 1 FROM node_bin_types t WHERE t.node_id = n.id)
		ORDER BY n.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
