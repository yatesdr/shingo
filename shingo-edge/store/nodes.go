package store

// Node represents a physical factory location tied to a process.
type Node struct {
	ID          int64  `json:"id"`
	NodeID      string `json:"node_id"`
	LineID      int64  `json:"line_id"`
	Description string `json:"description"`
}

func (db *DB) ListNodes() ([]Node, error) {
	rows, err := db.Query("SELECT id, node_id, COALESCE(line_id, 0), description FROM nodes ORDER BY node_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.NodeID, &n.LineID, &n.Description); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func (db *DB) ListNodesByProcess(lineID int64) ([]Node, error) {
	rows, err := db.Query("SELECT id, node_id, COALESCE(line_id, 0), description FROM nodes WHERE line_id = ? ORDER BY node_id", lineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.NodeID, &n.LineID, &n.Description); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// ListKnownNodes returns distinct process nodes referenced by operator station nodes.
func (db *DB) ListKnownNodes() ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT delivery_node FROM op_station_nodes WHERE delivery_node != ''
		UNION SELECT DISTINCT staging_node FROM op_station_nodes WHERE staging_node != ''
		UNION SELECT DISTINCT outgoing_node FROM op_station_nodes WHERE outgoing_node != ''
		ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func (db *DB) CreateNode(nodeID string, lineID int64, description string) (int64, error) {
	res, err := db.Exec("INSERT INTO nodes (node_id, line_id, description) VALUES (?, ?, ?)", nodeID, lineID, description)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateNode(id int64, nodeID string, lineID int64, description string) error {
	_, err := db.Exec("UPDATE nodes SET node_id = ?, line_id = ?, description = ? WHERE id = ?", nodeID, lineID, description, id)
	return err
}

func (db *DB) DeleteNode(id int64) error {
	_, err := db.Exec("DELETE FROM nodes WHERE id = ?", id)
	return err
}
