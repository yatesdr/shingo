// Package changeovers holds process_changeover, station_task, and
// node_task persistence for shingo-edge. The three tables share the
// same aggregate: a process_changeover owns a set of station_tasks
// (one per operator station) and node_tasks (one per process_node
// touched by the changeover).
//
// Phase 5b of the architecture plan moved this CRUD out of the flat
// store/ package and into this sub-package. The outer store/ keeps
// type aliases (`store.ProcessChangeover = changeovers.Changeover`,
// `store.ChangeoverStationTask = changeovers.StationTask`,
// `store.ChangeoverNodeTask = changeovers.NodeTask`) and one-line
// delegate methods on *store.DB so external callers see no API change.
//
// CreateChangeover stays at the top-level store package because it
// runs as a single transaction that also updates the processes
// aggregate (target_style_id, production_state) and inserts rows into
// process_nodes / process_node_runtime_states; that orchestration
// would otherwise have to thread *sql.Tx through several packages.
package changeovers

import (
	"database/sql"
	"time"

	"shingoedge/store/internal/helpers"
)

// NodeTaskInput holds pre-computed data for a single node task to be
// created as part of a changeover transaction.
type NodeTaskInput struct {
	ProcessID    int64  // used for auto-creating process node
	CoreNodeName string // matched against existing nodes or used for auto-create
	FromClaimID  *int64
	ToClaimID    *int64
	Situation    string
	State        string
}

// Changeover is one row of process_changeovers.
type Changeover struct {
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

// StationTask is one row of changeover_station_tasks.
type StationTask struct {
	ID                  int64     `json:"id"`
	ProcessChangeoverID int64     `json:"process_changeover_id"`
	OperatorStationID   int64     `json:"operator_station_id"`
	State               string    `json:"state"`
	UpdatedAt           time.Time `json:"updated_at"`
	StationName         string    `json:"station_name"`
}

// NodeTask is one row of changeover_node_tasks.
type NodeTask struct {
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

// --- changeover header ---

func scanChangeover(scanner interface{ Scan(...interface{}) error }) (Changeover, error) {
	var c Changeover
	var startedAt, completedAt, updatedAt string
	err := scanner.Scan(&c.ID, &c.ProcessID, &c.FromStyleID, &c.ToStyleID, &c.State, &c.CalledBy, &c.Notes,
		&startedAt, &completedAt, &updatedAt, &c.ProcessName, &c.FromStyleName, &c.ToStyleName)
	if err != nil {
		return c, err
	}
	c.StartedAt = helpers.ScanTime(startedAt)
	if completedAt != "" {
		t := helpers.ScanTime(completedAt)
		c.CompletedAt = &t
	}
	c.UpdatedAt = helpers.ScanTime(updatedAt)
	return c, nil
}

// List returns every process_changeover for a process, newest first.
func List(db *sql.DB, processID int64) ([]Changeover, error) {
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
	var out []Changeover
	for rows.Next() {
		c, err := scanChangeover(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetActive returns the active (non-completed, non-cancelled)
// changeover for a process, if any.
func GetActive(db *sql.DB, processID int64) (*Changeover, error) {
	c, err := scanChangeover(db.QueryRow(`SELECT c.id, c.process_id, c.from_style_id, c.to_style_id, c.state, c.called_by, c.notes,
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

// UpdateState changes the state on a process_changeover, setting
// completed_at when transitioning to "completed" or "cancelled".
func UpdateState(db *sql.DB, id int64, state string) error {
	completedAt := sql.NullString{}
	if state == "completed" || state == "cancelled" {
		completedAt = sql.NullString{Valid: true, String: time.Now().UTC().Format(helpers.TimeLayout)}
	}
	_, err := db.Exec(`UPDATE process_changeovers SET state=?, completed_at=CASE WHEN ? != '' THEN ? ELSE completed_at END, updated_at=datetime('now') WHERE id=?`,
		state, completedAt.String, completedAt.String, id)
	return err
}

// --- station tasks ---

// ListStationTasks returns every changeover_station_task for one
// changeover.
func ListStationTasks(db *sql.DB, changeoverID int64) ([]StationTask, error) {
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
	var out []StationTask
	for rows.Next() {
		var t StationTask
		var updatedAt string
		if err := rows.Scan(&t.ID, &t.ProcessChangeoverID, &t.OperatorStationID, &t.State,
			&updatedAt, &t.StationName); err != nil {
			return nil, err
		}
		t.UpdatedAt = helpers.ScanTime(updatedAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateStationTaskState writes the state on a station task.
func UpdateStationTaskState(db *sql.DB, id int64, state string) error {
	_, err := db.Exec(`UPDATE changeover_station_tasks SET state=?, updated_at=datetime('now') WHERE id=?`,
		state, id)
	return err
}

// GetStationTaskByStation returns the station task for one
// (changeover, station) pair.
func GetStationTaskByStation(db *sql.DB, changeoverID, stationID int64) (*StationTask, error) {
	row := db.QueryRow(`SELECT t.id, t.process_changeover_id, t.operator_station_id, t.state,
		t.updated_at, COALESCE(s.name, '')
		FROM changeover_station_tasks t
		LEFT JOIN operator_stations s ON s.id = t.operator_station_id
		WHERE t.process_changeover_id = ? AND t.operator_station_id = ? LIMIT 1`, changeoverID, stationID)
	var t StationTask
	var updatedAt string
	if err := row.Scan(&t.ID, &t.ProcessChangeoverID, &t.OperatorStationID, &t.State,
		&updatedAt, &t.StationName); err != nil {
		return nil, err
	}
	t.UpdatedAt = helpers.ScanTime(updatedAt)
	return &t, nil
}

// --- node tasks ---

func scanNodeTask(scanner interface{ Scan(...interface{}) error }) (NodeTask, error) {
	var t NodeTask
	var updatedAt string
	if err := scanner.Scan(&t.ID, &t.ProcessChangeoverID, &t.ProcessNodeID,
		&t.FromClaimID, &t.ToClaimID, &t.Situation, &t.State,
		&t.NextMaterialOrderID, &t.OldMaterialReleaseOrderID,
		&updatedAt, &t.NodeName); err != nil {
		return t, err
	}
	t.UpdatedAt = helpers.ScanTime(updatedAt)
	return t, nil
}

func listNodeTasksQuery(db *sql.DB, changeoverID int64, extraWhere string, extraArgs ...interface{}) ([]NodeTask, error) {
	query := `SELECT t.id, t.process_changeover_id, t.process_node_id,
		t.from_claim_id, t.to_claim_id, t.situation, t.state,
		t.next_material_order_id, t.old_material_release_order_id,
		t.updated_at, COALESCE(n.name, '')
		FROM changeover_node_tasks t
		LEFT JOIN process_nodes n ON n.id = t.process_node_id
		WHERE t.process_changeover_id=?`
	args := []interface{}{changeoverID}
	if extraWhere != "" {
		query += " AND " + extraWhere
		args = append(args, extraArgs...)
	}
	query += " ORDER BY n.sequence, n.name"
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeTask
	for rows.Next() {
		t, err := scanNodeTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListNodeTasks returns every changeover_node_task for a changeover.
func ListNodeTasks(db *sql.DB, changeoverID int64) ([]NodeTask, error) {
	return listNodeTasksQuery(db, changeoverID, "")
}

// ListNodeTasksByStation filters node tasks to those whose process
// node belongs to the given operator_station.
func ListNodeTasksByStation(db *sql.DB, changeoverID, stationID int64) ([]NodeTask, error) {
	return listNodeTasksQuery(db, changeoverID, "n.operator_station_id=?", stationID)
}

// GetNodeTaskByNode returns the node task for one (changeover, node)
// pair.
func GetNodeTaskByNode(db *sql.DB, changeoverID, processNodeID int64) (*NodeTask, error) {
	t, err := scanNodeTask(db.QueryRow(`SELECT t.id, t.process_changeover_id, t.process_node_id,
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

// UpdateNodeTaskState writes the state on a node task.
func UpdateNodeTaskState(db *sql.DB, id int64, state string) error {
	_, err := db.Exec(`UPDATE changeover_node_tasks SET state=?, updated_at=datetime('now') WHERE id=?`, state, id)
	return err
}

// LinkNodeOrders associates the next/old material order ids with a
// node task. COALESCE preserves any existing values when nil is
// passed.
func LinkNodeOrders(db *sql.DB, id int64, nextOrderID, oldOrderID *int64) error {
	_, err := db.Exec(`UPDATE changeover_node_tasks SET next_material_order_id=COALESCE(?, next_material_order_id),
		old_material_release_order_id=COALESCE(?, old_material_release_order_id), updated_at=datetime('now')
		WHERE id=?`, nextOrderID, oldOrderID, id)
	return err
}
