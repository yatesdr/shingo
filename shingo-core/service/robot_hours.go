package service

import (
	"fmt"
	"sort"
	"time"

	"shingocore/store/robothours"
)

// The Robots page's Fleet tab, history half: per robot, today and the days
// before it, summed from the hourly record (robot_hours, robot_alarm_onsets).
//
// The day is the PLANT's, computed here at read time in the zone the handler
// passes (Q-004); the record itself is UTC hours. Nothing about laser frames is
// in here on purpose: robot_lidar_hours is read per robot against itself, and a
// fleet table would put single frames side by side across robots, which is the
// comparison the record calls invalid (DATA-laser-frame-gauge.md §3).

// Alarm codes the Fleet tab counts onsets of. The set is the lidar
// contamination sweep's (FINDINGS-lidar-contamination-sweep-2026-10-06.md):
// 54901 is the recognition camera failing to find its target (the film gauge),
// 52101 is a laser frame with every point invalid, and 50401 is the fatal
// "battery too low, robot will shut down", the sweep's battery-fatal row.
const (
	AlarmFilmRecognition = 54901
	AlarmLaserInvalid    = 52101
	AlarmBatteryFatal    = 50401
)

// RobotHoursFigures is one robot's totals over a span of plant days. Pointer
// fields are nil when no reading in the span carried one.
type RobotHoursFigures struct {
	Hours         int      `json:"hours"`
	ConnectedMin  int64    `json:"connected_min"`
	ChargingMin   int64    `json:"charging_min"`
	BlockedMin    int64    `json:"blocked_min"`
	Km            float64  `json:"km"`
	ChargeStarts  int64    `json:"charge_starts"`
	BatteryMinPct *float64 `json:"battery_min_pct"`
	Lifts         *int     `json:"lifts"`
	Film          int      `json:"onsets_54901"`
	LaserInvalid  int      `json:"onsets_52101"`
	BatteryFatal  int      `json:"onsets_battery_fatal"`
}

// RobotHoursRow is one robot's today and prior days. Today or Prior is nil when
// the record has no hour for that robot in the span: no data, which the page
// shows as a dash, never as zero.
type RobotHoursRow struct {
	VehicleID string             `json:"vehicle_id"`
	Today     *RobotHoursFigures `json:"today"`
	Prior     *RobotHoursFigures `json:"prior"`
}

// RobotHoursView is the Fleet tab's history payload.
type RobotHoursView struct {
	Zone      string          `json:"zone"`
	Today     string          `json:"today"`      // YYYY-MM-DD, plant-local
	PriorFrom string          `json:"prior_from"` // first of the prior days
	PriorDays int             `json:"prior_days"`
	Robots    []RobotHoursRow `json:"robots"`
}

// RobotHoursAt reads today and the priorDays plant-local days before it, in
// loc, as of now.
func (s *NodeService) RobotHoursAt(loc *time.Location, now time.Time, priorDays int) (RobotHoursView, error) {
	if loc == nil {
		loc = time.UTC
	}
	if priorDays < 1 || priorDays > 31 {
		return RobotHoursView{}, fmt.Errorf("robot hours: prior days must be 1 to 31, got %d", priorDays)
	}
	local := now.In(loc)
	today := local.Format("2006-01-02")
	from := time.Date(local.Year(), local.Month(), local.Day()-priorDays, 0, 0, 0, 0, loc).Format("2006-01-02")
	rows, err := s.db.RobotDayRows(loc, from, today)
	if err != nil {
		return RobotHoursView{}, err
	}
	return RobotHoursView{
		Zone: loc.String(), Today: today, PriorFrom: from, PriorDays: priorDays,
		Robots: robotHoursRows(rows, today),
	}, nil
}

// robotHoursRows folds plant-day rows into one row per robot: the day equal to
// today, and the sum of the others.
func robotHoursRows(rows []robothours.DayRow, today string) []RobotHoursRow {
	by := map[string]*RobotHoursRow{}
	for _, d := range rows {
		r := by[d.VehicleID]
		if r == nil {
			r = &RobotHoursRow{VehicleID: d.VehicleID}
			by[d.VehicleID] = r
		}
		target := &r.Prior
		if d.Day == today {
			target = &r.Today
		}
		if *target == nil {
			*target = &RobotHoursFigures{}
		}
		addDay(*target, d)
	}
	out := make([]RobotHoursRow, 0, len(by))
	for _, r := range by {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VehicleID < out[j].VehicleID })
	return out
}

// addDay adds one plant day to a span's figures. Seconds become whole minutes
// and metres kilometres only here, at the edge; the record keeps seconds and
// metres.
func addDay(f *RobotHoursFigures, d robothours.DayRow) {
	f.Hours += d.Hours
	f.ConnectedMin += d.ConnectedS / 60
	f.ChargingMin += d.ChargingS / 60
	f.BlockedMin += d.BlockedS / 60
	f.Km += d.Metres / 1000
	f.ChargeStarts += d.ChargeStarts
	if d.BatteryMin != nil && (f.BatteryMinPct == nil || *d.BatteryMin < *f.BatteryMinPct) {
		v := *d.BatteryMin
		f.BatteryMinPct = &v
	}
	if d.Lifts != nil {
		n := *d.Lifts
		if f.Lifts != nil {
			n += *f.Lifts
		}
		f.Lifts = &n
	}
	f.Film += d.Onsets[AlarmFilmRecognition]
	f.LaserInvalid += d.Onsets[AlarmLaserInvalid]
	f.BatteryFatal += d.Onsets[AlarmBatteryFatal]
}
