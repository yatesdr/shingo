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

type ProcessNodeStyleAssignment struct {
	ID                           int64     `json:"id"`
	ProcessNodeID                int64     `json:"process_node_id"`
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

type ProcessNodeStyleAssignmentInput struct {
	ProcessNodeID                int64  `json:"process_node_id"`
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

type ProcessNodeRuntimeState struct {
	ID                 int64      `json:"id"`
	ProcessNodeID      int64      `json:"process_node_id"`
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

const assignmentSelect = `a.id, a.process_node_id, a.style_id, a.payload_code, a.payload_description,
	a.role, a.uop_capacity, a.reorder_point, a.auto_reorder_enabled, a.cycle_mode,
	a.retrieve_empty, a.requires_manifest_confirmation, a.allows_partial_return,
	a.changeover_group, a.changeover_sequence, a.changeover_policy,
	a.created_at, a.updated_at, COALESCE(s.name, ''), COALESCE(n.name, '')`

const assignmentJoin = `FROM process_node_style_assignments a
	LEFT JOIN styles s ON s.id = a.style_id
	LEFT JOIN process_nodes n ON n.id = a.process_node_id`

func scanAssignment(scanner interface{ Scan(...interface{}) error }) (ProcessNodeStyleAssignment, error) {
	var a ProcessNodeStyleAssignment
	var createdAt, updatedAt string
	err := scanner.Scan(
		&a.ID, &a.ProcessNodeID, &a.StyleID, &a.PayloadCode, &a.PayloadDescription,
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

func scanAssignments(rows rowScanner) ([]ProcessNodeStyleAssignment, error) {
	var out []ProcessNodeStyleAssignment
	for rows.Next() {
		a, err := scanAssignment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (db *DB) ListProcessNodeAssignmentsByStyle(styleID int64) ([]ProcessNodeStyleAssignment, error) {
	rows, err := db.Query(`SELECT `+assignmentSelect+` `+assignmentJoin+` WHERE a.style_id=? ORDER BY n.name`, styleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAssignments(rows)
}

func (db *DB) ListProcessNodeAssignmentsByNode(processNodeID int64) ([]ProcessNodeStyleAssignment, error) {
	rows, err := db.Query(`SELECT `+assignmentSelect+` `+assignmentJoin+` WHERE a.process_node_id=? ORDER BY s.name`, processNodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAssignments(rows)
}

func (db *DB) ListProcessNodeAssignmentsByProcess(processID int64) ([]ProcessNodeStyleAssignment, error) {
	query := `SELECT ` + assignmentSelect + ` ` + assignmentJoin
	var rows *sql.Rows
	var err error
	if processID > 0 {
		query += ` WHERE n.process_id=? ORDER BY s.name, n.name`
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

func (db *DB) GetProcessNodeAssignment(id int64) (*ProcessNodeStyleAssignment, error) {
	a, err := scanAssignment(db.QueryRow(`SELECT `+assignmentSelect+` `+assignmentJoin+` WHERE a.id=?`, id))
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (db *DB) GetProcessNodeAssignmentForStyle(processNodeID, styleID int64) (*ProcessNodeStyleAssignment, error) {
	a, err := scanAssignment(db.QueryRow(`SELECT `+assignmentSelect+` `+assignmentJoin+` WHERE a.process_node_id=? AND a.style_id=?`, processNodeID, styleID))
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (db *DB) GetPreferredProcessNodeAssignment(processNodeID int64) (*ProcessNodeStyleAssignment, error) {
	runtime, err := db.EnsureProcessNodeRuntime(processNodeID)
	if err == nil {
		if runtime.ActiveAssignmentID != nil {
			return db.GetProcessNodeAssignment(*runtime.ActiveAssignmentID)
		}
		if runtime.StagedAssignmentID != nil {
			return db.GetProcessNodeAssignment(*runtime.StagedAssignmentID)
		}
	}
	node, err := db.GetProcessNode(processNodeID)
	if err != nil {
		return nil, err
	}
	process, err := db.GetProcess(node.ProcessID)
	if err != nil {
		return nil, err
	}
	if process.ActiveStyleID != nil {
		if a, err := db.GetProcessNodeAssignmentForStyle(processNodeID, *process.ActiveStyleID); err == nil {
			return a, nil
		}
	}
	if process.TargetStyleID != nil {
		if a, err := db.GetProcessNodeAssignmentForStyle(processNodeID, *process.TargetStyleID); err == nil {
			return a, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (db *DB) UpsertProcessNodeAssignment(in ProcessNodeStyleAssignmentInput) (int64, error) {
	existing, err := db.GetProcessNodeAssignmentForStyle(in.ProcessNodeID, in.StyleID)
	if err == nil && existing != nil {
		_, err = db.Exec(`UPDATE process_node_style_assignments SET
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
	res, err := db.Exec(`INSERT INTO process_node_style_assignments (
		process_node_id, style_id, payload_code, payload_description, role, uop_capacity, reorder_point,
		auto_reorder_enabled, cycle_mode, retrieve_empty, requires_manifest_confirmation,
		allows_partial_return, changeover_group, changeover_sequence, changeover_policy
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ProcessNodeID, in.StyleID, in.PayloadCode, in.PayloadDescription, in.Role, in.UOPCapacity, in.ReorderPoint,
		in.AutoReorderEnabled, in.CycleMode, in.RetrieveEmpty, in.RequiresManifestConfirmation,
		in.AllowsPartialReturn, in.ChangeoverGroup, in.ChangeoverSequence, in.ChangeoverPolicy,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) DeleteProcessNodeAssignment(id int64) error {
	_, err := db.Exec(`DELETE FROM process_node_style_assignments WHERE id=?`, id)
	return err
}

func scanRuntime(scanner interface{ Scan(...interface{}) error }) (ProcessNodeRuntimeState, error) {
	var r ProcessNodeRuntimeState
	var loadedAt, updatedAt string
	err := scanner.Scan(
		&r.ID, &r.ProcessNodeID, &r.EffectiveStyleID, &r.ActiveAssignmentID, &r.StagedAssignmentID,
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

func (db *DB) EnsureProcessNodeRuntime(processNodeID int64) (*ProcessNodeRuntimeState, error) {
	r, err := db.GetProcessNodeRuntime(processNodeID)
	if err == nil {
		return r, nil
	}
	_, err = db.Exec(`INSERT INTO process_node_runtime_states (process_node_id) VALUES (?)`, processNodeID)
	if err != nil {
		return nil, err
	}
	return db.GetProcessNodeRuntime(processNodeID)
}

func (db *DB) GetProcessNodeRuntime(processNodeID int64) (*ProcessNodeRuntimeState, error) {
	r, err := scanRuntime(db.QueryRow(`SELECT id, process_node_id, effective_style_id, active_assignment_id, staged_assignment_id,
		loaded_payload_code, material_status, remaining_uop, manifest_status,
		active_order_id, staged_order_id, loaded_bin_label, COALESCE(loaded_at, ''), updated_at
		FROM process_node_runtime_states WHERE process_node_id=?`, processNodeID))
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (db *DB) SetProcessNodeRuntime(processNodeID int64, effectiveStyleID, activeAssignmentID, stagedAssignmentID *int64, payloadCode, materialStatus string, remaining int, manifestStatus string) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET
		effective_style_id=?, active_assignment_id=?, staged_assignment_id=?, loaded_payload_code=?,
		material_status=?, remaining_uop=?, manifest_status=?, updated_at=datetime('now')
		WHERE process_node_id=?`,
		effectiveStyleID, activeAssignmentID, stagedAssignmentID, payloadCode, materialStatus, remaining, manifestStatus, processNodeID)
	return err
}

func (db *DB) UpdateProcessNodeRuntimeOrders(processNodeID int64, activeOrderID, stagedOrderID *int64) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET active_order_id=?, staged_order_id=?, updated_at=datetime('now') WHERE process_node_id=?`,
		activeOrderID, stagedOrderID, processNodeID)
	return err
}

func (db *DB) UpdateProcessNodeManifestStatus(processNodeID int64, manifestStatus string) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET manifest_status=?, updated_at=datetime('now') WHERE process_node_id=?`,
		manifestStatus, processNodeID)
	return err
}

func (db *DB) UpdateProcessNodeMaterialState(processNodeID int64, remaining int, materialStatus string) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET remaining_uop=?, material_status=?, updated_at=datetime('now') WHERE process_node_id=?`,
		remaining, materialStatus, processNodeID)
	return err
}
