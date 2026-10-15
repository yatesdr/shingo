package service

import (
	"fmt"
	"sort"
	"time"

	"shingocore/store/cleaning"
)

// The Robots page's Cleaning tab and the robot pop-up's opt-out, read side.
//
// ASSEMBLED HERE, NOT IN THE HANDLER, for the localization board's reason: www
// may not import a store sub-package, so the types below are this package's
// and the handler never names store/cleaning. Whether a robot can be cleaned
// right now is NOT here: that answer reads the fleet and the dispatch seam, so
// it is engine.CleanNowRefusal, which the handler asks per robot with the same
// words the Clean verb refuses with.

// CleaningSetupView is the plant's one cleaning mission as saved. Points is
// empty when none is set up, which is the loop switched off.
type CleaningSetupView struct {
	Saved      bool       `json:"saved"`
	Points     []string   `json:"points"`
	Frequency  string     `json:"frequency"` // "4h" | "8h" | "12h" | "24h"; "" before any save
	MinBattery int        `json:"min_battery"`
	UpdatedAt  *time.Time `json:"updated_at"`
}

// CleaningMissionView is one cleaning order sent to the fleet.
type CleaningMissionView struct {
	VehicleID     string     `json:"vehicle_id"`
	VendorOrderID string     `json:"vendor_order_id"`
	State         string     `json:"state"`
	Reason        string     `json:"reason"`
	DispatchedAt  time.Time  `json:"dispatched_at"`
	FinishedAt    *time.Time `json:"finished_at"`
}

// CleaningRobotView is one robot's line on the Cleaning tab.
type CleaningRobotView struct {
	VehicleID      string     `json:"vehicle_id"`
	LastCleaned    *time.Time `json:"last_cleaned"` // last mission that FINISHED
	LastDispatched *time.Time `json:"last_dispatched"`
	LastReason     string     `json:"last_reason"`
	MissionsToday  int        `json:"missions_today"` // dispatched since plant-local midnight
	OptedOut       bool       `json:"opted_out"`
}

// CleaningRecord is the Cleaning tab's payload from the database: the setup,
// a line per robot the record knows, and the mission history since Since.
type CleaningRecord struct {
	Setup    CleaningSetupView     `json:"setup"`
	Robots   []CleaningRobotView   `json:"robots"`
	Missions []CleaningMissionView `json:"missions"`
	Since    time.Time             `json:"since"`
	// AnyOpen is whether a mission is still with the fleet. An open one is
	// minutes old (the loop fails it at 30), so it is always in Missions.
	AnyOpen bool `json:"any_open"`
}

// cleaningHistoryDays is how far back the tab lists missions: the trial's
// window, inside the record's 90-day retention.
const cleaningHistoryDays = 60

// cleaningHistoryLimit caps the list. At the loop's ceiling (4 h minimum
// frequency, around the clock) a fleet of 12 sends at most 72 a day, about 500
// a week; the cap keeps a mistake from rendering thousands.
const cleaningHistoryLimit = 500

// CleaningRecordAt reads the Cleaning tab's record as of now, counting
// "today" from midnight in loc (the plant's day). Three queries: the setup
// row, the per-robot view, and the history.
func (s *NodeService) CleaningRecordAt(loc *time.Location, now time.Time) (CleaningRecord, error) {
	if loc == nil {
		loc = time.UTC
	}
	var out CleaningRecord
	setup, ok, err := s.db.CleaningSetup()
	if err != nil {
		return out, err
	}
	out.Setup = cleaningSetupView(setup, ok)

	views, err := s.db.CleaningViews()
	if err != nil {
		return out, err
	}
	local := now.In(loc)
	out.Since = time.Date(local.Year(), local.Month(), local.Day()-cleaningHistoryDays, 0, 0, 0, 0, loc)
	missions, err := s.db.CleaningMissions(out.Since, cleaningHistoryLimit)
	if err != nil {
		return out, err
	}
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	out.Missions, out.Robots = cleaningRobots(views, missions, midnight)
	for _, m := range missions {
		if m.State == cleaning.StateDispatched {
			out.AnyOpen = true
			break
		}
	}
	return out, nil
}

// CleaningSetupNow is the saved setup alone, for a caller that needs no
// history (the robot pop-up's refusal). One query.
func (s *NodeService) CleaningSetupNow() (CleaningSetupView, error) {
	setup, ok, err := s.db.CleaningSetup()
	if err != nil {
		return CleaningSetupView{}, err
	}
	return cleaningSetupView(setup, ok), nil
}

// SetCleaningOptOut records (out) or clears a robot's opt-out and appends an
// audit row naming the robot, when it changed. It is a one-row write with no
// fleet call, which is why it is a service verb and not an engine one.
func (s *NodeService) SetCleaningOptOut(vehicleID string, out bool, actor string) error {
	if vehicleID == "" {
		return fmt.Errorf("vehicle_id is required")
	}
	changed, err := s.db.SetCleaningOptOut(vehicleID, out)
	if err != nil || !changed {
		return err
	}
	action, was, now := "opt_out", vehicleID+" cleaned", vehicleID+" never cleaned"
	if !out {
		action, was, now = "opt_in", now, was
	}
	// Entity id 0 is the fire-alarm shape for a string-keyed entity; the robot
	// is in the text.
	return s.db.AppendAudit("cleaning", 0, action, was, now, actor)
}

func cleaningSetupView(setup cleaning.Setup, ok bool) CleaningSetupView {
	v := CleaningSetupView{Saved: ok, Points: []string{}}
	if !ok {
		return v
	}
	v.Points = setup.Points
	v.Frequency = fmt.Sprintf("%dh", int(setup.Frequency/time.Hour))
	v.MinBattery = setup.MinBattery
	at := setup.UpdatedAt
	v.UpdatedAt = &at
	return v
}

// cleaningRobots builds the history list and one line per robot the record
// knows (a mission or an opt-out). missions arrive newest first.
func cleaningRobots(views map[string]cleaning.View, missions []cleaning.Mission, midnight time.Time) ([]CleaningMissionView, []CleaningRobotView) {
	hist := make([]CleaningMissionView, 0, len(missions))
	lines := map[string]*CleaningRobotView{}
	line := func(vid string) *CleaningRobotView {
		l := lines[vid]
		if l == nil {
			l = &CleaningRobotView{VehicleID: vid}
			lines[vid] = l
		}
		return l
	}
	for vid, v := range views {
		l := line(vid)
		l.OptedOut = v.OptedOut
		l.LastDispatched = v.LastDispatched
	}
	for _, m := range missions {
		hist = append(hist, CleaningMissionView{
			VehicleID: m.VehicleID, VendorOrderID: m.VendorOrderID, State: m.State, Reason: m.Reason,
			DispatchedAt: m.DispatchedAt, FinishedAt: m.FinishedAt,
		})
		l := line(m.VehicleID)
		if l.LastReason == "" {
			l.LastReason = m.Reason // newest first: the first seen is the last sent
		}
		if !m.DispatchedAt.Before(midnight) {
			l.MissionsToday++
		}
		if m.State == cleaning.StateFinished && m.FinishedAt != nil && (l.LastCleaned == nil || m.FinishedAt.After(*l.LastCleaned)) {
			at := *m.FinishedAt
			l.LastCleaned = &at
		}
	}
	robots := make([]CleaningRobotView, 0, len(lines))
	for _, l := range lines {
		robots = append(robots, *l)
	}
	sort.Slice(robots, func(i, j int) bool { return robots[i].VehicleID < robots[j].VehicleID })
	return hist, robots
}
