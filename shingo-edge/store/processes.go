package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

type ProcessFlowStep struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

var allowedProcessFlowKinds = map[string]string{
	"runout":      "Runout",
	"tool_change": "Tool Change",
	"release":     "Release",
	"cutover":     "Start New Style",
	"verify":      "Verify",
}

func DefaultProcessFlow() []ProcessFlowStep {
	return []ProcessFlowStep{
		{Key: "runout", Label: "Runout", Kind: "runout"},
		{Key: "tool_change", Label: "Tool Change", Kind: "tool_change"},
		{Key: "release", Label: "Release", Kind: "release"},
		{Key: "cutover", Label: "Start New Style", Kind: "cutover"},
		{Key: "verify", Label: "Verify", Kind: "verify"},
	}
}

func parseProcessFlow(raw string) []ProcessFlowStep {
	if raw == "" {
		return DefaultProcessFlow()
	}
	var flow []ProcessFlowStep
	if err := json.Unmarshal([]byte(raw), &flow); err != nil || len(flow) == 0 {
		return DefaultProcessFlow()
	}
	return SanitizeProcessFlow(flow)
}

func marshalProcessFlow(flow []ProcessFlowStep) string {
	flow = SanitizeProcessFlow(flow)
	b, _ := json.Marshal(flow)
	return string(b)
}

func SanitizeProcessFlow(flow []ProcessFlowStep) []ProcessFlowStep {
	if len(flow) == 0 {
		return DefaultProcessFlow()
	}
	used := make(map[string]bool, len(flow))
	out := make([]ProcessFlowStep, 0, len(flow))
	for _, step := range flow {
		if _, ok := allowedProcessFlowKinds[step.Kind]; !ok {
			continue
		}
		if used[step.Kind] {
			continue
		}
		used[step.Kind] = true
		key := step.Key
		if key == "" {
			key = step.Kind
		}
		label := step.Label
		if label == "" {
			label = allowedProcessFlowKinds[step.Kind]
		}
		out = append(out, ProcessFlowStep{Key: key, Label: label, Kind: step.Kind})
	}
	if len(out) == 0 {
		return DefaultProcessFlow()
	}
	if ProcessFlowIndex(out, "cutover") < 0 {
		insertAt := len(out)
		if verifyIdx := ProcessFlowIndex(out, "verify"); verifyIdx >= 0 {
			insertAt = verifyIdx
		}
		cutover := ProcessFlowStep{Key: "cutover", Label: allowedProcessFlowKinds["cutover"], Kind: "cutover"}
		out = append(out, ProcessFlowStep{})
		copy(out[insertAt+1:], out[insertAt:])
		out[insertAt] = cutover
	}
	return out
}

func ProcessFlowIndex(flow []ProcessFlowStep, kind string) int {
	for i, step := range SanitizeProcessFlow(flow) {
		if step.Kind == kind {
			return i
		}
	}
	return -1
}

func NextProcessFlowStep(flow []ProcessFlowStep, current string) *ProcessFlowStep {
	sanitized := SanitizeProcessFlow(flow)
	for i, step := range sanitized {
		if step.Kind == current && i+1 < len(sanitized) {
			next := sanitized[i+1]
			return &next
		}
	}
	return nil
}

// Process represents a production process (physical production area).
type Process struct {
	ID              int64             `json:"id"`
	Name            string            `json:"name"`
	Description     string            `json:"description"`
	ActiveStyleID   *int64            `json:"active_style_id"`
	TargetStyleID   *int64            `json:"target_style_id,omitempty"`
	ProductionState string            `json:"production_state"`
	CutoverMode     string            `json:"cutover_mode"`
	ChangeoverFlow  []ProcessFlowStep `json:"changeover_flow"`
	CreatedAt       time.Time         `json:"created_at"`
}

type ProcessCounterBinding struct {
	ID               int64     `json:"id"`
	ProcessID        int64     `json:"process_id"`
	ReportingPointID *int64    `json:"reporting_point_id,omitempty"`
	PLCName          string    `json:"plc_name"`
	TagName          string    `json:"tag_name"`
	Enabled          bool      `json:"enabled"`
	WarlinkManaged   bool      `json:"warlink_managed"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func scanProcess(scanner interface{ Scan(...interface{}) error }) (Process, error) {
	var p Process
	var createdAt, flowRaw string
	if err := scanner.Scan(&p.ID, &p.Name, &p.Description, &p.ActiveStyleID, &p.TargetStyleID, &p.ProductionState, &p.CutoverMode, &flowRaw, &createdAt); err != nil {
		return p, err
	}
	p.ChangeoverFlow = parseProcessFlow(flowRaw)
	p.CreatedAt = scanTime(createdAt)
	return p, nil
}

func (db *DB) ListProcesses() ([]Process, error) {
	rows, err := db.Query(`SELECT id, name, description, active_job_style_id, target_job_style_id, production_state, cutover_mode, changeover_flow_json, created_at FROM processes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []Process
	for rows.Next() {
		l, err := scanProcess(rows)
		if err != nil {
			return nil, err
		}
		lines = append(lines, l)
	}
	return lines, rows.Err()
}

func (db *DB) GetProcess(id int64) (*Process, error) {
	l, err := scanProcess(db.QueryRow(`SELECT id, name, description, active_job_style_id, target_job_style_id, production_state, cutover_mode, changeover_flow_json, created_at FROM processes WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (db *DB) CreateProcess(name, description, productionState, cutoverMode string, flow []ProcessFlowStep) (int64, error) {
	if productionState == "" {
		productionState = "active_production"
	}
	if cutoverMode == "" {
		cutoverMode = "manual"
	}
	res, err := db.Exec(`INSERT INTO processes (name, description, production_state, cutover_mode, changeover_flow_json) VALUES (?, ?, ?, ?, ?)`,
		name, description, productionState, cutoverMode, marshalProcessFlow(flow))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateProcess(id int64, name, description, productionState, cutoverMode string, flow []ProcessFlowStep) error {
	if productionState == "" {
		productionState = "active_production"
	}
	if cutoverMode == "" {
		cutoverMode = "manual"
	}
	_, err := db.Exec(`UPDATE processes SET name=?, description=?, production_state=?, cutover_mode=?, changeover_flow_json=? WHERE id=?`,
		name, description, productionState, cutoverMode, marshalProcessFlow(flow), id)
	return err
}

func (db *DB) DeleteProcess(id int64) error {
	_, err := db.Exec(`DELETE FROM processes WHERE id=?`, id)
	return err
}

func (db *DB) SetActiveStyle(lineID int64, styleID *int64) error {
	_, err := db.Exec(`UPDATE processes SET active_job_style_id=? WHERE id=?`, styleID, lineID)
	return err
}

func (db *DB) SetTargetStyle(processID int64, styleID *int64) error {
	_, err := db.Exec(`UPDATE processes SET target_job_style_id=? WHERE id=?`, styleID, processID)
	return err
}

func (db *DB) GetActiveStyleID(lineID int64) (*int64, error) {
	var id *int64
	err := db.QueryRow(`SELECT active_job_style_id FROM processes WHERE id = ?`, lineID).Scan(&id)
	if err != nil {
		return nil, err
	}
	return id, nil
}

func (db *DB) SetProcessProductionState(processID int64, state string) error {
	_, err := db.Exec(`UPDATE processes SET production_state=? WHERE id=?`, state, processID)
	return err
}

func scanProcessCounterBinding(scanner interface{ Scan(...interface{}) error }) (ProcessCounterBinding, error) {
	var b ProcessCounterBinding
	var reportingPointID sql.NullInt64
	var createdAt, updatedAt string
	if err := scanner.Scan(&b.ID, &b.ProcessID, &reportingPointID, &b.PLCName, &b.TagName, &b.Enabled, &b.WarlinkManaged, &createdAt, &updatedAt); err != nil {
		return b, err
	}
	if reportingPointID.Valid {
		b.ReportingPointID = &reportingPointID.Int64
	}
	b.CreatedAt = scanTime(createdAt)
	b.UpdatedAt = scanTime(updatedAt)
	return b, nil
}

func (db *DB) GetProcessCounterBinding(processID int64) (*ProcessCounterBinding, error) {
	b, err := scanProcessCounterBinding(db.QueryRow(`SELECT id, process_id, reporting_point_id, plc_name, tag_name, enabled, warlink_managed, created_at, updated_at FROM process_counter_bindings WHERE process_id=?`, processID))
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (db *DB) UpsertProcessCounterBinding(processID int64, plcName, tagName string, enabled bool) (int64, error) {
	existing, err := db.GetProcessCounterBinding(processID)
	if err == nil && existing != nil {
		_, err = db.Exec(`UPDATE process_counter_bindings SET plc_name=?, tag_name=?, enabled=?, updated_at=datetime('now') WHERE process_id=?`, plcName, tagName, enabled, processID)
		return existing.ID, err
	}
	res, err := db.Exec(`INSERT INTO process_counter_bindings (process_id, plc_name, tag_name, enabled) VALUES (?, ?, ?, ?)`, processID, plcName, tagName, enabled)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateProcessCounterReportingPoint(processID int64, reportingPointID *int64, managed bool) error {
	_, err := db.Exec(`UPDATE process_counter_bindings SET reporting_point_id=?, warlink_managed=?, updated_at=datetime('now') WHERE process_id=?`, reportingPointID, managed, processID)
	return err
}
