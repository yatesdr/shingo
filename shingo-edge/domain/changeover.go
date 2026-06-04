package domain

import "time"

// Changeover is one in-flight (or completed) style change for a
// Process. Tracks who called it, the from/to styles, and the
// transition state machine. StationTask + NodeTask rows hang off
// this row via ProcessChangeoverID.
type Changeover struct {
	ID          int64           `json:"id"`
	ProcessID   int64           `json:"process_id"`
	FromStyleID *int64          `json:"from_style_id,omitempty"`
	ToStyleID   int64           `json:"to_style_id"`
	State       ChangeoverState `json:"state"`
	CalledBy    string          `json:"called_by"`
	Notes       string          `json:"notes"`
	StartedAt   time.Time       `json:"started_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	// TriggeredBy records the trigger source that drove the row to its
	// current terminal state. Empty while in_progress; one of
	// "operator-hmi" | "plc-auto" | "auto-task-terminal" once
	// completed/cancelled. Audit-only: differentiates operator-driven
	// cutover from PLC-driven cutover from the B.3 auto-completion
	// path that fires when terminal task transitions land while the
	// gate was open.
	TriggeredBy string    `json:"triggered_by,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Joined fields
	ProcessName   string `json:"process_name"`
	FromStyleName string `json:"from_style_name"`
	ToStyleName   string `json:"to_style_name"`

	// AwaitingStock is a transient, non-persisted advisory set only on the
	// StartProcessChangeover response: the required payload codes that had
	// zero available supermarket bins at start time. The changeover still
	// starts — these supply legs park as "Awaiting Stock" and dispatch
	// automatically once the operator loads + manifest-confirms the bins.
	// Not stored and not re-emitted on refresh; the live per-order "Awaiting
	// Stock" status is the durable signal. Empty/omitted when fully stocked.
	AwaitingStock []string `json:"awaiting_stock,omitempty"`
}

// StationTask is the per-operator-station leg of a Changeover. The
// station's state advances independently of other stations as the
// operator works through their part of the changeover.
type StationTask struct {
	ID                  int64            `json:"id"`
	ProcessChangeoverID int64            `json:"process_changeover_id"`
	OperatorStationID   int64            `json:"operator_station_id"`
	State               StationTaskState `json:"state"`
	UpdatedAt           time.Time        `json:"updated_at"`
	// Joined fields
	StationName string `json:"station_name"`
}

// NodeTask is the per-process-node leg of a Changeover. Each node
// drives a from-claim → to-claim transition; the orchestration
// layer creates material orders against this task as needed.
type NodeTask struct {
	ID                        int64         `json:"id"`
	ProcessChangeoverID       int64         `json:"process_changeover_id"`
	ProcessNodeID             int64         `json:"process_node_id"`
	FromClaimID               *int64        `json:"from_claim_id,omitempty"`
	ToClaimID                 *int64        `json:"to_claim_id,omitempty"`
	Situation                 string        `json:"situation"`
	State                     NodeTaskState `json:"state"`
	NextMaterialOrderID       *int64        `json:"next_material_order_id,omitempty"`
	OldMaterialReleaseOrderID *int64        `json:"old_material_release_order_id,omitempty"`
	// SkipNote is the operator-facing message set when a linked complex
	// order reached terminal "skipped" (Core's no_source_bin path) —
	// typically "evac skipped: bin missing at <node>". Empty when no
	// skip has been recorded. Cleared by the next state-advancing
	// operator action so the chip disappears once recovery happens.
	SkipNote  string    `json:"skip_note,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	// Joined fields
	NodeName string `json:"node_name"`
}

// IsNodeTaskStateTerminal is the function form of NodeTaskState.IsTerminal,
// retained so callers holding a NodeTaskState (formerly: a raw string)
// can use either form. See NodeTaskState.IsTerminal for the full predicate
// rationale.
func IsNodeTaskStateTerminal(state NodeTaskState, situation string) bool {
	return state.IsTerminal(situation)
}
