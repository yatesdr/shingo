package store

import (
	"database/sql"
	"strings"
	"time"
)

type ProcessNode struct {
	ID                int64     `json:"id"`
	ProcessID         int64     `json:"process_id"`
	OperatorStationID *int64    `json:"operator_station_id,omitempty"`
	CoreNodeName      string    `json:"core_node_name"`
	Code              string    `json:"code"`
	Name              string    `json:"name"`
	Sequence          int       `json:"sequence"`
	Enabled           bool      `json:"enabled"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	StationName       string    `json:"station_name"`
	ProcessName       string    `json:"process_name"`
}

type ProcessNodeInput struct {
	ProcessID         int64  `json:"process_id"`
	OperatorStationID *int64 `json:"operator_station_id,omitempty"`
	CoreNodeName      string `json:"core_node_name"`
	Code              string `json:"code"`
	Name              string `json:"name"`
	Sequence          int    `json:"sequence"`
	Enabled           bool   `json:"enabled"`
}

const processNodeSelect = `n.id, n.process_id, n.operator_station_id, n.core_node_name, n.code, n.name,
	n.sequence, n.enabled, n.created_at, n.updated_at, COALESCE(s.name, ''), COALESCE(p.name, '')`

const processNodeJoin = `FROM process_nodes n
	LEFT JOIN operator_stations s ON s.id = n.operator_station_id
	LEFT JOIN processes p ON p.id = n.process_id`

func scanProcessNode(scanner interface{ Scan(...interface{}) error }) (ProcessNode, error) {
	var n ProcessNode
	var createdAt, updatedAt string
	var stationID sql.NullInt64
	err := scanner.Scan(
		&n.ID, &n.ProcessID, &stationID, &n.CoreNodeName, &n.Code, &n.Name,
		&n.Sequence, &n.Enabled, &createdAt, &updatedAt, &n.StationName, &n.ProcessName,
	)
	if err != nil {
		return n, err
	}
	n.CreatedAt = scanTime(createdAt)
	n.UpdatedAt = scanTime(updatedAt)
	if stationID.Valid {
		id := stationID.Int64
		n.OperatorStationID = &id
	}
	return n, nil
}

func scanProcessNodes(rows rowScanner) ([]ProcessNode, error) {
	var out []ProcessNode
	for rows.Next() {
		n, err := scanProcessNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (db *DB) ListProcessNodes() ([]ProcessNode, error) {
	rows, err := db.Query(`SELECT ` + processNodeSelect + ` ` + processNodeJoin + ` ORDER BY n.process_id, n.sequence, n.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProcessNodes(rows)
}

func (db *DB) ListProcessNodesByProcess(processID int64) ([]ProcessNode, error) {
	rows, err := db.Query(`SELECT `+processNodeSelect+` `+processNodeJoin+` WHERE n.process_id=? ORDER BY n.sequence, n.name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProcessNodes(rows)
}

func (db *DB) ListProcessNodesByStation(stationID int64) ([]ProcessNode, error) {
	rows, err := db.Query(`SELECT `+processNodeSelect+` `+processNodeJoin+` WHERE n.operator_station_id=? ORDER BY n.sequence, n.name`, stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProcessNodes(rows)
}

func (db *DB) GetProcessNode(id int64) (*ProcessNode, error) {
	n, err := scanProcessNode(db.QueryRow(`SELECT `+processNodeSelect+` `+processNodeJoin+` WHERE n.id=?`, id))
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (db *DB) CreateProcessNode(in ProcessNodeInput) (int64, error) {
	in.CoreNodeName = strings.TrimSpace(in.CoreNodeName)
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = in.CoreNodeName
	}
	if in.OperatorStationID != nil && *in.OperatorStationID <= 0 {
		in.OperatorStationID = nil
	}
	if in.Code == "" {
		code, err := db.generateProcessNodeCode(in.ProcessID, in.CoreNodeName, in.Name)
		if err != nil {
			return 0, err
		}
		in.Code = code
	}
	if in.Sequence <= 0 {
		next, err := db.nextProcessNodeSequence(in.ProcessID)
		if err != nil {
			return 0, err
		}
		in.Sequence = next
	}
	res, err := db.Exec(`INSERT INTO process_nodes (
		process_id, operator_station_id, core_node_name, code, name, sequence, enabled
	) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		in.ProcessID, in.OperatorStationID, in.CoreNodeName, in.Code, in.Name, in.Sequence, in.Enabled,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateProcessNode(id int64, in ProcessNodeInput) error {
	existing, err := db.GetProcessNode(id)
	if err != nil {
		return err
	}
	in.CoreNodeName = strings.TrimSpace(in.CoreNodeName)
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = in.CoreNodeName
	}
	if in.OperatorStationID != nil && *in.OperatorStationID <= 0 {
		in.OperatorStationID = nil
	}
	if in.Code == "" {
		in.Code = existing.Code
	}
	if in.Sequence <= 0 {
		in.Sequence = existing.Sequence
	}
	_, err = db.Exec(`UPDATE process_nodes SET
		process_id=?, operator_station_id=?, core_node_name=?, code=?, name=?,
		sequence=?, enabled=?, updated_at=datetime('now')
		WHERE id=?`,
		in.ProcessID, in.OperatorStationID, in.CoreNodeName, in.Code, in.Name,
		in.Sequence, in.Enabled, id,
	)
	return err
}

func (db *DB) DeleteProcessNode(id int64) error {
	_, err := db.Exec(`DELETE FROM process_nodes WHERE id=?`, id)
	return err
}

func (db *DB) nextProcessNodeSequence(processID int64) (int, error) {
	var maxSeq sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(sequence) FROM process_nodes WHERE process_id=?`, processID).Scan(&maxSeq); err != nil {
		return 0, err
	}
	if !maxSeq.Valid {
		return 1, nil
	}
	return int(maxSeq.Int64) + 1, nil
}

func (db *DB) generateProcessNodeCode(processID int64, coreNodeName, name string) (string, error) {
	base := slugName(coreNodeName, "")
	if base == "" {
		base = slugName(name, "")
	}
	return generateUniqueCode(db, "process_nodes", "process_id", processID, base, "node")
}
