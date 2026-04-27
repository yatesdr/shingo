package domain

import "time"

// Changeover is one in-flight (or completed) style change for a
// Process. Tracks who called it, the from/to styles, and the
// transition state machine. StationTask + NodeTask rows hang off
// this row via ProcessChangeoverID.
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
	// Joined fields
	ProcessName   string `json:"process_name"`
	FromStyleName string `json:"from_style_name"`
	ToStyleName   string `json:"to_style_name"`
}

// StationTask is the per-operator-station leg of a Changeover. The
// station's state advances independently of other stations as the
// operator works through their part of the changeover.
type StationTask struct {
	ID                  int64     `json:"id"`
	ProcessChangeoverID int64     `json:"process_changeover_id"`
	OperatorStationID   int64     `json:"operator_station_id"`
	State               string    `json:"state"`
	UpdatedAt           time.Time `json:"updated_at"`
	// Joined fields
	StationName string `json:"station_name"`
}

// NodeTask is the per-process-node leg of a Changeover. Each node
// drives a from-claim → to-claim transition; the orchestration
// layer creates material orders against this task as needed.
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
	// Joined fields
	NodeName string `json:"node_name"`
}
