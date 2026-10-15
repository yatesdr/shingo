// Package robothours stores the robot poll as totals per robot per UTC hour
// (migration v158): robot_hours, robot_alarm_onsets, and robot_lidar_hours.
//
// THE GRAIN IS THE UTC HOUR AND THE DAY IS COMPUTED AT READ TIME. A plant-local
// day is not a fixed set of UTC hours (2026-11-01 is 25 hours long at a Central
// plant), so the writer never decides which day an hour belongs to; DayRows
// buckets in the zone it is handed, the footprint precedent.
//
// Pure SQL on *sql.DB. The engine's tap (engine/robot_hours.go) and lidar loop
// call it directly; the read side reaches it through store/robothours.go.
package robothours

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// LidarRetention is how long a robot_lidar_hours row is kept. robot_hours and
// robot_alarm_onsets are permanent.
const LidarRetention = 90 * 24 * time.Hour

// Hour is one robot's totals for one UTC hour.
//
// BatteryMin is nil until the robot gave a connected, non-zero reading in the
// hour. LiftCount is the jack's lifetime counter as last seen, not a count of
// lifts in the hour: lifts per day is a difference between days.
type Hour struct {
	Hour         time.Time
	VehicleID    string
	ConnectedS   int
	ChargingS    int
	BlockedS     int
	Metres       float64
	ChargeStarts int
	BatteryMin   *float64
	LiftCount    *int
}

// Onset is one alarm code's onsets for one robot in one UTC hour.
type Onset struct {
	Hour      time.Time
	VehicleID string
	Code      int
	Onsets    int
	FirstAt   time.Time
}

// Flush writes running totals for the hours and onsets given, in one
// transaction: one multi-row statement for each table.
//
// REPLACE, NOT ADD. Each row carries the bucket's total so far, and the
// conflict sets every column to the new value. A flush that fails and is
// retried, or that runs twice, writes the same totals again and cannot double
// count. The caller seeds its totals from LoadHour on first touch of an hour,
// so a restart mid-hour carries the stored totals forward rather than
// replacing them with a fresh count.
//
// Keys must be unique within one call: Postgres refuses a multi-row upsert
// that touches the same row twice. ctx bounds the whole transaction: the
// caller is the 2-second robot poll, which a slow database must not stall.
func Flush(ctx context.Context, db *sql.DB, hours []Hour, onsets []Onset) error {
	if len(hours) == 0 && len(onsets) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("robot hours: begin: %w", err)
	}
	defer tx.Rollback() // no-op after Commit

	if len(hours) > 0 {
		var (
			vals []string
			args []any
		)
		for _, h := range hours {
			n := len(args)
			vals = append(vals, fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9))
			args = append(args, h.Hour.UTC(), h.VehicleID, h.ConnectedS, h.ChargingS, h.BlockedS,
				h.Metres, h.ChargeStarts, h.BatteryMin, h.LiftCount)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO robot_hours
				(hour, vehicle_id, connected_s, charging_s, blocked_s, metres, charge_starts, battery_min, lift_count)
			VALUES `+strings.Join(vals, ",")+`
			ON CONFLICT (hour, vehicle_id) DO UPDATE SET
				connected_s = EXCLUDED.connected_s, charging_s = EXCLUDED.charging_s,
				blocked_s = EXCLUDED.blocked_s, metres = EXCLUDED.metres,
				charge_starts = EXCLUDED.charge_starts, battery_min = EXCLUDED.battery_min,
				lift_count = EXCLUDED.lift_count`, args...); err != nil {
			return fmt.Errorf("robot hours: upsert hours: %w", err)
		}
	}
	if len(onsets) > 0 {
		var (
			vals []string
			args []any
		)
		for _, o := range onsets {
			n := len(args)
			vals = append(vals, fmt.Sprintf("($%d,$%d,$%d,$%d,$%d)", n+1, n+2, n+3, n+4, n+5))
			args = append(args, o.Hour.UTC(), o.VehicleID, o.Code, o.Onsets, o.FirstAt.UTC())
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO robot_alarm_onsets (hour, vehicle_id, code, onsets, first_at)
			VALUES `+strings.Join(vals, ",")+`
			ON CONFLICT (hour, vehicle_id, code) DO UPDATE SET
				onsets = EXCLUDED.onsets, first_at = EXCLUDED.first_at`, args...); err != nil {
			return fmt.Errorf("robot hours: upsert onsets: %w", err)
		}
	}
	return tx.Commit()
}

// LoadHour reads every stored row for one UTC hour: what a writer seeds its
// totals from the first time it touches that hour, so a restart mid-hour adds
// to the record instead of overwriting it. Two queries, once per hour per
// process. ctx bounds both, for the same reason as Flush.
func LoadHour(ctx context.Context, db *sql.DB, hour time.Time) (map[string]Hour, map[string][]Onset, error) {
	hour = hour.UTC()
	rows, err := db.QueryContext(ctx, `SELECT vehicle_id, connected_s, charging_s, blocked_s, metres,
			charge_starts, battery_min, lift_count
		FROM robot_hours WHERE hour = $1`, hour)
	if err != nil {
		return nil, nil, fmt.Errorf("robot hours: load hour: %w", err)
	}
	hours := map[string]Hour{}
	for rows.Next() {
		h := Hour{Hour: hour}
		if err := rows.Scan(&h.VehicleID, &h.ConnectedS, &h.ChargingS, &h.BlockedS, &h.Metres,
			&h.ChargeStarts, &h.BatteryMin, &h.LiftCount); err != nil {
			rows.Close()
			return nil, nil, err
		}
		hours[h.VehicleID] = h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	rows, err = db.QueryContext(ctx, `SELECT vehicle_id, code, onsets, first_at
		FROM robot_alarm_onsets WHERE hour = $1`, hour)
	if err != nil {
		return nil, nil, fmt.Errorf("robot hours: load onsets: %w", err)
	}
	defer rows.Close()
	onsets := map[string][]Onset{}
	for rows.Next() {
		o := Onset{Hour: hour}
		if err := rows.Scan(&o.VehicleID, &o.Code, &o.Onsets, &o.FirstAt); err != nil {
			return nil, nil, err
		}
		onsets[o.VehicleID] = append(onsets[o.VehicleID], o)
	}
	return hours, onsets, rows.Err()
}

// DayRow is one robot's plant-local day, summed from its hours.
//
// Hours is how many hour rows the day has; zero never appears (a day with no
// row is absent), so a reader renders a missing day as no data rather than 0.
// LiftCountMax is the last lifetime lift count seen that day; lifts in the day
// are its difference from the previous day's (DayRows fills Lifts).
type DayRow struct {
	Day          string // YYYY-MM-DD, plant-local
	VehicleID    string
	Hours        int
	ConnectedS   int64
	ChargingS    int64
	BlockedS     int64
	Metres       float64
	ChargeStarts int64
	BatteryMin   *float64
	LiftCountMax *int
	Lifts        *int
	Onsets       map[int]int // code -> onsets that day
}

// DayRows sums robot_hours and robot_alarm_onsets into plant-local days in
// loc, for days from fromDay to toDay inclusive (both YYYY-MM-DD in loc).
//
// Lifts for a day are its last lift count minus the previous day's; the query
// reads one extra day before fromDay so the first day has a previous one. A
// day whose robot has no previous day in that span falls back to the spread
// within the day, which undercounts only the lifts between midnight and the
// first poll.
func DayRows(db *sql.DB, loc *time.Location, fromDay, toDay string) ([]DayRow, error) {
	if loc == nil {
		loc = time.UTC
	}
	from, err := time.ParseInLocation("2006-01-02", fromDay, loc)
	if err != nil {
		return nil, fmt.Errorf("robot hours: from day: %w", err)
	}
	to, err := time.ParseInLocation("2006-01-02", toDay, loc)
	if err != nil {
		return nil, fmt.Errorf("robot hours: to day: %w", err)
	}
	lo := from.AddDate(0, 0, -1) // one day back, for the first day's lifts
	hi := to.AddDate(0, 0, 1)
	tz := loc.String()

	rows, err := db.Query(`SELECT to_char((hour AT TIME ZONE $1)::date, 'YYYY-MM-DD') AS day, vehicle_id,
			count(*), sum(connected_s), sum(charging_s), sum(blocked_s), sum(metres),
			sum(charge_starts), min(battery_min), max(lift_count), min(lift_count)
		FROM robot_hours WHERE hour >= $2 AND hour < $3
		GROUP BY 1, 2 ORDER BY 2, 1`, tz, lo.UTC(), hi.UTC())
	if err != nil {
		return nil, fmt.Errorf("robot hours: day rows: %w", err)
	}
	type key struct{ day, vid string }
	var (
		all   []DayRow
		index = map[key]int{}
		mins  []*int
	)
	for rows.Next() {
		var (
			r       DayRow
			liftMin *int
		)
		if err := rows.Scan(&r.Day, &r.VehicleID, &r.Hours, &r.ConnectedS, &r.ChargingS, &r.BlockedS,
			&r.Metres, &r.ChargeStarts, &r.BatteryMin, &r.LiftCountMax, &liftMin); err != nil {
			rows.Close()
			return nil, err
		}
		r.Onsets = map[int]int{}
		index[key{r.Day, r.VehicleID}] = len(all)
		all = append(all, r)
		mins = append(mins, liftMin)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Lifts: rows are ordered by vehicle then day, so the previous row of the
	// same vehicle is its previous day with data.
	for i := range all {
		cur := all[i].LiftCountMax
		if cur == nil {
			continue
		}
		var base *int
		if i > 0 && all[i-1].VehicleID == all[i].VehicleID && all[i-1].LiftCountMax != nil {
			base = all[i-1].LiftCountMax
		} else {
			base = mins[i]
		}
		if base != nil && *cur >= *base {
			d := *cur - *base
			all[i].Lifts = &d
		}
	}

	orows, err := db.Query(`SELECT to_char((hour AT TIME ZONE $1)::date, 'YYYY-MM-DD'), vehicle_id, code, sum(onsets)
		FROM robot_alarm_onsets WHERE hour >= $2 AND hour < $3
		GROUP BY 1, 2, 3`, tz, from.UTC(), hi.UTC())
	if err != nil {
		return nil, fmt.Errorf("robot hours: day onsets: %w", err)
	}
	defer orows.Close()
	for orows.Next() {
		var (
			day, vid string
			code, n  int
		)
		if err := orows.Scan(&day, &vid, &code, &n); err != nil {
			return nil, err
		}
		i, ok := index[key{day, vid}]
		if !ok {
			// Onsets in a day with no hour row cannot happen from the one
			// writer (both are flushed together); a hand-written row would
			// land here and is dropped rather than inventing a day.
			continue
		}
		all[i].Onsets[code] += n
	}
	if err := orows.Err(); err != nil {
		return nil, err
	}

	out := all[:0]
	for _, r := range all {
		if r.Day >= fromDay && r.Day <= toDay {
			out = append(out, r)
		}
	}
	return out, nil
}

// LidarHour is one lidar's reduced frame for one parked robot in one UTC hour.
// The rssi quartiles are over VALID beams only and are nil when no beam was
// valid; NearValid counts valid beams closer than 0.3 m.
type LidarHour struct {
	Hour         time.Time
	VehicleID    string
	Lidar        string
	Station      string
	UseForLoc    bool
	Beams        int
	Valid        int
	RSSIMedValid *float64
	RSSIP25Valid *float64
	RSSIP75Valid *float64
	NearValid    int
}

// InsertLidarHours writes one round's rows in one statement. A second probe in
// the same hour (a restart mid-hour) replaces the first.
func InsertLidarHours(db *sql.DB, rows []LidarHour) error {
	if len(rows) == 0 {
		return nil
	}
	var (
		vals []string
		args []any
	)
	for _, r := range rows {
		n := len(args)
		vals = append(vals, fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9, n+10, n+11))
		args = append(args, r.Hour.UTC(), r.VehicleID, r.Lidar, r.Station, r.UseForLoc, r.Beams, r.Valid,
			r.RSSIMedValid, r.RSSIP25Valid, r.RSSIP75Valid, r.NearValid)
	}
	_, err := db.Exec(`INSERT INTO robot_lidar_hours
			(hour, vehicle_id, lidar, station, use_for_loc, beams, valid,
			 rssi_med_valid, rssi_p25_valid, rssi_p75_valid, near_valid)
		VALUES `+strings.Join(vals, ",")+`
		ON CONFLICT (hour, vehicle_id, lidar) DO UPDATE SET
			station = EXCLUDED.station, use_for_loc = EXCLUDED.use_for_loc,
			beams = EXCLUDED.beams, valid = EXCLUDED.valid,
			rssi_med_valid = EXCLUDED.rssi_med_valid, rssi_p25_valid = EXCLUDED.rssi_p25_valid,
			rssi_p75_valid = EXCLUDED.rssi_p75_valid, near_valid = EXCLUDED.near_valid`, args...)
	if err != nil {
		return fmt.Errorf("robot lidar hours: insert: %w", err)
	}
	return nil
}

// PruneLidarHours deletes lidar rows older than LidarRetention. The lidar
// loop calls it once per hourly round, so it needs no condition of its own.
func PruneLidarHours(db *sql.DB, now time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM robot_lidar_hours WHERE hour < $1`, now.Add(-LidarRetention).UTC())
	if err != nil {
		return 0, fmt.Errorf("robot lidar hours: prune: %w", err)
	}
	return res.RowsAffected()
}
