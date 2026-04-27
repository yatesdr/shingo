package domain

import "time"

// Station is one operator station on the edge — physical area that an
// operator manages. Holds device + health state plus the area label
// and sequence used to render the HMI.
type Station struct {
	ID               int64      `json:"id"`
	ProcessID        int64      `json:"process_id"`
	Code             string     `json:"code"`
	Name             string     `json:"name"`
	Note             string     `json:"note"`
	AreaLabel        string     `json:"area_label"`
	Sequence         int        `json:"sequence"`
	ControllerNodeID string     `json:"controller_node_id"`
	DeviceMode       string     `json:"device_mode"`
	Enabled          bool       `json:"enabled"`
	HealthStatus     string     `json:"health_status"`
	LastSeenAt       *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	// Joined fields
	ProcessName string `json:"process_name"`
}

// StationInput is the request shape for creating or updating a Station
// — the persisted fields minus ID, timestamps, joined columns, and
// server-managed health state.
type StationInput struct {
	ProcessID        int64  `json:"process_id"`
	Code             string `json:"code"`
	Name             string `json:"name"`
	Note             string `json:"note"`
	AreaLabel        string `json:"area_label"`
	Sequence         int    `json:"sequence"`
	ControllerNodeID string `json:"controller_node_id"`
	DeviceMode       string `json:"device_mode"`
	Enabled          bool   `json:"enabled"`
}
