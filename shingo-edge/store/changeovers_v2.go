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
	Phase       string     `json:"phase"`
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
	ID                  int64      `json:"id"`
	ProcessChangeoverID int64      `json:"process_changeover_id"`
	OperatorStationID   int64      `json:"operator_station_id"`
	State               string     `json:"state"`
	CurrentPhase        string     `json:"current_phase"`
	TransitionMode      string     `json:"transition_mode"`
	ReadyForLocalChange bool       `json:"ready_for_local_change"`
	SwitchedAt          *time.Time `json:"switched_at,omitempty"`
	VerifiedAt          *time.Time `json:"verified_at,omitempty"`
	BlockedReason       string     `json:"blocked_reason"`
	UpdatedAt           time.Time  `json:"updated_at"`

	StationName string `json:"station_name"`
}

type ChangeoverNodeTask struct {
	ID                         int64     `json:"id"`
	ProcessChangeoverID        int64     `json:"process_changeover_id"`
	OperatorStationID          *int64    `json:"operator_station_id,omitempty"`
	ProcessNodeID              int64     `json:"process_node_id"`
	FromAssignmentID           *int64    `json:"from_assignment_id,omitempty"`
	ToAssignmentID             *int64    `json:"to_assignment_id,omitempty"`
	State                      string    `json:"state"`
	OldMaterialReleaseRequired bool      `json:"old_material_release_required"`
	NextMaterialOrderID        *int64    `json:"next_material_order_id,omitempty"`
	OldMaterialReleaseOrderID  *int64    `json:"old_material_release_order_id,omitempty"`
	UpdatedAt                  time.Time `json:"updated_at"`

	NodeName    string `json:"node_name"`
	StationName string `json:"station_name"`
}

func scanProcessChangeover(scanner interface{ Scan(...interface{}) error }) (ProcessChangeover, error) {
	var c ProcessChangeover
	var startedAt, completedAt, updatedAt string
	err := scanner.Scan(&c.ID, &c.ProcessID, &c.FromStyleID, &c.ToStyleID, &c.State, &c.Phase, &c.CalledBy, &c.Notes,
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
	rows, err := db.Query(`SELECT c.id, c.process_id, c.from_style_id, c.to_style_id, c.state, c.phase, c.called_by, c.notes,
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
	c, err := scanProcessChangeover(db.QueryRow(`SELECT c.id, c.process_id, c.from_style_id, c.to_style_id, c.state, c.phase, c.called_by, c.notes,
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
	res, err := db.Exec(`INSERT INTO process_changeovers (process_id, from_style_id, to_style_id, state, phase, called_by, notes)
		VALUES (?, ?, ?, 'active', 'runout', ?, ?)`, processID, fromStyleID, toStyleID, calledBy, notes)
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

func (db *DB) UpdateProcessChangeoverPhase(id int64, phase string) error {
	_, err := db.Exec(`UPDATE process_changeovers SET phase=?, updated_at=datetime('now') WHERE id=?`, phase, id)
	return err
}

func (db *DB) CreateChangeoverStationTask(changeoverID, stationID int64, state, mode string, ready bool) (int64, error) {
	res, err := db.Exec(`INSERT INTO changeover_station_tasks (
		process_changeover_id, operator_station_id, state, current_phase, transition_mode, ready_for_local_change
	) VALUES (?, ?, ?, 'runout', ?, ?)`, changeoverID, stationID, state, mode, ready)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) ListChangeoverStationTasks(changeoverID int64) ([]ChangeoverStationTask, error) {
	rows, err := db.Query(`SELECT t.id, t.process_changeover_id, t.operator_station_id, t.state, t.current_phase, t.transition_mode,
		t.ready_for_local_change, COALESCE(t.switched_at, ''), COALESCE(t.verified_at, ''), t.blocked_reason,
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
		var switchedAt, verifiedAt, updatedAt string
		if err := rows.Scan(&t.ID, &t.ProcessChangeoverID, &t.OperatorStationID, &t.State, &t.CurrentPhase, &t.TransitionMode,
			&t.ReadyForLocalChange, &switchedAt, &verifiedAt, &t.BlockedReason, &updatedAt, &t.StationName); err != nil {
			return nil, err
		}
		if switchedAt != "" {
			v := scanTime(switchedAt)
			t.SwitchedAt = &v
		}
		if verifiedAt != "" {
			v := scanTime(verifiedAt)
			t.VerifiedAt = &v
		}
		t.UpdatedAt = scanTime(updatedAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (db *DB) UpdateChangeoverStationTaskState(id int64, state string, ready bool) error {
	var switchedAt interface{} = nil
	if state == "switched" || state == "verified" {
		switchedAt = time.Now().UTC().Format("2006-01-02 15:04:05")
	}
	_, err := db.Exec(`UPDATE changeover_station_tasks SET state=?, ready_for_local_change=?, switched_at=COALESCE(?, switched_at), updated_at=datetime('now') WHERE id=?`,
		state, ready, switchedAt, id)
	return err
}

func (db *DB) UpdateChangeoverStationTaskPhase(id int64, phase string) error {
	_, err := db.Exec(`UPDATE changeover_station_tasks SET current_phase=?, updated_at=datetime('now') WHERE id=?`, phase, id)
	return err
}

func (db *DB) GetChangeoverStationTaskByStation(changeoverID, stationID int64) (*ChangeoverStationTask, error) {
	tasks, err := db.ListChangeoverStationTasks(changeoverID)
	if err != nil {
		return nil, err
	}
	for _, task := range tasks {
		if task.OperatorStationID == stationID {
			taskCopy := task
			return &taskCopy, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (db *DB) CreateChangeoverNodeTask(changeoverID int64, stationID *int64, processNodeID int64, fromAssignmentID, toAssignmentID *int64, state string, releaseRequired bool) (int64, error) {
	res, err := db.Exec(`INSERT INTO changeover_node_tasks (
		process_changeover_id, operator_station_id, process_node_id, from_assignment_id, to_assignment_id, state, old_material_release_required
	) VALUES (?, ?, ?, ?, ?, ?, ?)`, changeoverID, stationID, processNodeID, fromAssignmentID, toAssignmentID, state, releaseRequired)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) ListChangeoverNodeTasks(changeoverID int64) ([]ChangeoverNodeTask, error) {
	rows, err := db.Query(`SELECT t.id, t.process_changeover_id, t.operator_station_id, t.process_node_id, t.from_assignment_id, t.to_assignment_id,
		t.state, t.old_material_release_required, t.next_material_order_id, t.old_material_release_order_id,
		t.updated_at, COALESCE(n.name, ''), COALESCE(s.name, '')
		FROM changeover_node_tasks t
		LEFT JOIN process_nodes n ON n.id = t.process_node_id
		LEFT JOIN operator_stations s ON s.id = t.operator_station_id
		WHERE t.process_changeover_id=? ORDER BY COALESCE(s.sequence, 99999), n.sequence, n.name`, changeoverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChangeoverNodeTask
	for rows.Next() {
		var t ChangeoverNodeTask
		var updatedAt string
		var stationID sql.NullInt64
		if err := rows.Scan(&t.ID, &t.ProcessChangeoverID, &stationID, &t.ProcessNodeID, &t.FromAssignmentID, &t.ToAssignmentID,
			&t.State, &t.OldMaterialReleaseRequired, &t.NextMaterialOrderID, &t.OldMaterialReleaseOrderID,
			&updatedAt, &t.NodeName, &t.StationName); err != nil {
			return nil, err
		}
		if stationID.Valid {
			v := stationID.Int64
			t.OperatorStationID = &v
		}
		t.UpdatedAt = scanTime(updatedAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (db *DB) ListChangeoverNodeTasksByStation(changeoverID, stationID int64) ([]ChangeoverNodeTask, error) {
	rows, err := db.Query(`SELECT t.id, t.process_changeover_id, t.operator_station_id, t.process_node_id, t.from_assignment_id, t.to_assignment_id,
		t.state, t.old_material_release_required, t.next_material_order_id, t.old_material_release_order_id,
		t.updated_at, COALESCE(n.name, ''), COALESCE(s.name, '')
		FROM changeover_node_tasks t
		LEFT JOIN process_nodes n ON n.id = t.process_node_id
		LEFT JOIN operator_stations s ON s.id = t.operator_station_id
		WHERE t.process_changeover_id=? AND t.operator_station_id=? ORDER BY n.sequence, n.name`, changeoverID, stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChangeoverNodeTask
	for rows.Next() {
		var t ChangeoverNodeTask
		var updatedAt string
		var stationID sql.NullInt64
		if err := rows.Scan(&t.ID, &t.ProcessChangeoverID, &stationID, &t.ProcessNodeID, &t.FromAssignmentID, &t.ToAssignmentID,
			&t.State, &t.OldMaterialReleaseRequired, &t.NextMaterialOrderID, &t.OldMaterialReleaseOrderID,
			&updatedAt, &t.NodeName, &t.StationName); err != nil {
			return nil, err
		}
		if stationID.Valid {
			v := stationID.Int64
			t.OperatorStationID = &v
		}
		t.UpdatedAt = scanTime(updatedAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (db *DB) GetChangeoverNodeTaskByNode(changeoverID, processNodeID int64) (*ChangeoverNodeTask, error) {
	rows, err := db.ListChangeoverNodeTasks(changeoverID)
	if err != nil {
		return nil, err
	}
	for _, t := range rows {
		if t.ProcessNodeID == processNodeID {
			return &t, nil
		}
	}
	return nil, sql.ErrNoRows
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
