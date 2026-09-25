package domain

import "time"

// PayloadContainmentRow is one payload's containment state as the UI reads it.
// The row persists after deactivation so the alert's history survives it.
//
// Declared here rather than in store because the containment handlers name it
// and www may not import store; store keeps the name through an alias.
type PayloadContainmentRow struct {
	PayloadCode   string     `json:"payload_code"`
	Active        bool       `json:"active"`
	Reason        string     `json:"reason"`
	ActivatedBy   string     `json:"activated_by"`
	ActivatedAt   *time.Time `json:"activated_at"`
	DeactivatedBy string     `json:"deactivated_by"`
	DeactivatedAt *time.Time `json:"deactivated_at"`
}

// HeldBinRow is one held bin as the containment screens read it. The bin's
// location is carried as the node name only: the screens render the name, and
// the Edge's mirror of this row (engine.HeldBinRow) has no node id to read.
type HeldBinRow struct {
	BinID       int64      `json:"bin_id"`
	Label       string     `json:"label"`
	PayloadCode string     `json:"payload_code"`
	NodeName    string     `json:"node_name"`
	HoldBy      string     `json:"hold_by"`
	HoldAt      *time.Time `json:"hold_at"`
}
