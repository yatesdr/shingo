package rds

import "encoding/json"

// --- Device types ---

type CallTerminalRequest struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type CallTerminalResponse struct {
	Response
	Data json.RawMessage `json:"data,omitempty"`
}

type DevicesResponse struct {
	Response
	Doors     []DoorStatus     `json:"doors,omitempty"`
	Lifts     []LiftStatus     `json:"lifts,omitempty"`
	Terminals []TerminalStatus `json:"terminals,omitempty"`
}

type DoorStatus struct {
	Name     string              `json:"name"`
	State    int                 `json:"state"`
	Disabled bool                `json:"disabled"`
	Reasons  []UnavailableReason `json:"reasons,omitempty"`
}

type LiftStatus struct {
	Name     string              `json:"name"`
	State    int                 `json:"state"`
	Disabled bool                `json:"disabled"`
	Reasons  []UnavailableReason `json:"reasons,omitempty"`
}

type UnavailableReason struct {
	Reason string `json:"reason"`
}

type TerminalStatus struct {
	ID    string `json:"id"`
	State int    `json:"state"`
}

type CallDoorRequest struct {
	Name  string `json:"name"`
	State int    `json:"state"`
}

type DisableDeviceRequest struct {
	Names    []string `json:"names"`
	Disabled bool     `json:"disabled"`
}

type CallLiftRequest struct {
	Name       string `json:"name"`
	TargetArea string `json:"target_area"`
}
