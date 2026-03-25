package store

import (
	"fmt"
	"strings"
	"time"
)

// MissionEvent records a single state transition during a mission,
// including a robot position snapshot at that moment.
type MissionEvent struct {
	ID            int64    `json:"id"`
	OrderID       int64    `json:"order_id"`
	VendorOrderID string   `json:"vendor_order_id"`
	OldState      string   `json:"old_state"`
	NewState      string   `json:"new_state"`
	RobotID       string   `json:"robot_id"`
	RobotX        *float64 `json:"robot_x,omitempty"`
	RobotY        *float64 `json:"robot_y,omitempty"`
	RobotAngle    *float64 `json:"robot_angle,omitempty"`
	RobotBattery  *float64 `json:"robot_battery,omitempty"`
	RobotStation  string   `json:"robot_station"`
	BlocksJSON    string   `json:"blocks_json"`
	ErrorsJSON    string   `json:"errors_json"`
	Detail        string   `json:"detail"`
	CreatedAt     time.Time `json:"created_at"`
}

// MissionTelemetry is the summary row for a completed mission.
type MissionTelemetry struct {
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

// MissionFilter supports filtered queries of mission_telemetry.
type MissionFilter struct {
	StationID string
	RobotID   string
	State     string // terminal state filter
	Since     *time.Time
	Until     *time.Time
	Limit     int
	Offset    int
}

// MissionStats provides aggregated mission metrics.
type MissionStats struct {
	TotalMissions int64   `json:"total_missions"`
	Completed     int64   `json:"completed"`
	Failed        int64   `json:"failed"`
	Cancelled     int64   `json:"cancelled"`
	AvgDurationMS int64   `json:"avg_duration_ms"`
	P50DurationMS int64   `json:"p50_duration_ms"`
	P95DurationMS int64   `json:"p95_duration_ms"`
	SuccessRate   float64 `json:"success_rate"`
}

func (db *DB) InsertMissionEvent(e *MissionEvent) error {
	_, err := db.Exec(`INSERT INTO mission_events
		(order_id, vendor_order_id, old_state, new_state, robot_id,
		 robot_x, robot_y, robot_angle, robot_battery, robot_station,
		 blocks_json, errors_json, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		e.OrderID, e.VendorOrderID, e.OldState, e.NewState, e.RobotID,
		e.RobotX, e.RobotY, e.RobotAngle, e.RobotBattery, e.RobotStation,
		e.BlocksJSON, e.ErrorsJSON, e.Detail)
	if err != nil {
		return fmt.Errorf("insert mission event: %w", err)
	}
	return nil
}

func (db *DB) ListMissionEvents(orderID int64) ([]*MissionEvent, error) {
	rows, err := db.Query(`SELECT id, order_id, vendor_order_id, old_state, new_state,
		robot_id, robot_x, robot_y, robot_angle, robot_battery, robot_station,
		blocks_json, errors_json, detail, created_at
		FROM mission_events WHERE order_id=$1 ORDER BY created_at`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []*MissionEvent
	for rows.Next() {
		e := &MissionEvent{}
		err := rows.Scan(&e.ID, &e.OrderID, &e.VendorOrderID, &e.OldState, &e.NewState,
			&e.RobotID, &e.RobotX, &e.RobotY, &e.RobotAngle, &e.RobotBattery, &e.RobotStation,
			&e.BlocksJSON, &e.ErrorsJSON, &e.Detail, &e.CreatedAt)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (db *DB) UpsertMissionTelemetry(t *MissionTelemetry) error {
	_, err := db.Exec(`INSERT INTO mission_telemetry
		(order_id, vendor_order_id, robot_id, station_id, order_type,
		 source_node, delivery_node, terminal_state,
		 vendor_created, vendor_completed, core_created, core_completed,
		 duration_ms, vendor_duration_ms,
		 blocks_json, errors_json, warnings_json, notices_json)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		ON CONFLICT (order_id) DO UPDATE SET
		 robot_id=EXCLUDED.robot_id, terminal_state=EXCLUDED.terminal_state,
		 vendor_created=EXCLUDED.vendor_created, vendor_completed=EXCLUDED.vendor_completed,
		 core_completed=EXCLUDED.core_completed,
		 duration_ms=EXCLUDED.duration_ms, vendor_duration_ms=EXCLUDED.vendor_duration_ms,
		 blocks_json=EXCLUDED.blocks_json, errors_json=EXCLUDED.errors_json,
		 warnings_json=EXCLUDED.warnings_json, notices_json=EXCLUDED.notices_json`,
		t.OrderID, t.VendorOrderID, t.RobotID, t.StationID, t.OrderType,
		t.SourceNode, t.DeliveryNode, t.TerminalState,
		t.VendorCreated, t.VendorCompleted, t.CoreCreated, t.CoreCompleted,
		t.DurationMS, t.VendorDurationMS,
		t.BlocksJSON, t.ErrorsJSON, t.WarningsJSON, t.NoticesJSON)
	if err != nil {
		return fmt.Errorf("upsert mission telemetry: %w", err)
	}
	return nil
}

func (db *DB) GetMissionTelemetry(orderID int64) (*MissionTelemetry, error) {
	row := db.QueryRow(`SELECT id, order_id, vendor_order_id, robot_id, station_id, order_type,
		source_node, delivery_node, terminal_state,
		vendor_created, vendor_completed, core_created, core_completed,
		duration_ms, vendor_duration_ms,
		blocks_json, errors_json, warnings_json, notices_json, created_at
		FROM mission_telemetry WHERE order_id=$1`, orderID)
	return scanMissionTelemetry(row)
}

func scanMissionTelemetry(row interface{ Scan(...any) error }) (*MissionTelemetry, error) {
	t := &MissionTelemetry{}
	err := row.Scan(&t.ID, &t.OrderID, &t.VendorOrderID, &t.RobotID, &t.StationID, &t.OrderType,
		&t.SourceNode, &t.DeliveryNode, &t.TerminalState,
		&t.VendorCreated, &t.VendorCompleted, &t.CoreCreated, &t.CoreCompleted,
		&t.DurationMS, &t.VendorDurationMS,
		&t.BlocksJSON, &t.ErrorsJSON, &t.WarningsJSON, &t.NoticesJSON, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (db *DB) ListMissions(f MissionFilter) ([]*MissionTelemetry, int, error) {
	where, args := buildMissionWhere(f)

	// Get total count
	var total int
	countQuery := "SELECT COUNT(*) FROM mission_telemetry" + where
	if err := db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}

	query := fmt.Sprintf(`SELECT id, order_id, vendor_order_id, robot_id, station_id, order_type,
		source_node, delivery_node, terminal_state,
		vendor_created, vendor_completed, core_created, core_completed,
		duration_ms, vendor_duration_ms,
		blocks_json, errors_json, warnings_json, notices_json, created_at
		FROM mission_telemetry%s ORDER BY core_completed DESC NULLS LAST LIMIT $%d OFFSET $%d`,
		where, len(args)+1, len(args)+2)
	args = append(args, limit, f.Offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var missions []*MissionTelemetry
	for rows.Next() {
		t, err := scanMissionTelemetry(rows)
		if err != nil {
			return nil, 0, err
		}
		missions = append(missions, t)
	}
	return missions, total, rows.Err()
}

func (db *DB) GetMissionStats(f MissionFilter) (*MissionStats, error) {
	where, args := buildMissionWhere(f)

	s := &MissionStats{}

	// Total and state counts
	countQuery := fmt.Sprintf(`SELECT
		COUNT(*),
		COUNT(*) FILTER (WHERE terminal_state IN ('FINISHED', 'delivered', 'confirmed')),
		COUNT(*) FILTER (WHERE terminal_state IN ('FAILED', 'failed')),
		COUNT(*) FILTER (WHERE terminal_state IN ('STOPPED', 'cancelled'))
		FROM mission_telemetry%s`, where)
	if err := db.QueryRow(countQuery, args...).Scan(&s.TotalMissions, &s.Completed, &s.Failed, &s.Cancelled); err != nil {
		return nil, err
	}

	if s.TotalMissions == 0 {
		return s, nil
	}

	if s.Completed > 0 {
		s.SuccessRate = float64(s.Completed) / float64(s.TotalMissions) * 100
	}

	// Duration stats (only for orders with positive duration)
	durWhere := where
	if durWhere == "" {
		durWhere = " WHERE duration_ms > 0"
	} else {
		durWhere += " AND duration_ms > 0"
	}
	durQuery := fmt.Sprintf(`SELECT
		COALESCE(AVG(duration_ms), 0)::BIGINT,
		COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms), 0)::BIGINT,
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms), 0)::BIGINT
		FROM mission_telemetry%s`, durWhere)
	db.QueryRow(durQuery, args...).Scan(&s.AvgDurationMS, &s.P50DurationMS, &s.P95DurationMS)

	return s, nil
}

func buildMissionWhere(f MissionFilter) (string, []any) {
	var conds []string
	var args []any
	n := 0

	add := func(cond string, val any) {
		n++
		conds = append(conds, fmt.Sprintf(cond, n))
		args = append(args, val)
	}

	if f.StationID != "" {
		add("station_id=$%d", f.StationID)
	}
	if f.RobotID != "" {
		add("robot_id=$%d", f.RobotID)
	}
	if f.State != "" {
		add("terminal_state=$%d", f.State)
	}
	if f.Since != nil {
		add("core_completed >= $%d", *f.Since)
	}
	if f.Until != nil {
		add("core_completed <= $%d", *f.Until)
	}

	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

