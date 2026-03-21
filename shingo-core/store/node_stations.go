package store

import "fmt"

func (db *DB) AssignNodeToStation(nodeID int64, stationID string) error {
	_, err := db.Exec(`INSERT INTO node_stations (node_id, station_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, nodeID, stationID)
	return err
}

func (db *DB) UnassignNodeFromStation(nodeID int64, stationID string) error {
	_, err := db.Exec(`DELETE FROM node_stations WHERE node_id=$1 AND station_id=$2`, nodeID, stationID)
	return err
}

func (db *DB) ListStationsForNode(nodeID int64) ([]string, error) {
	rows, err := db.Query(`SELECT station_id FROM node_stations WHERE node_id=$1 ORDER BY station_id`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stations []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		stations = append(stations, s)
	}
	return stations, rows.Err()
}

// ListNodesForStation returns nodes directly assigned to a station.
// Includes top-level nodes and non-synthetic direct children of NGRP node groups.
func (db *DB) ListNodesForStation(stationID string) ([]*Node, error) {
	rows, err := db.Query(fmt.Sprintf(`
		SELECT %s %s
		WHERE n.id IN (
			SELECT ns.node_id FROM node_stations ns WHERE ns.station_id = $1
		)
		AND (n.parent_id IS NULL
		     OR (n.is_synthetic = false AND EXISTS (
		         SELECT 1 FROM nodes p
		         JOIN node_types pt ON pt.id = p.node_type_id
		         WHERE p.id = n.parent_id AND pt.code = 'NGRP'
		     )))
		ORDER BY n.name`, nodeSelectCols, nodeFromClause), stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetEffectiveStations returns stations for a node based on its station_mode property:
//   - "all": no restrictions (returns nil)
//   - "specific": returns directly assigned stations
//   - "" / "inherit": walks parent chain until a non-empty set is found
func (db *DB) GetEffectiveStations(nodeID int64) ([]string, error) {
	mode := db.GetNodeProperty(nodeID, "station_mode")
	switch mode {
	case "all":
		return nil, nil
	case "none":
		return []string{}, nil // empty = no stations permitted
	case "specific":
		return db.ListStationsForNode(nodeID)
	default: // "" or "inherit"
		rows, err := db.Query(`
			WITH RECURSIVE ancestors AS (
				SELECT id, parent_id, 0 AS depth FROM nodes WHERE id = $1
				UNION ALL
				SELECT n.id, n.parent_id, a.depth + 1 FROM nodes n
				JOIN ancestors a ON n.id = a.parent_id
			)
			SELECT ns.station_id FROM node_stations ns
			WHERE ns.node_id = (
				SELECT a.id FROM ancestors a
				WHERE EXISTS (SELECT 1 FROM node_stations ns2 WHERE ns2.node_id = a.id)
				ORDER BY a.depth ASC
				LIMIT 1
			)
			ORDER BY ns.station_id
		`, nodeID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var stations []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return nil, err
			}
			stations = append(stations, s)
		}
		return stations, rows.Err()
	}
}

// SetNodeStations replaces all station assignments for a node.
func (db *DB) SetNodeStations(nodeID int64, stationIDs []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM node_stations WHERE node_id=$1`, nodeID); err != nil {
		return err
	}
	for _, sid := range stationIDs {
		if _, err := tx.Exec(`INSERT INTO node_stations (node_id, station_id) VALUES ($1, $2)`, nodeID, sid); err != nil {
			return err
		}
	}
	return tx.Commit()
}
