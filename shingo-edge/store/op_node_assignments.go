package store

import (
	"database/sql"
	"time"
)

const (
	CycleModeSequential  = "sequential"
	CycleModeSingleRobot = "single_robot"
	CycleModeTwoRobot    = "two_robot"
)

type OpNodeStyleAssignment struct {
	ID                           int64     `json:"id"`
	OpNodeID                     int64     `json:"op_node_id"`
	StyleID                      int64     `json:"style_id"`
	PayloadCode                  string    `json:"payload_code"`
	PayloadDescription           string    `json:"payload_description"`
	Role                         string    `json:"role"`
	UOPCapacity                  int       `json:"uop_capacity"`
	ReorderPoint                 int       `json:"reorder_point"`
	AutoReorderEnabled           bool      `json:"auto_reorder_enabled"`
	CycleMode                    string    `json:"cycle_mode"`
	RetrieveEmpty                bool      `json:"retrieve_empty"`
	RequiresManifestConfirmation bool      `json:"requires_manifest_confirmation"`
	AllowsPartialReturn          bool      `json:"allows_partial_return"`
	ChangeoverGroup              string    `json:"changeover_group"`
	ChangeoverSequence           int       `json:"changeover_sequence"`
	ChangeoverPolicy             string    `json:"changeover_policy"`
	CreatedAt                    time.Time `json:"created_at"`
	UpdatedAt                    time.Time `json:"updated_at"`

	StyleName string `json:"style_name"`
	NodeName  string `json:"node_name"`
}

type OpNodeStyleAssignmentInput struct {
	OpNodeID                     int64  `json:"op_node_id"`
	StyleID                      int64  `json:"style_id"`
	PayloadCode                  string `json:"payload_code"`
	PayloadDescription           string `json:"payload_description"`
	Role                         string `json:"role"`
	UOPCapacity                  int    `json:"uop_capacity"`
	ReorderPoint                 int    `json:"reorder_point"`
	AutoReorderEnabled           bool   `json:"auto_reorder_enabled"`
	CycleMode                    string `json:"cycle_mode"`
	RetrieveEmpty                bool   `json:"retrieve_empty"`
	RequiresManifestConfirmation bool   `json:"requires_manifest_confirmation"`
	AllowsPartialReturn          bool   `json:"allows_partial_return"`
	ChangeoverGroup              string `json:"changeover_group"`
	ChangeoverSequence           int    `json:"changeover_sequence"`
	ChangeoverPolicy             string `json:"changeover_policy"`
}

type OpNodeRuntimeState struct {
	ID                 int64      `json:"id"`
	OpNodeID           int64      `json:"op_node_id"`
	EffectiveStyleID   *int64     `json:"effective_style_id,omitempty"`
	ActiveAssignmentID *int64     `json:"active_assignment_id,omitempty"`
	StagedAssignmentID *int64     `json:"staged_assignment_id,omitempty"`
	LoadedPayloadCode  string     `json:"loaded_payload_code"`
	MaterialStatus     string     `json:"material_status"`
	RemainingUOP       int        `json:"remaining_uop"`
	ManifestStatus     string     `json:"manifest_status"`
	ActiveOrderID      *int64     `json:"active_order_id,omitempty"`
	StagedOrderID      *int64     `json:"staged_order_id,omitempty"`
	LoadedBinLabel     string     `json:"loaded_bin_label"`
	LoadedAt           *time.Time `json:"loaded_at,omitempty"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

const assignmentSelect = `a.id, a.op_node_id, a.style_id, a.payload_code, a.payload_description,
	a.role, a.uop_capacity, a.reorder_point, a.auto_reorder_enabled, a.cycle_mode,
	a.retrieve_empty, a.requires_manifest_confirmation, a.allows_partial_return,
	a.changeover_group, a.changeover_sequence, a.changeover_policy,
	a.created_at, a.updated_at, COALESCE(s.name, ''), COALESCE(n.name, '')`

const assignmentJoin = `FROM op_node_style_assignments a
	LEFT JOIN styles s ON s.id = a.style_id
	LEFT JOIN op_station_nodes n ON n.id = a.op_node_id`

func scanAssignment(scanner interface{ Scan(...interface{}) error }) (OpNodeStyleAssignment, error) {
	var a OpNodeStyleAssignment
	var createdAt, updatedAt string
	err := scanner.Scan(
		&a.ID, &a.OpNodeID, &a.StyleID, &a.PayloadCode, &a.PayloadDescription,
		&a.Role, &a.UOPCapacity, &a.ReorderPoint, &a.AutoReorderEnabled, &a.CycleMode,
		&a.RetrieveEmpty, &a.RequiresManifestConfirmation, &a.AllowsPartialReturn,
		&a.ChangeoverGroup, &a.ChangeoverSequence, &a.ChangeoverPolicy,
		&createdAt, &updatedAt, &a.StyleName, &a.NodeName,
	)
	if err != nil {
		return a, err
	}
	a.CreatedAt = scanTime(createdAt)
	a.UpdatedAt = scanTime(updatedAt)
	return a, nil
}

func scanAssignments(rows rowScanner) ([]OpNodeStyleAssignment, error) {
	var out []OpNodeStyleAssignment
	for rows.Next() {
		a, err := scanAssignment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (db *DB) ListOpNodeAssignmentsByStyle(styleID int64) ([]OpNodeStyleAssignment, error) {
	rows, err := db.Query(`SELECT `+assignmentSelect+` `+assignmentJoin+` WHERE a.style_id=? ORDER BY n.name`, styleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAssignments(rows)
}

func (db *DB) ListOpNodeAssignmentsByNode(opNodeID int64) ([]OpNodeStyleAssignment, error) {
	rows, err := db.Query(`SELECT `+assignmentSelect+` `+assignmentJoin+` WHERE a.op_node_id=? ORDER BY s.name`, opNodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAssignments(rows)
}

func (db *DB) ListOpNodeAssignmentsByProcess(processID int64) ([]OpNodeStyleAssignment, error) {
	query := `SELECT ` + assignmentSelect + ` ` + assignmentJoin + `
		LEFT JOIN operator_stations os ON os.id = n.operator_station_id`
	var rows *sql.Rows
	var err error
	if processID > 0 {
		query += ` WHERE os.process_id=? ORDER BY s.name, n.name`
		rows, err = db.Query(query, processID)
	} else {
		query += ` ORDER BY s.name, n.name`
		rows, err = db.Query(query)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAssignments(rows)
}

func (db *DB) GetOpNodeAssignment(id int64) (*OpNodeStyleAssignment, error) {
	a, err := scanAssignment(db.QueryRow(`SELECT `+assignmentSelect+` `+assignmentJoin+` WHERE a.id=?`, id))
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (db *DB) GetOpNodeAssignmentForStyle(opNodeID, styleID int64) (*OpNodeStyleAssignment, error) {
	a, err := scanAssignment(db.QueryRow(`SELECT `+assignmentSelect+` `+assignmentJoin+` WHERE a.op_node_id=? AND a.style_id=?`, opNodeID, styleID))
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (db *DB) GetPreferredOpNodeAssignment(opNodeID int64) (*OpNodeStyleAssignment, error) {
	runtime, err := db.EnsureOpNodeRuntime(opNodeID)
	if err == nil {
		if runtime.ActiveAssignmentID != nil {
			return db.GetOpNodeAssignment(*runtime.ActiveAssignmentID)
		}
		if runtime.StagedAssignmentID != nil {
			return db.GetOpNodeAssignment(*runtime.StagedAssignmentID)
		}
	}
	node, err := db.GetOpStationNode(opNodeID)
	if err != nil {
		return nil, err
	}
	process, err := db.GetProcess(node.ProcessID)
	if err != nil {
		return nil, err
	}
	if process.ActiveStyleID != nil {
		if a, err := db.GetOpNodeAssignmentForStyle(opNodeID, *process.ActiveStyleID); err == nil {
			return a, nil
		}
	}
	if process.TargetStyleID != nil {
		if a, err := db.GetOpNodeAssignmentForStyle(opNodeID, *process.TargetStyleID); err == nil {
			return a, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (db *DB) UpsertOpNodeAssignment(in OpNodeStyleAssignmentInput) (int64, error) {
	existing, err := db.GetOpNodeAssignmentForStyle(in.OpNodeID, in.StyleID)
	if err == nil && existing != nil {
		_, err = db.Exec(`UPDATE op_node_style_assignments SET
			payload_code=?, payload_description=?, role=?, uop_capacity=?, reorder_point=?,
			auto_reorder_enabled=?, cycle_mode=?, retrieve_empty=?, requires_manifest_confirmation=?,
			allows_partial_return=?, changeover_group=?, changeover_sequence=?, changeover_policy=?,
			updated_at=datetime('now')
			WHERE id=?`,
			in.PayloadCode, in.PayloadDescription, in.Role, in.UOPCapacity, in.ReorderPoint,
			in.AutoReorderEnabled, in.CycleMode, in.RetrieveEmpty, in.RequiresManifestConfirmation,
			in.AllowsPartialReturn, in.ChangeoverGroup, in.ChangeoverSequence, in.ChangeoverPolicy,
			existing.ID,
		)
		return existing.ID, err
	}
	res, err := db.Exec(`INSERT INTO op_node_style_assignments (
		op_node_id, style_id, payload_code, payload_description, role, uop_capacity, reorder_point,
		auto_reorder_enabled, cycle_mode, retrieve_empty, requires_manifest_confirmation,
		allows_partial_return, changeover_group, changeover_sequence, changeover_policy
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.OpNodeID, in.StyleID, in.PayloadCode, in.PayloadDescription, in.Role, in.UOPCapacity, in.ReorderPoint,
		in.AutoReorderEnabled, in.CycleMode, in.RetrieveEmpty, in.RequiresManifestConfirmation,
		in.AllowsPartialReturn, in.ChangeoverGroup, in.ChangeoverSequence, in.ChangeoverPolicy,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) DeleteOpNodeAssignment(id int64) error {
	_, err := db.Exec(`DELETE FROM op_node_style_assignments WHERE id=?`, id)
	return err
}

func scanRuntime(scanner interface{ Scan(...interface{}) error }) (OpNodeRuntimeState, error) {
	var r OpNodeRuntimeState
	var loadedAt, updatedAt string
	err := scanner.Scan(
		&r.ID, &r.OpNodeID, &r.EffectiveStyleID, &r.ActiveAssignmentID, &r.StagedAssignmentID,
		&r.LoadedPayloadCode, &r.MaterialStatus, &r.RemainingUOP, &r.ManifestStatus,
		&r.ActiveOrderID, &r.StagedOrderID, &r.LoadedBinLabel, &loadedAt, &updatedAt,
	)
	if err != nil {
		return r, err
	}
	if loadedAt != "" {
		t := scanTime(loadedAt)
		r.LoadedAt = &t
	}
	r.UpdatedAt = scanTime(updatedAt)
	return r, nil
}

func (db *DB) EnsureOpNodeRuntime(opNodeID int64) (*OpNodeRuntimeState, error) {
	r, err := db.GetOpNodeRuntime(opNodeID)
	if err == nil {
		return r, nil
	}
	_, err = db.Exec(`INSERT INTO op_node_runtime_states (op_node_id) VALUES (?)`, opNodeID)
	if err != nil {
		return nil, err
	}
	return db.GetOpNodeRuntime(opNodeID)
}

func (db *DB) GetOpNodeRuntime(opNodeID int64) (*OpNodeRuntimeState, error) {
	r, err := scanRuntime(db.QueryRow(`SELECT id, op_node_id, effective_style_id, active_assignment_id, staged_assignment_id,
		loaded_payload_code, material_status, remaining_uop, manifest_status, active_order_id, staged_order_id,
		loaded_bin_label, COALESCE(loaded_at, ''), updated_at
		FROM op_node_runtime_states WHERE op_node_id=?`, opNodeID))
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (db *DB) SetOpNodeRuntime(opNodeID int64, effectiveStyleID, activeAssignmentID, stagedAssignmentID *int64, loadedPayloadCode, materialStatus string, remainingUOP int, manifestStatus string) error {
	if _, err := db.EnsureOpNodeRuntime(opNodeID); err != nil {
		return err
	}
	_, err := db.Exec(`UPDATE op_node_runtime_states SET
		effective_style_id=?, active_assignment_id=?, staged_assignment_id=?, loaded_payload_code=?,
		material_status=?, remaining_uop=?, manifest_status=?, updated_at=datetime('now')
		WHERE op_node_id=?`,
		effectiveStyleID, activeAssignmentID, stagedAssignmentID, loadedPayloadCode,
		materialStatus, remainingUOP, manifestStatus, opNodeID,
	)
	return err
}

func (db *DB) UpdateOpNodeRuntimeOrders(opNodeID int64, activeOrderID, stagedOrderID *int64) error {
	if _, err := db.EnsureOpNodeRuntime(opNodeID); err != nil {
		return err
	}
	_, err := db.Exec(`UPDATE op_node_runtime_states SET active_order_id=?, staged_order_id=?, updated_at=datetime('now') WHERE op_node_id=?`,
		activeOrderID, stagedOrderID, opNodeID)
	return err
}

func (db *DB) UpdateOpNodeManifestStatus(opNodeID int64, manifestStatus string) error {
	if _, err := db.EnsureOpNodeRuntime(opNodeID); err != nil {
		return err
	}
	_, err := db.Exec(`UPDATE op_node_runtime_states SET manifest_status=?, updated_at=datetime('now') WHERE op_node_id=?`,
		manifestStatus, opNodeID)
	return err
}

func (db *DB) UpdateOpNodeRemaining(opNodeID int64, remainingUOP int, materialStatus string) error {
	if _, err := db.EnsureOpNodeRuntime(opNodeID); err != nil {
		return err
	}
	_, err := db.Exec(`UPDATE op_node_runtime_states SET remaining_uop=?, material_status=?, updated_at=datetime('now') WHERE op_node_id=?`,
		remainingUOP, materialStatus, opNodeID)
	return err
}
