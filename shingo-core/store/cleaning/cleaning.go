// Package cleaning stores the air-knife cleaning loop's setup and record
// (migration v158): the one mission a plant sets up on the Robots page, a row
// per cleaning order sent to the fleet, and the robots never to clean.
//
// Pure SQL on *sql.DB. The engine's loop (engine/cleaning.go) calls it
// directly, the way the confidence sampler calls robotconfidence.InsertBatch;
// the read side reaches it through the delegates in store/cleaning.go.
package cleaning

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Mission states. A row is open while it is StateDispatched; the loop moves it
// to one of the other three exactly once (Finalize refuses anything else).
const (
	StateDispatched = "dispatched"
	StateFinished   = "finished"
	StateCancelled  = "cancelled"
	StateFailed     = "failed"
)

// Why a mission was sent.
const (
	ReasonInterval = "interval"
	ReasonManual   = "manual"
)

// Retention is how long a mission row is kept.
const Retention = 90 * 24 * time.Hour

// Setup is the one cleaning mission a plant has. ActiveFrom and ActiveTo are
// plant-local wall-clock times "HH:MM"; the zone is planttime's, not stored.
type Setup struct {
	Points     []string
	Frequency  time.Duration
	ActiveFrom string
	ActiveTo   string
	MinBattery int
	UpdatedAt  time.Time
}

// GetSetup reads the setup row. ok is false when no mission has ever been
// saved, which is every plant until someone sets one up.
//
// points comes back through array_to_json rather than as a Postgres array
// literal: a point name is the fleet's, and nothing promises it is free of the
// commas and quotes a literal would have to be parsed around.
func GetSetup(db *sql.DB) (Setup, bool, error) {
	var (
		s      Setup
		points string
		secs   int64
	)
	err := db.QueryRow(`SELECT array_to_json(points)::text,
			EXTRACT(EPOCH FROM frequency)::bigint,
			to_char(active_from, 'HH24:MI'), to_char(active_to, 'HH24:MI'),
			min_battery, updated_at
		FROM cleaning_mission_setup WHERE id = 1`).
		Scan(&points, &secs, &s.ActiveFrom, &s.ActiveTo, &s.MinBattery, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Setup{}, false, nil
	}
	if err != nil {
		return Setup{}, false, fmt.Errorf("cleaning setup: %w", err)
	}
	if err := json.Unmarshal([]byte(points), &s.Points); err != nil {
		return Setup{}, false, fmt.Errorf("cleaning setup points: %w", err)
	}
	if s.Points == nil {
		s.Points = []string{}
	}
	s.Frequency = time.Duration(secs) * time.Second
	return s, true, nil
}

// SaveSetup writes the setup row, replacing whatever was there, and stamps
// updated_at. Validation is the caller's (engine.SaveCleaningSetup): this
// stores what it is given.
func SaveSetup(db *sql.DB, s Setup) error {
	points := s.Points
	if points == nil {
		points = []string{}
	}
	_, err := db.Exec(`INSERT INTO cleaning_mission_setup
			(id, points, frequency, active_from, active_to, min_battery, updated_at)
		VALUES (1, $1, make_interval(secs => $2), $3::time, $4::time, $5, now())
		ON CONFLICT (id) DO UPDATE SET
			points = EXCLUDED.points, frequency = EXCLUDED.frequency,
			active_from = EXCLUDED.active_from, active_to = EXCLUDED.active_to,
			min_battery = EXCLUDED.min_battery, updated_at = EXCLUDED.updated_at`,
		points, s.Frequency.Seconds(), s.ActiveFrom, s.ActiveTo, s.MinBattery)
	if err != nil {
		return fmt.Errorf("cleaning setup save: %w", err)
	}
	return nil
}

// Mission is one cleaning order sent to the fleet.
type Mission struct {
	ID            int64
	VehicleID     string
	VendorOrderID string
	State         string
	Reason        string
	DispatchedAt  time.Time
	FinishedAt    *time.Time
}

const missionCols = `id, vehicle_id, vendor_order_id, state, reason, dispatched_at, finished_at`

func scanMissions(rows *sql.Rows) ([]Mission, error) {
	defer rows.Close()
	out := []Mission{}
	for rows.Next() {
		var m Mission
		if err := rows.Scan(&m.ID, &m.VehicleID, &m.VendorOrderID, &m.State, &m.Reason,
			&m.DispatchedAt, &m.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecordDispatch stores a mission the fleet has accepted, open.
func RecordDispatch(db *sql.DB, vehicleID, vendorOrderID, reason string, at time.Time) (int64, error) {
	var id int64
	err := db.QueryRow(`INSERT INTO cleaning_missions (vehicle_id, vendor_order_id, state, reason, dispatched_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		vehicleID, vendorOrderID, StateDispatched, reason, at).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("cleaning record dispatch: %w", err)
	}
	return id, nil
}

// OpenMissions lists the missions still dispatched, oldest first.
func OpenMissions(db *sql.DB) ([]Mission, error) {
	rows, err := db.Query(`SELECT `+missionCols+` FROM cleaning_missions
		WHERE state = $1 ORDER BY dispatched_at`, StateDispatched)
	if err != nil {
		return nil, fmt.Errorf("cleaning open missions: %w", err)
	}
	return scanMissions(rows)
}

// AnyOpen reports whether any mission is still dispatched, fleet-wide. One
// robot at the knife at a time: the open row is that fact.
func AnyOpen(db *sql.DB) (bool, error) {
	var open bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM cleaning_missions WHERE state = $1)`,
		StateDispatched).Scan(&open); err != nil {
		return false, fmt.Errorf("cleaning any open: %w", err)
	}
	return open, nil
}

// Finalize closes an open mission in state, which must be one of finished,
// cancelled or failed. A row that is no longer dispatched is left alone and
// reported as not changed, so a second finalizer cannot rewrite the first's
// answer.
func Finalize(db *sql.DB, id int64, state string, at time.Time) (bool, error) {
	switch state {
	case StateFinished, StateCancelled, StateFailed:
	default:
		return false, fmt.Errorf("cleaning finalize: %q is not a final state", state)
	}
	res, err := db.Exec(`UPDATE cleaning_missions SET state = $2, finished_at = $3
		WHERE id = $1 AND state = $4`, id, state, at, StateDispatched)
	if err != nil {
		return false, fmt.Errorf("cleaning finalize: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// View is what the cleaning guard needs to know about one robot from the
// record: when it was last sent (any state) and whether it is opted out.
type View struct {
	LastDispatched *time.Time
	OptedOut       bool
}

// Views reads every robot the record knows about in one query: the last
// dispatch of any state, and the opt-out. A robot in neither table is absent
// from the map, which is the zero View: never sent, not opted out.
func Views(db *sql.DB) (map[string]View, error) {
	rows, err := db.Query(`SELECT coalesce(m.vehicle_id, o.vehicle_id), m.last, o.vehicle_id IS NOT NULL
		FROM (SELECT vehicle_id, max(dispatched_at) AS last FROM cleaning_missions GROUP BY vehicle_id) m
		FULL OUTER JOIN cleaning_opt_outs o ON o.vehicle_id = m.vehicle_id`)
	if err != nil {
		return nil, fmt.Errorf("cleaning views: %w", err)
	}
	defer rows.Close()
	out := map[string]View{}
	for rows.Next() {
		var (
			vid string
			v   View
		)
		if err := rows.Scan(&vid, &v.LastDispatched, &v.OptedOut); err != nil {
			return nil, err
		}
		out[vid] = v
	}
	return out, rows.Err()
}

// SetOptOut records (out) or clears (!out) a robot's opt-out. changed is false
// when the robot was already in that state.
func SetOptOut(db *sql.DB, vehicleID string, out bool) (bool, error) {
	var (
		res sql.Result
		err error
	)
	if out {
		res, err = db.Exec(`INSERT INTO cleaning_opt_outs (vehicle_id) VALUES ($1) ON CONFLICT DO NOTHING`, vehicleID)
	} else {
		res, err = db.Exec(`DELETE FROM cleaning_opt_outs WHERE vehicle_id = $1`, vehicleID)
	}
	if err != nil {
		return false, fmt.Errorf("cleaning opt-out: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ListMissions returns the missions dispatched at or after since, newest
// first, at most limit.
func ListMissions(db *sql.DB, since time.Time, limit int) ([]Mission, error) {
	rows, err := db.Query(`SELECT `+missionCols+` FROM cleaning_missions
		WHERE dispatched_at >= $1 ORDER BY dispatched_at DESC LIMIT $2`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("cleaning list missions: %w", err)
	}
	return scanMissions(rows)
}

// PruneMissions deletes missions older than Retention, but only when the
// oldest row is a day past it.
//
// THE ONCE-A-DAY CONDITION IS THE TABLE ITSELF. The loop that calls this runs
// every minute and keeps no memory, and Core restarts far too often for a
// 24-hour ticker ever to fire (R3-9). After a prune the oldest row is younger
// than Retention, so the condition cannot hold again for at least a day: the
// delete runs at most daily on any restart pattern, and every other minute the
// statement is one index probe for min(dispatched_at).
func PruneMissions(db *sql.DB, now time.Time) (int64, error) {
	cutoff := now.Add(-Retention)
	res, err := db.Exec(`DELETE FROM cleaning_missions
		WHERE dispatched_at < $1
		  AND (SELECT min(dispatched_at) FROM cleaning_missions) < $2`,
		cutoff, cutoff.Add(-24*time.Hour))
	if err != nil {
		return 0, fmt.Errorf("cleaning prune: %w", err)
	}
	return res.RowsAffected()
}
