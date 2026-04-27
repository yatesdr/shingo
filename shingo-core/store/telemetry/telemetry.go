// Package telemetry holds mission-event + mission_telemetry persistence
// for shingo-core.
//
// Phase 5 of the architecture plan moved mission events, telemetry rows,
// filter + stats queries out of the flat store/ package and into this
// sub-package. The outer store/ keeps type aliases
// (`store.MissionEvent = telemetry.Event`, etc.) and one-line delegate
// methods on *store.DB so external callers see no API change.
//
// Stage 2A.2 lifted the Event, Mission, Filter, and Stats structs into
// shingocore/domain so www handlers + service callers can construct
// query filters and reference response shapes without importing this
// persistence sub-package. The aliases below preserve every existing
// telemetry.X identifier; this file remains the only place
// SQL-touching code for these tables lives.
package telemetry

import (
	"database/sql"
	"fmt"
	"strings"

	"shingocore/domain"
)

// Event, Mission, Filter, and Stats are the telemetry data types. The
// structs live in shingocore/domain (Stage 2A.2); these aliases keep
// the unprefixed telemetry.X names used by every read helper, scan
// function, and Insert/List call site in this package, plus the
// outer store/ re-exports and the www handlers + service callers.
type (
	Event   = domain.TelemetryEvent
	Mission = domain.TelemetryMission
	Filter  = domain.TelemetryFilter
	Stats   = domain.TelemetryStats
)

// InsertEvent writes a mission-event row.
func InsertEvent(db *sql.DB, e *Event) error {
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

// ListEvents returns all mission-event rows for the given order.
func ListEvents(db *sql.DB, orderID int64) ([]*Event, error) {
	rows, err := db.Query(`SELECT id, order_id, vendor_order_id, old_state, new_state,
		robot_id, robot_x, robot_y, robot_angle, robot_battery, robot_station,
		blocks_json, errors_json, detail, created_at
		FROM mission_events WHERE order_id=$1 ORDER BY created_at`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []*Event
	for rows.Next() {
		e := &Event{}
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

// UpsertMission inserts or updates a mission_telemetry summary row.
func UpsertMission(db *sql.DB, t *Mission) error {
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

// GetMission returns the mission_telemetry summary for an order.
func GetMission(db *sql.DB, orderID int64) (*Mission, error) {
	row := db.QueryRow(`SELECT id, order_id, vendor_order_id, robot_id, station_id, order_type,
		source_node, delivery_node, terminal_state,
		vendor_created, vendor_completed, core_created, core_completed,
		duration_ms, vendor_duration_ms,
		blocks_json, errors_json, warnings_json, notices_json, created_at
		FROM mission_telemetry WHERE order_id=$1`, orderID)
	return scanMission(row)
}

func scanMission(row interface{ Scan(...any) error }) (*Mission, error) {
	t := &Mission{}
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

// ListMissions returns mission telemetry rows matching the filter, plus
// the unpaginated total count.
func ListMissions(db *sql.DB, f Filter) ([]*Mission, int, error) {
	where, args := buildWhere(f)

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

	var missions []*Mission
	for rows.Next() {
		t, err := scanMission(rows)
		if err != nil {
			return nil, 0, err
		}
		missions = append(missions, t)
	}
	return missions, total, rows.Err()
}

// GetStats returns aggregated mission metrics for the filter.
func GetStats(db *sql.DB, f Filter) (*Stats, error) {
	where, args := buildWhere(f)

	s := &Stats{}

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

func buildWhere(f Filter) (string, []any) {
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
