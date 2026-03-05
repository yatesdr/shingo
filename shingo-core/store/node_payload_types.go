package store

import "fmt"

func (db *DB) AssignPayloadStyleToNode(nodeID, styleID int64) error {
	_, err := db.Exec(db.Q(`INSERT INTO node_payload_styles (node_id, style_id) VALUES (?, ?) ON CONFLICT DO NOTHING`), nodeID, styleID)
	return err
}

func (db *DB) UnassignPayloadStyleFromNode(nodeID, styleID int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM node_payload_styles WHERE node_id=? AND style_id=?`), nodeID, styleID)
	return err
}

func (db *DB) ListPayloadStylesForNode(nodeID int64) ([]*PayloadStyle, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`
		SELECT %s FROM payload_styles
		WHERE id IN (SELECT style_id FROM node_payload_styles WHERE node_id=?)
		ORDER BY name`, payloadStyleSelectCols)), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloadStyles(rows)
}

func (db *DB) ListNodesForPayloadStyle(styleID int64) ([]*Node, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`
		SELECT %s %s
		WHERE n.id IN (SELECT nps.node_id FROM node_payload_styles nps WHERE nps.style_id=?)
		ORDER BY n.name`, nodeSelectCols, nodeFromClause)), styleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetEffectivePayloadStyles returns payload styles for a node, walking up the parent
// chain until a non-empty set is found. Returns nil (all styles) if no ancestor has styles.
func (db *DB) GetEffectivePayloadStyles(nodeID int64) ([]*PayloadStyle, error) {
	cur := nodeID
	for {
		styles, err := db.ListPayloadStylesForNode(cur)
		if err != nil {
			return nil, err
		}
		if len(styles) > 0 {
			return styles, nil
		}
		node, err := db.GetNode(cur)
		if err != nil {
			return nil, nil
		}
		if node.ParentID == nil {
			return nil, nil
		}
		cur = *node.ParentID
	}
}

// SetNodePayloadStyles replaces all payload style assignments for a node.
func (db *DB) SetNodePayloadStyles(nodeID int64, styleIDs []int64) error {
	if _, err := db.Exec(db.Q(`DELETE FROM node_payload_styles WHERE node_id=?`), nodeID); err != nil {
		return err
	}
	for _, sID := range styleIDs {
		if _, err := db.Exec(db.Q(`INSERT INTO node_payload_styles (node_id, style_id) VALUES (?, ?)`), nodeID, sID); err != nil {
			return err
		}
	}
	return nil
}
