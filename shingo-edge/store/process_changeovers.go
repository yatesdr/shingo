package store

import (
	"database/sql"
	"time"
)

type ProcessChangeover struct {
	ID          int64      `json:"id"`
	ProcessID   int64      `json:"process_id"`
	FromStyleID *int64     `json:"from_style_id,omitempty"`
	ToStyleID   int64      `json:"to_style_id"`
	State       string     `json:"state"`
	CalledBy    string     `json:"called_by"`
	Notes       string     `json:"notes"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`

	ProcessName   string `json:"process_name"`
	FromStyleName string `json:"from_style_name"`
	ToStyleName   string `json:"to_style_name"`
}

type ChangeoverStationTask struct {
	ID                  int64     `json:"id"`
	ProcessChangeoverID int64     `json:"process_changeover_id"`
	OperatorStationID   int64     `json:"operator_station_id"`
	State               string    `json:"state"`
	UpdatedAt           time.Time `json:"updated_at"`
	StationName         string    `json:"station_name"`
}

type ChangeoverNodeTask struct {
	ID                        int64     `json:"id"`
	ProcessChangeoverID       int64     `json:"process_changeover_id"`
	ProcessNodeID             int64     `json:"process_node_id"`
	FromClaimID               *int64    `json:"from_claim_id,omitempty"`
	ToClaimID                 *int64    `json:"to_claim_id,omitempty"`
	Situation                 string    `json:"situation"`
	State                     string    `json:"state"`
	NextMaterialOrderID       *int64    `json:"next_material_order_id,omitempty"`
	OldMaterialReleaseOrderID *int64    `json:"old_material_release_order_id,omitempty"`
	UpdatedAt                 time.Time `json:"updated_at"`
	NodeName                  string    `json:"node_name"`
}

func scanProcessChangeover(scanner interface{ Scan(...interface{}) error }) (ProcessChangeover, error) {
	var c ProcessChangeover
	var startedAt, completedAt, updatedAt string
	err := scanner.Scan(&c.ID, &c.ProcessID, &c.FromStyleID, &c.ToStyleID, &c.State, &c.CalledBy, &c.Notes,
		&startedAt, &completedAt, &updatedAt, &c.ProcessName, &c.FromStyleName, &c.ToStyleName)
	if err != nil {
		return c, err
	}
	c.StartedAt = scanTime(startedAt)
	if completedAt != "" {
		t := scanTime(completedAt)
		c.CompletedAt = &t
	}
	c.UpdatedAt = scanTime(updatedAt)
	return c, nil
}

func (db *DB) ListProcessChangeovers(processID int64) ([]ProcessChangeover, error) {
	rows, err := db.Query(`SELECT c.id, c.process_id, c.from_style_id, c.to_style_id, c.state, c.called_by, c.notes,
		c.started_at, COALESCE(c.completed_at, ''), c.updated_at,
		COALESCE(p.name, ''), COALESCE(fs.name, ''), COALESCE(ts.name, '')
		FROM process_changeovers c
		LEFT JOIN processes p ON p.id = c.process_id
		LEFT JOIN styles fs ON fs.id = c.from_style_id
		LEFT JOIN styles ts ON ts.id = c.to_style_id
		WHERE c.process_id = ?
		ORDER BY c.started_at DESC`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProcessChangeover
	for rows.Next() {
		c, err := scanProcessChangeover(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (db *DB) GetActiveProcessChangeover(processID int64) (*ProcessChangeover, error) {
	c, err := scanProcessChangeover(db.QueryRow(`SELECT c.id, c.process_id, c.from_style_id, c.to_style_id, c.state, c.called_by, c.notes,
		c.started_at, COALESCE(c.completed_at, ''), c.updated_at,
		COALESCE(p.name, ''), COALESCE(fs.name, ''), COALESCE(ts.name, '')
		FROM process_changeovers c
		LEFT JOIN processes p ON p.id = c.process_id
		LEFT JOIN styles fs ON fs.id = c.from_style_id
		LEFT JOIN styles ts ON ts.id = c.to_style_id
		WHERE c.process_id = ? AND c.state NOT IN ('completed', 'cancelled')
		ORDER BY c.started_at DESC LIMIT 1`, processID))
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (db *DB) CreateProcessChangeover(processID int64, fromStyleID *int64, toStyleID int64, calledBy, notes string) (int64, error) {
	res, err := db.Exec(`INSERT INTO process_changeovers (process_id, from_style_id, to_style_id, state, called_by, notes)
		VALUES (?, ?, ?, 'active', ?, ?)`, processID, fromStyleID, toStyleID, calledBy, notes)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateProcessChangeoverState(id int64, state string) error {
	completedAt := sql.NullString{}
	if state == "completed" || state == "cancelled" {
		completedAt = sql.NullString{Valid: true, String: time.Now().UTC().Format("2006-01-02 15:04:05")}
	}
	_, err := db.Exec(`UPDATE process_changeovers SET state=?, completed_at=CASE WHEN ? != '' THEN ? ELSE completed_at END, updated_at=datetime('now') WHERE id=?`,
		state, completedAt.String, completedAt.String, id)
	return err
}

func (db *DB) CreateChangeoverStationTask(changeoverID, stationID int64) (int64, error) {
	res, err := db.Exec(`INSERT INTO changeover_station_tasks (
		process_changeover_id, operator_station_id
	) VALUES (?, ?)`, changeoverID, stationID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) ListChangeoverStationTasks(changeoverID int64) ([]ChangeoverStationTask, error) {
	rows, err := db.Query(`SELECT t.id, t.process_changeover_id, t.operator_station_id, t.state,
		t.updated_at, COALESCE(s.name, '')
		FROM changeover_station_tasks t
		LEFT JOIN operator_stations s ON s.id = t.operator_station_id
		WHERE t.process_changeover_id = ?
		ORDER BY s.sequence, s.name`, changeoverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChangeoverStationTask
	for rows.Next() {
		var t ChangeoverStationTask
		var updatedAt string
		if err := rows.Scan(&t.ID, &t.ProcessChangeoverID, &t.OperatorStationID, &t.State,
			&updatedAt, &t.StationName); err != nil {
			return nil, err
		}
		t.UpdatedAt = scanTime(updatedAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (db *DB) UpdateChangeoverStationTaskState(id int64, state string) error {
	_, err := db.Exec(`UPDATE changeover_station_tasks SET state=?, updated_at=datetime('now') WHERE id=?`,
		state, id)
	return err
}

func (db *DB) GetChangeoverStationTaskByStation(changeoverID, stationID int64) (*ChangeoverStationTask, error) {
	row := db.QueryRow(`SELECT t.id, t.process_changeover_id, t.operator_station_id, t.state,
		t.updated_at, COALESCE(s.name, '')
		FROM changeover_station_tasks t
		LEFT JOIN operator_stations s ON s.id = t.operator_station_id
		WHERE t.process_changeover_id = ? AND t.operator_station_id = ? LIMIT 1`, changeoverID, stationID)
	var t ChangeoverStationTask
	var updatedAt string
	if err := row.Scan(&t.ID, &t.ProcessChangeoverID, &t.OperatorStationID, &t.State,
		&updatedAt, &t.StationName); err != nil {
		return nil, err
	}
	t.UpdatedAt = scanTime(updatedAt)
	return &t, nil
}

func (db *DB) CreateChangeoverNodeTask(changeoverID int64, processNodeID int64, fromClaimID, toClaimID *int64, situation, state string) (int64, error) {
	res, err := db.Exec(`INSERT INTO changeover_node_tasks (
		process_changeover_id, process_node_id, from_claim_id, to_claim_id, situation, state
	) VALUES (?, ?, ?, ?, ?, ?)`, changeoverID, processNodeID, fromClaimID, toClaimID, situation, state)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanChangeoverNodeTask(scanner interface{ Scan(...interface{}) error }) (ChangeoverNodeTask, error) {
	var t ChangeoverNodeTask
	var updatedAt string
	if err := scanner.Scan(&t.ID, &t.ProcessChangeoverID, &t.ProcessNodeID,
		&t.FromClaimID, &t.ToClaimID, &t.Situation, &t.State,
		&t.NextMaterialOrderID, &t.OldMaterialReleaseOrderID,
		&updatedAt, &t.NodeName); err != nil {
		return t, err
	}
	t.UpdatedAt = scanTime(updatedAt)
	return t, nil
}

func (db *DB) ListChangeoverNodeTasks(changeoverID int64) ([]ChangeoverNodeTask, error) {
	rows, err := db.Query(`SELECT t.id, t.process_changeover_id, t.process_node_id,
		t.from_claim_id, t.to_claim_id, t.situation, t.state,
		t.next_material_order_id, t.old_material_release_order_id,
		t.updated_at, COALESCE(n.name, '')
		FROM changeover_node_tasks t
		LEFT JOIN process_nodes n ON n.id = t.process_node_id
		WHERE t.process_changeover_id=? ORDER BY n.sequence, n.name`, changeoverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChangeoverNodeTask
	for rows.Next() {
		t, err := scanChangeoverNodeTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (db *DB) ListChangeoverNodeTasksByStation(changeoverID, stationID int64) ([]ChangeoverNodeTask, error) {
	rows, err := db.Query(`SELECT t.id, t.process_changeover_id, t.process_node_id,
		t.from_claim_id, t.to_claim_id, t.situation, t.state,
		t.next_material_order_id, t.old_material_release_order_id,
		t.updated_at, COALESCE(n.name, '')
		FROM changeover_node_tasks t
		LEFT JOIN process_nodes n ON n.id = t.process_node_id
		WHERE t.process_changeover_id=? AND n.operator_station_id=? ORDER BY n.sequence, n.name`, changeoverID, stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChangeoverNodeTask
	for rows.Next() {
		t, err := scanChangeoverNodeTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (db *DB) GetChangeoverNodeTaskByNode(changeoverID, processNodeID int64) (*ChangeoverNodeTask, error) {
	t, err := scanChangeoverNodeTask(db.QueryRow(`SELECT t.id, t.process_changeover_id, t.process_node_id,
		t.from_claim_id, t.to_claim_id, t.situation, t.state,
		t.next_material_order_id, t.old_material_release_order_id,
		t.updated_at, COALESCE(n.name, '')
		FROM changeover_node_tasks t
		LEFT JOIN process_nodes n ON n.id = t.process_node_id
		WHERE t.process_changeover_id=? AND t.process_node_id=? LIMIT 1`, changeoverID, processNodeID))
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (db *DB) UpdateChangeoverNodeTaskState(id int64, state string) error {
	_, err := db.Exec(`UPDATE changeover_node_tasks SET state=?, updated_at=datetime('now') WHERE id=?`, state, id)
	return err
}

func (db *DB) LinkChangeoverNodeOrders(id int64, nextOrderID, oldOrderID *int64) error {
	_, err := db.Exec(`UPDATE changeover_node_tasks SET next_material_order_id=COALESCE(?, next_material_order_id),
		old_material_release_order_id=COALESCE(?, old_material_release_order_id), updated_at=datetime('now')
		WHERE id=?`, nextOrderID, oldOrderID, id)
	return err
}
