// changeovers.go — process_changeover, station_task, and node_task
// persistence inside the processes aggregate. The three tables share
// the same aggregate: a process_changeover owns a set of station_tasks
// (one per operator station) and node_tasks (one per process_node
// touched by the changeover).
//
// Phase 6.0c folded shingo-edge/store/changeovers/ into store/processes/
// because changeovers transition processes between styles — the verbs
// belong to the same domain cluster as Process / Style / NodeClaim.
// Function names carry Changeover / StationTask / NodeTask prefixes/
// suffixes so they don't collide with sibling functions in this package.
//
// CreateChangeover stays at the top-level store package (in
// store/process_changeovers.go) because it runs as a single transaction
// that also updates the processes table (target_style_id,
// production_state) and inserts rows into process_nodes /
// process_node_runtime_states; that orchestration would otherwise have
// to thread *sql.Tx through several files.

package processes

import (
	"database/sql"
	"time"

	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// Changeover, StationTask, NodeTask, and NodeTaskInput are the
// changeover-aggregate data types. The structs live in
// shingoedge/domain (Stage 2A.2); these aliases keep the unprefixed
// processes.X names used by every scan helper, Create call site,
// and the outer store/ re-exports. www handlers reference the types
// via shingoedge/domain instead of importing this persistence
// sub-package.
type (
	Changeover    = domain.Changeover
	StationTask   = domain.StationTask
	NodeTask      = domain.NodeTask
	NodeTaskInput = domain.NodeTaskInput
)

// --- changeover header ---

const changeoverSelect = `c.id, c.process_id, c.from_style_id, c.to_style_id, c.state, c.called_by, c.notes,
		c.started_at, COALESCE(c.completed_at, ''), COALESCE(c.triggered_by, ''), c.updated_at,
		COALESCE(p.name, ''), COALESCE(fs.name, ''), COALESCE(ts.name, '')`

func scanChangeover(scanner interface{ Scan(...interface{}) error }) (Changeover, error) {
	var c Changeover
	var startedAt, completedAt, updatedAt string
	err := scanner.Scan(&c.ID, &c.ProcessID, &c.FromStyleID, &c.ToStyleID, &c.State, &c.CalledBy, &c.Notes,
		&startedAt, &completedAt, &c.TriggeredBy, &updatedAt, &c.ProcessName, &c.FromStyleName, &c.ToStyleName)
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

// ListChangeovers returns every process_changeover for a process,
// newest first.
func ListChangeovers(db *sql.DB, processID int64) ([]Changeover, error) {
	rows, err := db.Query(`SELECT `+changeoverSelect+`
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

// GetActiveChangeover returns the active (non-completed,
// non-cancelled) changeover for a process, if any.
func GetActiveChangeover(db *sql.DB, processID int64) (*Changeover, error) {
	c, err := scanChangeover(db.QueryRow(`SELECT `+changeoverSelect+`
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

// UpdateChangeoverState changes the state on a process_changeover,
// setting completed_at when transitioning to "completed" or
// "cancelled". Does NOT touch triggered_by — callers that record a
// trigger source should call UpdateChangeoverStateWithTrigger.
func UpdateChangeoverState(db *sql.DB, id int64, state string) error {
	return UpdateChangeoverStateWithTrigger(db, id, state, "")
}

// UpdateChangeoverStateWithTrigger writes the state and (when
// non-empty) the triggered_by audit field together. Empty triggeredBy
// leaves the existing value untouched, so callers that don't have a
// source distinction don't blank it out on later state writes.
func UpdateChangeoverStateWithTrigger(db *sql.DB, id int64, state, triggeredBy string) error {
	completedAt := sql.NullString{}
	if state == "completed" || state == "cancelled" {
		completedAt = sql.NullString{Valid: true, String: time.Now().UTC().Format(helpers.TimeLayout)}
	}
	_, err := db.Exec(`UPDATE process_changeovers SET state=?,
		completed_at=CASE WHEN ? != '' THEN ? ELSE completed_at END,
		triggered_by=CASE WHEN ? != '' THEN ? ELSE triggered_by END,
		updated_at=datetime('now') WHERE id=?`,
		state, completedAt.String, completedAt.String,
		triggeredBy, triggeredBy, id)
	return err
}

// --- station tasks ---

// ListChangeoverStationTasks returns every changeover_station_task for
// one changeover.
func ListChangeoverStationTasks(db *sql.DB, changeoverID int64) ([]StationTask, error) {
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

// UpdateChangeoverStationTaskState writes the state on a station task.
func UpdateChangeoverStationTaskState(db *sql.DB, id int64, state string) error {
	_, err := db.Exec(`UPDATE changeover_station_tasks SET state=?, updated_at=datetime('now') WHERE id=?`,
		state, id)
	return err
}

// GetChangeoverStationTaskByStation returns the station task for one
// (changeover, station) pair.
func GetChangeoverStationTaskByStation(db *sql.DB, changeoverID, stationID int64) (*StationTask, error) {
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
		&t.SkipNote,
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
		COALESCE(t.skip_note, ''),
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

// ListChangeoverNodeTasks returns every changeover_node_task for a
// changeover.
func ListChangeoverNodeTasks(db *sql.DB, changeoverID int64) ([]NodeTask, error) {
	return listNodeTasksQuery(db, changeoverID, "")
}

// ListChangeoverNodeTasksByStation filters node tasks to those whose
// process node belongs to the given operator_station.
func ListChangeoverNodeTasksByStation(db *sql.DB, changeoverID, stationID int64) ([]NodeTask, error) {
	return listNodeTasksQuery(db, changeoverID, "n.operator_station_id=?", stationID)
}

// GetChangeoverNodeTaskByNode returns the node task for one
// (changeover, node) pair.
func GetChangeoverNodeTaskByNode(db *sql.DB, changeoverID, processNodeID int64) (*NodeTask, error) {
	t, err := scanNodeTask(db.QueryRow(`SELECT t.id, t.process_changeover_id, t.process_node_id,
		t.from_claim_id, t.to_claim_id, t.situation, t.state,
		t.next_material_order_id, t.old_material_release_order_id,
		COALESCE(t.skip_note, ''),
		t.updated_at, COALESCE(n.name, '')
		FROM changeover_node_tasks t
		LEFT JOIN process_nodes n ON n.id = t.process_node_id
		WHERE t.process_changeover_id=? AND t.process_node_id=? LIMIT 1`, changeoverID, processNodeID))
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// FindChangeoverNodeTaskByOrderID returns the node task that references
// orderID via NextMaterialOrderID or OldMaterialReleaseOrderID, plus its
// parent changeover's state.
//
// Direct order-ID match — does NOT filter by changeover state, unlike
// GetActiveProcessChangeover. Used by the orphan-cancellation handler so
// historical orphans (orders linked to changeover tasks that completed
// in non-success terminal states outside cancelProcessChangeoverInternal)
// can be reconciled even when the changeover row itself has already
// finalized.
func FindChangeoverNodeTaskByOrderID(db *sql.DB, orderID int64) (*NodeTask, string, error) {
	row := db.QueryRow(`SELECT t.id, t.process_changeover_id, t.process_node_id,
		t.from_claim_id, t.to_claim_id, t.situation, t.state,
		t.next_material_order_id, t.old_material_release_order_id,
		COALESCE(t.skip_note, ''),
		t.updated_at, COALESCE(n.name, ''), c.state
		FROM changeover_node_tasks t
		LEFT JOIN process_nodes n ON n.id = t.process_node_id
		JOIN process_changeovers c ON c.id = t.process_changeover_id
		WHERE t.next_material_order_id=? OR t.old_material_release_order_id=?
		LIMIT 1`, orderID, orderID)
	var t NodeTask
	var updatedAt, coState string
	if err := row.Scan(&t.ID, &t.ProcessChangeoverID, &t.ProcessNodeID,
		&t.FromClaimID, &t.ToClaimID, &t.Situation, &t.State,
		&t.NextMaterialOrderID, &t.OldMaterialReleaseOrderID,
		&t.SkipNote,
		&updatedAt, &t.NodeName, &coState); err != nil {
		return nil, "", err
	}
	t.UpdatedAt = helpers.ScanTime(updatedAt)
	return &t, coState, nil
}

// GetChangeoverNodeTaskByEvacOrderID returns the node task that
// references orderID via OldMaterialReleaseOrderID (the evac leg) only.
// Distinct from FindChangeoverNodeTaskByOrderID, which OR-matches both
// legs for the orphan-cancellation use case.
//
// Used by handler_bin_picked_up.go's deferred-supply auto-release: when
// the picked-up order is an evac, the paired supply (NextMaterialOrderID
// on the same task) is the one to release. Matching evac only avoids a
// degenerate case where a pickup on the supply leg would re-trigger
// release on itself.
//
// Returns sql.ErrNoRows when the order isn't an evac on any task —
// that's the normal case for non-changeover orders, and callers treat
// it as "no auto-release to do, move on."
func GetChangeoverNodeTaskByEvacOrderID(db *sql.DB, orderID int64) (*NodeTask, error) {
	t, err := scanNodeTask(db.QueryRow(`SELECT t.id, t.process_changeover_id, t.process_node_id,
		t.from_claim_id, t.to_claim_id, t.situation, t.state,
		t.next_material_order_id, t.old_material_release_order_id,
		COALESCE(t.skip_note, ''),
		t.updated_at, COALESCE(n.name, '')
		FROM changeover_node_tasks t
		LEFT JOIN process_nodes n ON n.id = t.process_node_id
		WHERE t.old_material_release_order_id=? LIMIT 1`, orderID))
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// UpdateChangeoverNodeTaskState writes the state on a node task.
func UpdateChangeoverNodeTaskState(db *sql.DB, id int64, state string) error {
	_, err := db.Exec(`UPDATE changeover_node_tasks SET state=?, updated_at=datetime('now') WHERE id=?`, state, id)
	return err
}

// SetChangeoverNodeTaskSkipNote writes the operator-facing skip message on
// a node task. Pass the empty string to clear it (e.g. when the next
// state-advancing operator action makes the chip stale).
func SetChangeoverNodeTaskSkipNote(db *sql.DB, id int64, note string) error {
	_, err := db.Exec(`UPDATE changeover_node_tasks SET skip_note=?, updated_at=datetime('now') WHERE id=?`, note, id)
	return err
}

// LinkChangeoverNodeOrders associates the next/old material order ids
// with a node task. COALESCE preserves any existing values when nil is
// passed.
func LinkChangeoverNodeOrders(db *sql.DB, id int64, nextOrderID, oldOrderID *int64) error {
	_, err := db.Exec(`UPDATE changeover_node_tasks SET next_material_order_id=COALESCE(?, next_material_order_id),
		old_material_release_order_id=COALESCE(?, old_material_release_order_id), updated_at=datetime('now')
		WHERE id=?`, nextOrderID, oldOrderID, id)
	return err
}
