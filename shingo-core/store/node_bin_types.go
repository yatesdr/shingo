package store

import "fmt"

// SetNodeBinTypes replaces all bin type assignments for a node.
func (db *DB) SetNodeBinTypes(nodeID int64, binTypeIDs []int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM node_bin_types WHERE node_id=$1`, nodeID); err != nil {
		return err
	}
	for _, btID := range binTypeIDs {
		if _, err := tx.Exec(`INSERT INTO node_bin_types (node_id, bin_type_id) VALUES ($1, $2)`, nodeID, btID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListBinTypesForNode returns the directly assigned bin types for a node.
func (db *DB) ListBinTypesForNode(nodeID int64) ([]*BinType, error) {
	rows, err := db.Query(fmt.Sprintf(`
		SELECT %s FROM bin_types
		WHERE id IN (SELECT bin_type_id FROM node_bin_types WHERE node_id=$1)
		ORDER BY code`, binTypeSelectCols), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBinTypes(rows)
}

// GetEffectiveBinTypes returns bin types for a node based on its bin_type_mode property:
//   - "all": no restrictions (returns nil)
//   - "specific": returns directly assigned bin types
//   - "" / "inherit": walks parent chain until a non-empty set is found
func (db *DB) GetEffectiveBinTypes(nodeID int64) ([]*BinType, error) {
	mode := db.GetNodeProperty(nodeID, "bin_type_mode")
	switch mode {
	case "all":
		return nil, nil
	case "specific":
		return db.ListBinTypesForNode(nodeID)
	default: // "" or "inherit"
		rows, err := db.Query(fmt.Sprintf(`
			WITH RECURSIVE ancestors AS (
				SELECT id, parent_id, 0 AS depth FROM nodes WHERE id = $1
				UNION ALL
				SELECT n.id, n.parent_id, a.depth + 1 FROM nodes n
				JOIN ancestors a ON n.id = a.parent_id
			)
			SELECT %s FROM bin_types
			WHERE id IN (
				SELECT nbt.bin_type_id FROM node_bin_types nbt
				WHERE nbt.node_id = (
					SELECT a.id FROM ancestors a
					WHERE EXISTS (SELECT 1 FROM node_bin_types nbt2 WHERE nbt2.node_id = a.id)
					ORDER BY a.depth ASC
					LIMIT 1
				)
			)
			ORDER BY code`, binTypeSelectCols), nodeID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanBinTypes(rows)
	}
}
