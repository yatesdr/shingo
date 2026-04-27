package domain

import "time"

// TelemetryEvent records a single state transition during a mission,
// including a robot position snapshot at that moment. Persisted via
// shingo-core/store/telemetry; lives in domain/ so handlers building
// mission-detail responses don't have to import the telemetry
// sub-package.
//
// Stage 2A.2 relocation. The store/telemetry package re-exports this
// type via `type Event = domain.TelemetryEvent`.
type TelemetryEvent struct {
	ID            int64     `json:"id"`
	OrderID       int64     `json:"order_id"`
	VendorOrderID string    `json:"vendor_order_id"`
	OldState      string    `json:"old_state"`
	NewState      string    `json:"new_state"`
	RobotID       string    `json:"robot_id"`
	RobotX        *float64  `json:"robot_x,omitempty"`
	RobotY        *float64  `json:"robot_y,omitempty"`
	RobotAngle    *float64  `json:"robot_angle,omitempty"`
	RobotBattery  *float64  `json:"robot_battery,omitempty"`
	RobotStation  string    `json:"robot_station"`
	BlocksJSON    string    `json:"blocks_json"`
	ErrorsJSON    string    `json:"errors_json"`
	Detail        string    `json:"detail"`
	CreatedAt     time.Time `json:"created_at"`
}

// TelemetryMission is the summary row for a completed mission. One
// row per completed Order, stamped at terminal state with vendor +
// core durations and serialised block / error / warning / notice
// payloads for post-hoc inspection.
//
// Stage 2A.2 relocation. The store/telemetry package re-exports this
// type via `type Mission = domain.TelemetryMission`.
type TelemetryMission struct {
	ID               int64      `json:"id"`
	OrderID          int64      `json:"order_id"`
	VendorOrderID    string     `json:"vendor_order_id"`
	RobotID          string     `json:"robot_id"`
	StationID        string     `json:"station_id"`
	OrderType        string     `json:"order_type"`
	SourceNode       string     `json:"source_node"`
	DeliveryNode     string     `json:"delivery_node"`
	TerminalState    string     `json:"terminal_state"`
	VendorCreated    *time.Time `json:"vendor_created,omitempty"`
	VendorCompleted  *time.Time `json:"vendor_completed,omitempty"`
	CoreCreated      *time.Time `json:"core_created,omitempty"`
	CoreCompleted    *time.Time `json:"core_completed,omitempty"`
	DurationMS       int64      `json:"duration_ms"`
	VendorDurationMS int64      `json:"vendor_duration_ms"`
	BlocksJSON       string     `json:"blocks_json"`
	ErrorsJSON       string     `json:"errors_json"`
	WarningsJSON     string     `json:"warnings_json"`
	NoticesJSON      string     `json:"notices_json"`
	CreatedAt        time.Time  `json:"created_at"`
}

// TelemetryFilter is the query DSL for mission-telemetry lookups. Lives
// in domain/ rather than store/telemetry because handlers parse HTTP
// query parameters into a filter and pass it through the service
// layer — the type is the contract between handler and service, not a
// persistence detail.
//
// Stage 2A.2 relocation. The store/telemetry package re-exports this
// type via `type Filter = domain.TelemetryFilter`.
type TelemetryFilter struct {
	StationID string
	RobotID   string
	State     string
	Since     *time.Time
	Until     *time.Time
	Limit     int
	Offset    int
}

// TelemetryStats provides aggregated mission metrics over a
// (typically) date-bounded set of TelemetryMission rows.
//
// Stage 2A.2 relocation. The store/telemetry package re-exports this
// type via `type Stats = domain.TelemetryStats`.
type TelemetryStats struct {
	TotalMissions int64   `json:"total_missions"`
	Completed     int64   `json:"completed"`
	Failed        int64   `json:"failed"`
	Cancelled     int64   `json:"cancelled"`
	AvgDurationMS int64   `json:"avg_duration_ms"`
	P50DurationMS int64   `json:"p50_duration_ms"`
	P95DurationMS int64   `json:"p95_duration_ms"`
	SuccessRate   float64 `json:"success_rate"`
}
