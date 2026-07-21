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

	// UnresolvedParticipants is a transient, non-persisted advisory set only
	// on the StartProcessChangeover response: participant nodes whose
	// core_node_name resolves to NO process_nodes row.
	//
	// These are almost always press-index extension seats — physically
	// traversed by the index motion, owning no task and no order, and (before
	// participants existed) invisible to every consumer. A node with no row
	// cannot be gated, rendered, or released, so naming it here is the point:
	// it is a config gap the engineer fixes on the process-nodes page.
	//
	// ADVISORY ONLY. The changeover still starts. A hard refusal on day one
	// could block every same-bin-type press-index changeover at a plant that
	// has never had rows for those seats -- and the 2026-06-03 Springfield
	// softening is the standing evidence that a guard the floor cannot work
	// around gets disabled rather than fixed. Hardening waits on the V.4 audit
	// returning clean at both plants AND the affordance widening shipping.
	UnresolvedParticipants []string `json:"unresolved_participants,omitempty"`
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

// Blocker is one reason a changeover cannot cut over yet, in a shape the HMI
// can render as a live panel instead of a one-shot toast string.
//
// Reason is the FULL human sentence and is the only field the operator reads;
// NodeName and OrderID are the same fact in structured form so the panel can
// link a blocker to the tile or order it names. Exactly one of them is set,
// matching which conjunct produced the blocker.
//
// Hard is DISPLAY-ONLY. There is no override — the cutover gate has no
// operator bypass and none is planned, because an out-of-order style flip is
// unrecoverable (see completeCutover's irreversibility comment). The flag
// exists so the panel can distinguish "this will clear itself" from "this
// needs you" once the gate grows a soft conjunct; today every blocker is hard.
// Do not grow a second code path off it.
type Blocker struct {
	Reason   string `json:"reason"`
	NodeName string `json:"node_name,omitempty"`
	OrderID  int64  `json:"order_id,omitempty"`
	Hard     bool   `json:"hard"`
}

// BlockersToReasons projects blockers back to the flat sentence list the
// click-time error path has always produced.
//
// This exists to keep the 400 toast BYTE-IDENTICAL across the structured-
// blocker change: Blocker.Reason holds the same string the old []string
// carried, so joining these reproduces the previous message exactly. The
// structured fields are additive metadata for the panel, never a re-render of
// the sentence — if a caller ever needs different wording it composes its own
// rather than editing Reason, or the toast and the panel drift.
func BlockersToReasons(blockers []Blocker) []string {
	if len(blockers) == 0 {
		return nil
	}
	reasons := make([]string, 0, len(blockers))
	for _, b := range blockers {
		reasons = append(reasons, b.Reason)
	}
	return reasons
}
