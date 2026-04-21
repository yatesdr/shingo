// Package processes holds process, process_node, and
// process_node_runtime persistence for shingo-edge. All three sit on
// the same aggregate: a process owns a set of process_nodes, each of
// which has a runtime row that tracks the active claim, remaining UOP,
// and currently-tracked orders.
//
// Phase 5b of the architecture plan moved this CRUD out of the flat
// store/ package and into this sub-package. The outer store/ keeps
// type aliases (`store.Process = processes.Process`, etc.) and one-line
// delegate methods on *store.DB so external callers see no API change.
package processes

import (
	"database/sql"
	"strings"
	"time"

	"shingoedge/store/internal/helpers"
)

// Process represents a production process (physical production area).
type Process struct {
	ID              int64     `json:"id"`
	Name            string    `json:"name"`
	Description     string    `json:"description"`
	ActiveStyleID   *int64    `json:"active_style_id"`
	TargetStyleID   *int64    `json:"target_style_id,omitempty"`
	ProductionState string    `json:"production_state"`
	CounterPLCName  string    `json:"counter_plc_name"`
	CounterTagName  string    `json:"counter_tag_name"`
	CounterEnabled  bool      `json:"counter_enabled"`
	CreatedAt       time.Time `json:"created_at"`
}

// Node is one row of process_nodes.
type Node struct {
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

// NodeInput is the input shape for CreateNode / UpdateNode.
type NodeInput struct {
	ProcessID         int64  `json:"process_id"`
	OperatorStationID *int64 `json:"operator_station_id,omitempty"`
	CoreNodeName      string `json:"core_node_name"`
	Code              string `json:"code"`
	Name              string `json:"name"`
	Sequence          int    `json:"sequence"`
	Enabled           bool   `json:"enabled"`
}

// RuntimeState is one row of process_node_runtime_states.
type RuntimeState struct {
	ID            int64     `json:"id"`
	ProcessNodeID int64     `json:"process_node_id"`
	ActiveClaimID *int64    `json:"active_claim_id,omitempty"`
	RemainingUOP  int       `json:"remaining_uop"`
	ActiveOrderID *int64    `json:"active_order_id,omitempty"`
	StagedOrderID *int64    `json:"staged_order_id,omitempty"`
	ActivePull    bool      `json:"active_pull"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// --- processes ---

func scanProcess(scanner interface{ Scan(...interface{}) error }) (Process, error) {
	var p Process
	var createdAt string
	if err := scanner.Scan(&p.ID, &p.Name, &p.Description, &p.ActiveStyleID, &p.TargetStyleID, &p.ProductionState, &p.CounterPLCName, &p.CounterTagName, &p.CounterEnabled, &createdAt); err != nil {
		return p, err
	}
	p.CreatedAt = helpers.ScanTime(createdAt)
	return p, nil
}

// List returns every process row sorted by name.
func List(db *sql.DB) ([]Process, error) {
	rows, err := db.Query(`SELECT id, name, description, active_style_id, target_style_id, production_state, counter_plc_name, counter_tag_name, counter_enabled, created_at FROM processes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Process
	for rows.Next() {
		l, err := scanProcess(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Get returns one process by id.
func Get(db *sql.DB, id int64) (*Process, error) {
	l, err := scanProcess(db.QueryRow(`SELECT id, name, description, active_style_id, target_style_id, production_state, counter_plc_name, counter_tag_name, counter_enabled, created_at FROM processes WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// Create inserts a process and returns the new row id.
func Create(db *sql.DB, name, description, productionState string, counterPLC, counterTag string, counterEnabled bool) (int64, error) {
	if productionState == "" {
		productionState = "active_production"
	}
	res, err := db.Exec(`INSERT INTO processes (name, description, production_state, counter_plc_name, counter_tag_name, counter_enabled) VALUES (?, ?, ?, ?, ?, ?)`,
		name, description, productionState, counterPLC, counterTag, counterEnabled)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Update modifies a process row.
func Update(db *sql.DB, id int64, name, description, productionState string, counterPLC, counterTag string, counterEnabled bool) error {
	if productionState == "" {
		productionState = "active_production"
	}
	_, err := db.Exec(`UPDATE processes SET name=?, description=?, production_state=?, counter_plc_name=?, counter_tag_name=?, counter_enabled=? WHERE id=?`,
		name, description, productionState, counterPLC, counterTag, counterEnabled, id)
	return err
}

// Delete removes a process row.
func Delete(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM processes WHERE id=?`, id)
	return err
}

// SetActiveStyle changes the active_style_id on a process.
func SetActiveStyle(db *sql.DB, processID int64, styleID *int64) error {
	_, err := db.Exec(`UPDATE processes SET active_style_id=? WHERE id=?`, styleID, processID)
	return err
}

// SetTargetStyle changes the target_style_id on a process (used during
// changeovers).
func SetTargetStyle(db *sql.DB, processID int64, styleID *int64) error {
	_, err := db.Exec(`UPDATE processes SET target_style_id=? WHERE id=?`, styleID, processID)
	return err
}

// GetActiveStyleID returns just the active_style_id pointer for a
// process.
func GetActiveStyleID(db *sql.DB, processID int64) (*int64, error) {
	var id *int64
	err := db.QueryRow(`SELECT active_style_id FROM processes WHERE id = ?`, processID).Scan(&id)
	if err != nil {
		return nil, err
	}
	return id, nil
}

// SetProductionState writes the production_state on a process.
func SetProductionState(db *sql.DB, processID int64, state string) error {
	_, err := db.Exec(`UPDATE processes SET production_state=? WHERE id=?`, state, processID)
	return err
}

// --- process nodes ---

const nodeSelect = `n.id, n.process_id, n.operator_station_id, n.core_node_name, n.code, n.name,
	n.sequence, n.enabled, n.created_at, n.updated_at, COALESCE(s.name, ''), COALESCE(p.name, '')`

const nodeJoin = `FROM process_nodes n
	LEFT JOIN operator_stations s ON s.id = n.operator_station_id
	LEFT JOIN processes p ON p.id = n.process_id`

func scanNode(scanner interface{ Scan(...interface{}) error }) (Node, error) {
	var n Node
	var createdAt, updatedAt string
	var stationID sql.NullInt64
	err := scanner.Scan(
		&n.ID, &n.ProcessID, &stationID, &n.CoreNodeName, &n.Code, &n.Name,
		&n.Sequence, &n.Enabled, &createdAt, &updatedAt, &n.StationName, &n.ProcessName,
	)
	if err != nil {
		return n, err
	}
	n.CreatedAt = helpers.ScanTime(createdAt)
	n.UpdatedAt = helpers.ScanTime(updatedAt)
	if stationID.Valid {
		id := stationID.Int64
		n.OperatorStationID = &id
	}
	return n, nil
}

func scanNodes(rows helpers.RowScanner) ([]Node, error) {
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListNodes returns every process_nodes row.
func ListNodes(db *sql.DB) ([]Node, error) {
	rows, err := db.Query(`SELECT ` + nodeSelect + ` ` + nodeJoin + ` ORDER BY n.process_id, n.sequence, n.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ListNodesByProcess returns process_nodes rows for one process.
func ListNodesByProcess(db *sql.DB, processID int64) ([]Node, error) {
	rows, err := db.Query(`SELECT `+nodeSelect+` `+nodeJoin+` WHERE n.process_id=? ORDER BY n.sequence, n.name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ListNodesByStation returns process_nodes rows for one operator_station.
func ListNodesByStation(db *sql.DB, stationID int64) ([]Node, error) {
	rows, err := db.Query(`SELECT `+nodeSelect+` `+nodeJoin+` WHERE n.operator_station_id=? ORDER BY n.sequence, n.name`, stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetNode returns one process_node row by id.
func GetNode(db *sql.DB, id int64) (*Node, error) {
	n, err := scanNode(db.QueryRow(`SELECT `+nodeSelect+` `+nodeJoin+` WHERE n.id=?`, id))
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// CreateNode inserts a process_node row, generating the code and
// sequence number when not supplied.
func CreateNode(db *sql.DB, in NodeInput) (int64, error) {
	in.CoreNodeName = strings.TrimSpace(in.CoreNodeName)
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = in.CoreNodeName
	}
	if in.OperatorStationID != nil && *in.OperatorStationID <= 0 {
		in.OperatorStationID = nil
	}
	if in.Code == "" {
		code, err := generateNodeCode(db, in.ProcessID, in.CoreNodeName, in.Name)
		if err != nil {
			return 0, err
		}
		in.Code = code
	}
	if in.Sequence <= 0 {
		next, err := nextNodeSequence(db, in.ProcessID)
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

// UpdateNode modifies an existing process_node row, falling back to
// the existing code/sequence when the input leaves them blank.
func UpdateNode(db *sql.DB, id int64, in NodeInput) error {
	existing, err := GetNode(db, id)
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

// DeleteNode removes a process_node row.
func DeleteNode(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM process_nodes WHERE id=?`, id)
	return err
}

func nextNodeSequence(db *sql.DB, processID int64) (int, error) {
	var maxSeq sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(sequence) FROM process_nodes WHERE process_id=?`, processID).Scan(&maxSeq); err != nil {
		return 0, err
	}
	if !maxSeq.Valid {
		return 1, nil
	}
	return int(maxSeq.Int64) + 1, nil
}

func generateNodeCode(db *sql.DB, processID int64, coreNodeName, name string) (string, error) {
	base := helpers.SlugName(coreNodeName, "")
	if base == "" {
		base = helpers.SlugName(name, "")
	}
	return helpers.GenerateUniqueCode(db, "process_nodes", "process_id", processID, base, "node")
}

// --- process node runtime states ---

func scanRuntime(scanner interface{ Scan(...interface{}) error }) (RuntimeState, error) {
	var r RuntimeState
	var updatedAt string
	err := scanner.Scan(&r.ID, &r.ProcessNodeID, &r.ActiveClaimID, &r.RemainingUOP,
		&r.ActiveOrderID, &r.StagedOrderID, &r.ActivePull, &updatedAt)
	if err != nil {
		return r, err
	}
	r.UpdatedAt = helpers.ScanTime(updatedAt)
	return r, nil
}

// EnsureRuntime returns the runtime row for a process_node, inserting
// a fresh row when none exists yet.
func EnsureRuntime(db *sql.DB, processNodeID int64) (*RuntimeState, error) {
	r, err := GetRuntime(db, processNodeID)
	if err == nil {
		return r, nil
	}
	_, err = db.Exec(`INSERT INTO process_node_runtime_states (process_node_id) VALUES (?)`, processNodeID)
	if err != nil {
		return nil, err
	}
	return GetRuntime(db, processNodeID)
}

// GetRuntime returns the runtime row for a process_node.
func GetRuntime(db *sql.DB, processNodeID int64) (*RuntimeState, error) {
	r, err := scanRuntime(db.QueryRow(`SELECT id, process_node_id, active_claim_id, remaining_uop,
		active_order_id, staged_order_id, active_pull, updated_at
		FROM process_node_runtime_states WHERE process_node_id=?`, processNodeID))
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// SetRuntime updates the active claim and remaining UOP on a runtime
// row.
func SetRuntime(db *sql.DB, processNodeID int64, activeClaimID *int64, remainingUOP int) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET
		active_claim_id=?, remaining_uop=?, updated_at=datetime('now')
		WHERE process_node_id=?`,
		activeClaimID, remainingUOP, processNodeID)
	return err
}

// UpdateRuntimeOrders writes the active and staged order pointers on a
// runtime row.
func UpdateRuntimeOrders(db *sql.DB, processNodeID int64, activeOrderID, stagedOrderID *int64) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET active_order_id=?, staged_order_id=?, updated_at=datetime('now') WHERE process_node_id=?`,
		activeOrderID, stagedOrderID, processNodeID)
	return err
}

// UpdateRuntimeUOP writes the remaining UOP on a runtime row.
func UpdateRuntimeUOP(db *sql.DB, processNodeID int64, remainingUOP int) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET remaining_uop=?, updated_at=datetime('now') WHERE process_node_id=?`,
		remainingUOP, processNodeID)
	return err
}

// SetActivePull marks a node as the active pull point for A/B cycling.
// Only the active-pull node gets counter delta decrements.
func SetActivePull(db *sql.DB, processNodeID int64, active bool) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET active_pull=?, updated_at=datetime('now') WHERE process_node_id=?`,
		active, processNodeID)
	return err
}
