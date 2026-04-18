package bins

import (
	"database/sql"
	"fmt"
)

// SetNodeTypes replaces all bin type assignments for a node.
// Runs as a single transaction.
func SetNodeTypes(db *sql.DB, nodeID int64, binTypeIDs []int64) error {
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

// ListTypesForNode returns the directly assigned bin types for a node.
func ListTypesForNode(db *sql.DB, nodeID int64) ([]*BinType, error) {
	rows, err := db.Query(fmt.Sprintf(`
		SELECT %s FROM bin_types
		WHERE id IN (SELECT bin_type_id FROM node_bin_types WHERE node_id=$1)
		ORDER BY code`, BinTypeSelectCols), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanBinTypes(rows)
}

// ListEffectiveTypesInherited returns bin types from the nearest ancestor
// node (inclusive of the node itself) that has any direct assignments.
// Walks the parent chain via a recursive CTE. Returns an empty slice if no
// ancestor has assignments.
//
// This is the "inherit" case of GetEffectiveBinTypes; the "all" and "specific"
// branches live at the outer store/ level because they depend on a node
// property lookup that spans aggregates.
func ListEffectiveTypesInherited(db *sql.DB, nodeID int64) ([]*BinType, error) {
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
		ORDER BY code`, BinTypeSelectCols), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanBinTypes(rows)
}
