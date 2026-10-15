// cleaning.go — the air-knife cleaning loop.
//
// A plant with an air knife sets up one mission on the Robots page (the
// ordered scene points a robot drives through the knife, how often each robot
// goes, the plant-local hours it may run, and a battery floor). Every minute
// this loop finalizes the mission in flight, if any, and when none is in
// flight sends the robot that has gone longest without one, if it is idle and
// due. The knife is preventive and runs on a clock: no signal selects a robot.
// A person can send one robot now (CleanNow) and opt a robot out.
//
// ZERO IN-MEMORY DECISION STATE (the keeper's lesson, maintainer.go): every
// tick re-reads the setup, the open missions and each robot's last dispatch
// from the database, so a restart is an ordinary tick. What the runner keeps
// is a memo of the points check, keyed by the setup's updated_at, and log-once
// flags; neither decides anything a restart would decide differently.
//
// A CLEANING ORDER IS NOT A CORE ORDER. It is a pinned vendor order with no
// bin task, and the only record of it is its cleaning_missions row. No orders
// row is written and the tracker never sees it, which is what keeps it out of
// the resolver, the boot census and every status path that reads the orders
// table (R2-5). The sg-clean- prefix is a label for people reading RDS
// history; nothing matches on it.
package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"shingo/protocol/clock"
	"shingo/shared/planttime"
	"shingocore/dispatch"
	"shingocore/domain"
	"shingocore/fleet"
	"shingocore/service"
	"shingocore/store/cleaning"
)

const (
	// cleaningTickInterval is the loop's cadence: the keeper's and the lane
	// floor's minute, which is far finer than a frequency measured in hours.
	cleaningTickInterval = 60 * time.Second

	// cleaningMaxAge is how long a mission may stay open before the loop
	// cancels it at the vendor and records it failed. A pass through the knife
	// takes a few minutes; thirty is ten times that. A pinned order whose robot
	// the fleet will not dispatch sits in RDS's queue forever rather than going
	// to another robot (fleet.CreateOrderRequest.Vehicle), and an open row
	// stops all cleaning, so something has to end it.
	cleaningMaxAge = 30 * time.Minute
)

// cleaningFrequencies are the only intervals the setup accepts: the Cleaning
// tab offers them as a segment, and the server refuses anything else.
var cleaningFrequencies = []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour}

// vendorOrderReader is the one fleet call the loop needs to finalize a
// mission. seerrds implements it; the simulator does not, and a fleet that
// cannot report a mission's state must never be sent one, because the row
// would stay open and stop all cleaning for good.
type vendorOrderReader interface {
	GetVendorOrderDetail(vendorOrderID string) (*fleet.VendorOrderDetail, error)
}

// cleaningSeam is the fleet's vendorOrderReader, or nil.
func (e *Engine) cleaningSeam() vendorOrderReader {
	if r, ok := e.fleet.(vendorOrderReader); ok {
		return r
	}
	return nil
}

// cleaningLoop runs the cleaning tick every minute until the engine stops.
func (e *Engine) cleaningLoop() {
	c := &cleaningRunner{e: e, seam: e.cleaningSeam()}
	ticker := clock.Default().NewTicker(cleaningTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C():
			robots, _ := e.latestPolledRobots()
			c.tick(time.Now(), robots)
		}
	}
}

// cleaningRunner is the loop's memory between ticks, and none of it decides
// anything: the seam is fixed for the process, the memo is a cache of a check
// keyed by the setup it checked, and the flags only stop a log line repeating.
type cleaningRunner struct {
	e    *Engine
	seam vendorOrderReader

	memoAt   time.Time
	memo     map[string]string
	saidSeam bool
}

// pointProblems is CleaningPointProblems for s, re-run only when the setup's
// updated_at moves.
func (c *cleaningRunner) pointProblems(s cleaning.Setup) map[string]string {
	if c.memo == nil || !c.memoAt.Equal(s.UpdatedAt) {
		c.memo = CleaningPointProblems(s.Points, c.e.nodeService, c.e.dispatcher)
		c.memoAt = s.UpdatedAt
	}
	return c.memo
}

// tick is one pass: the brief's seven steps in order.
func (c *cleaningRunner) tick(now time.Time, robots []fleet.RobotStatus) {
	e := c.e
	// 1. No mission set up, or one with no points: nothing at all runs. At a
	// plant that never set one up (Springfield) this lookup is the whole cost.
	setup, ok, err := cleaning.GetSetup(e.db.DB)
	if err != nil {
		e.dbg("cleaning: setup: %v", err)
		return
	}
	if !ok || len(setup.Points) == 0 {
		return
	}
	if c.seam == nil {
		if !c.saidSeam {
			e.logFn("cleaning: a mission is set up, but the fleet (%s) cannot report a mission's state; nothing will be sent", e.fleet.Name())
			c.saidSeam = true
		}
		return
	}

	// 2. Finalize what is open.
	open, err := cleaning.OpenMissions(e.db.DB)
	if err != nil {
		e.dbg("cleaning: open missions: %v", err)
		return
	}
	stillOpen := false
	for _, m := range open {
		if !e.finalizeCleaningMission(c.seam, m, now) {
			stillOpen = true
		}
	}

	// 3. Retention. The once-a-day condition is inside the statement.
	if _, err := cleaning.PruneMissions(e.db.DB, now); err != nil {
		e.dbg("cleaning: prune: %v", err)
	}

	// 4. Stop conditions. One robot at the knife at a time: an open row is
	// that fact, so nothing is sent while one is.
	if stillOpen || !e.fleetConnected.Load() || len(c.pointProblems(setup)) > 0 ||
		!inCleaningWindow(now, setup.ActiveFrom, setup.ActiveTo, e.plantZone()) {
		return
	}

	// 5–6. Who is due.
	views, err := cleaning.Views(e.db.DB)
	if err != nil {
		e.dbg("cleaning: views: %v", err)
		return
	}
	vehicle := pickCleaningRobot(now, setup, robots, views)
	if vehicle == "" {
		return
	}

	// 7. Send it.
	if err := e.dispatchCleaning(vehicle, setup.Points, cleaning.ReasonInterval, now); err != nil {
		e.logFn("cleaning: %s not sent: %v", vehicle, err)
	}
}

// plantZone is the plant's zone by the one resolver every screen uses.
// Timezone applies after a restart, but it is read under the config lock
// because the config page writes the struct.
func (e *Engine) plantZone() planttime.Zone {
	e.cfg.Lock()
	tz := e.cfg.Timezone
	e.cfg.Unlock()
	return planttime.ResolveZone(tz)
}

// finalizeCleaningMission reads one open mission at the vendor and closes it
// when the vendor is done with it, or when it is past cleaningMaxAge. It
// reports whether the mission is closed.
func (e *Engine) finalizeCleaningMission(seam vendorOrderReader, m cleaning.Mission, now time.Time) bool {
	detail, readErr := seam.GetVendorOrderDetail(m.VendorOrderID)
	state, cancel := cleaningFinalState(m, detail, readErr, now)
	if state == "" {
		return false
	}
	if cancel {
		// Cancel FIRST, then record failed, and record failed even when the
		// cancel is refused. Left open, the row stops every robot's cleaning
		// for good; marked failed while RDS still holds the order, the worst
		// case is that the robot drives through the knife later, which is
		// the mission. The refusal goes in the log line.
		if err := e.fleet.CancelOrder(m.VendorOrderID); err != nil {
			e.logFn("cleaning: %s %s open past %s, cancel refused: %v", m.VehicleID, m.VendorOrderID, cleaningMaxAge, err)
		}
	}
	if _, err := cleaning.Finalize(e.db.DB, m.ID, state, now); err != nil {
		e.dbg("cleaning: finalize %s: %v", m.VendorOrderID, err)
		return false
	}
	e.logFn("cleaning: %s %s %s after %s", m.VehicleID, m.VendorOrderID, state, now.Sub(m.DispatchedAt).Round(time.Second))
	return true
}

// cleaningFinalState maps the vendor's answer for an open mission to its
// final state, "" while it is still running. cancel is true when the mission
// is closed for age, and the vendor must be told first.
//
// The vendor states are SEER's (rds.OrderState) and are matched as strings on
// purpose: rds.OrderState.IsTerminal is FINISHED or STOPPED only, because
// Core's dispatch treats FAILED as a grace state, and a cleaning mission has no
// grace — FAILED is final here. STOPPED means someone or something stopped it,
// which is not a finished pass through the knife.
//
// A read error leaves the mission open: one transient failure must not close
// a mission that is running, and the age rule ends one that never answers.
func cleaningFinalState(m cleaning.Mission, d *fleet.VendorOrderDetail, readErr error, now time.Time) (state string, cancel bool) {
	if readErr == nil && d != nil {
		switch d.State {
		case "FINISHED":
			return cleaning.StateFinished, false
		case "STOPPED":
			return cleaning.StateCancelled, false
		case "FAILED":
			return cleaning.StateFailed, false
		}
	}
	if now.Sub(m.DispatchedAt) > cleaningMaxAge {
		return cleaning.StateFailed, true
	}
	return "", false
}

// dispatchCleaning sends vehicle through points as one pinned vendor order and
// records the mission.
//
// THE INVARIANT (R2-5): this writes no orders row and never calls the tracker.
// Those two omissions, not the id prefix, are what keep a cleaning order out
// of the resolver and every Core order path. No bin task on any block: the
// robot drives the points and the fleet returns it to park on its own.
//
// If the fleet refuses the order nothing is recorded: there is no mission to
// finalize, and a row for it would read as a dispatch and push the robot's
// next turn back by a whole frequency.
func (e *Engine) dispatchCleaning(vehicle string, points []string, reason string, now time.Time) error {
	orderID := dispatch.VendorIDPrefix + "clean-" + uuid.New().String()
	blocks := make([]fleet.OrderBlock, 0, len(points))
	for i, p := range points {
		blocks = append(blocks, fleet.OrderBlock{BlockID: fmt.Sprintf("%s-b%d", orderID, i+1), Location: p})
	}
	if _, err := e.fleet.CreateOrder(fleet.CreateOrderRequest{
		OrderID:  orderID,
		Vehicle:  vehicle,
		Blocks:   blocks,
		Complete: true,
	}); err != nil {
		return fmt.Errorf("fleet did not accept the order: %w", err)
	}
	if _, err := cleaning.RecordDispatch(e.db.DB, vehicle, orderID, reason, now); err != nil {
		// The order is at the vendor with no row: the robot will drive the
		// knife, and the next tick may send another robot behind it.
		return fmt.Errorf("sent %s but could not record it: %w", orderID, err)
	}
	e.logFn("cleaning: %s sent through %s (%s, %s)", vehicle, strings.Join(points, " → "), reason, orderID)
	return nil
}

// pickCleaningRobot is the robot to send this tick, "" for none: of the robots
// in the latest poll that pass cleanRefusal and are due, the one longest since
// its last dispatch (never sent first, then vehicle id). ONE per tick.
//
// Due is measured from the last DISPATCH of any state, not the last finish: a
// robot whose missions fail must not be sent again every minute.
func pickCleaningRobot(now time.Time, s cleaning.Setup, robots []fleet.RobotStatus, views map[string]cleaning.View) string {
	type cand struct {
		vid  string
		last *time.Time
	}
	var cands []cand
	for _, r := range robots {
		if r.VehicleID == "" {
			continue
		}
		v := views[r.VehicleID]
		if CleanRefusal(r, v.OptedOut, s.MinBattery) != "" {
			continue
		}
		if v.LastDispatched != nil && now.Sub(*v.LastDispatched) < s.Frequency {
			continue
		}
		cands = append(cands, cand{r.VehicleID, v.LastDispatched})
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		switch {
		case a.last == nil && b.last != nil:
			return true
		case a.last != nil && b.last == nil:
			return false
		case a.last != nil && !a.last.Equal(*b.last):
			return a.last.Before(*b.last)
		}
		return a.vid < b.vid
	})
	return cands[0].vid
}

// CleanRefusal is why robot r may not be sent through the knife now, in words
// for the robot pop-up, "" when it may. The loop, CleanNow and the pop-up's
// disabled-with-reason Clean button all read this one function. Plain values,
// not the store's view, because www may not import the store.
//
// It sits here rather than beside fleet.RobotStatus.NotDispatchableWhy because
// the deck check is service.RobotCarryingBin, and fleet cannot import service.
//
// A deck whose state is uncertain (moving, mid-lift, failed, or no jack report)
// counts as carrying: a knife pass with a bin on the deck is the one outcome
// this guard exists to prevent, and an idle robot reports a definite state.
func CleanRefusal(r fleet.RobotStatus, optedOut bool, minBattery int) string {
	switch {
	case !r.Connected:
		return "not connected"
	case !r.Available:
		return "not dispatchable"
	case r.Emergency:
		return "emergency stop"
	case r.IsError:
		return "in error"
	case r.Blocked:
		return "blocked"
	case r.Charging:
		return "charging"
	case r.Busy:
		return "on a task"
	}
	if carrying, sure := service.RobotCarryingBin(r); carrying || !sure {
		return "may be carrying a bin"
	}
	switch {
	case r.RelocStatus == 0 || r.RelocStatus == 2:
		return "not localized"
	case r.BatteryLevel < float64(minBattery):
		return fmt.Sprintf("battery %.0f%% is below %d%%", r.BatteryLevel, minBattery)
	case optedOut:
		return "opted out of cleaning"
	}
	return ""
}

// CleanNowFacts is everything CleanNowRefusal judges. The engine assembles it
// for CleanNow; the robot pop-up assembles the same facts from the same
// stores, so the button's disabled reason and the POST's refusal are one
// sentence.
type CleanNowFacts struct {
	SetUp         bool              // a setup row exists with at least one point
	PointProblems map[string]string // CleaningPointProblems
	CanReport     bool              // the fleet implements vendorOrderReader
	AnyOpen       bool              // a mission is open, fleet-wide
	Robot         *fleet.RobotStatus
	OptedOut      bool
	MinBattery    int
}

// CleanNowRefusal is why a person may not send this robot through the knife
// now, "" when they may. Manual cleaning skips the frequency and the active
// hours (a person decided) and nothing else.
func CleanNowRefusal(f CleanNowFacts) string {
	switch {
	case !f.SetUp:
		return "no clean mission is set up"
	case len(f.PointProblems) > 0:
		return firstPointProblem(f.PointProblems)
	case !f.CanReport:
		return "the fleet cannot report a mission's state"
	case f.AnyOpen:
		return "another robot is at the knife"
	case f.Robot == nil:
		return "not in the fleet's latest report"
	}
	return CleanRefusal(*f.Robot, f.OptedOut, f.MinBattery)
}

// firstPointProblem is the problem on the lowest-numbered point, so the words
// are the same on every read.
func firstPointProblem(p map[string]string) string {
	best, bestI := "", -1
	for k, msg := range p {
		var i int
		if _, err := fmt.Sscanf(k, "points.%d", &i); err != nil {
			i = 1 << 30
		}
		if bestI < 0 || i < bestI || (i == bestI && msg < best) {
			best, bestI = msg, i
		}
	}
	return best
}

// CleanNow sends vehicleID through the knife now, refused with the words
// CleanNowRefusal gives. Audited: it moves a robot.
func (e *Engine) CleanNow(vehicleID, actor string) error {
	setup, ok, err := cleaning.GetSetup(e.db.DB)
	if err != nil {
		return err
	}
	f := CleanNowFacts{
		SetUp:      ok && len(setup.Points) > 0,
		CanReport:  e.cleaningSeam() != nil,
		MinBattery: setup.MinBattery,
	}
	if f.SetUp {
		f.PointProblems = CleaningPointProblems(setup.Points, e.nodeService, e.dispatcher)
	}
	if f.AnyOpen, err = cleaning.AnyOpen(e.db.DB); err != nil {
		return err
	}
	robots, _ := e.latestPolledRobots()
	for i := range robots {
		if robots[i].VehicleID == vehicleID {
			f.Robot = &robots[i]
			break
		}
	}
	views, err := cleaning.Views(e.db.DB)
	if err != nil {
		return err
	}
	f.OptedOut = views[vehicleID].OptedOut
	if why := CleanNowRefusal(f); why != "" {
		return errors.New(why)
	}
	if err := e.dispatchCleaning(vehicleID, setup.Points, cleaning.ReasonManual, time.Now()); err != nil {
		return err
	}
	if err := e.auditService.Append("cleaning", 0, "clean_now", "", vehicleID, actor); err != nil {
		e.dbg("cleaning: audit clean_now %s: %v", vehicleID, err)
	}
	return nil
}

// CleaningPointLookup is what CleaningPointProblems reads of the scene and of
// Core's nodes; *service.NodeService satisfies it, so the engine and the
// Cleaning tab ask the same questions of the same doors.
type CleaningPointLookup interface {
	ClassOfPoint(instanceName string) (string, error)
	GetByName(name string) (*domain.Node, error)
}

// LaneLookup is the lane half; *dispatch.Dispatcher satisfies it.
type LaneLookup interface {
	LaneForNode(nodeID int64) (*domain.Node, error)
}

// CleaningPointProblems checks a mission's points, keyed "points.<i>" (0-based)
// so the setup block draws each under its own row. nil when every point is
// fine; empty points is not a problem (it is "off").
//
// A point must be in the scene: the fleet terminates an order whose block
// names a point it does not have. A point that is a Core node inside a lane is
// refused: Core's lane census counts reservations, and a robot driving a lane
// on a vendor-only order is invisible to it. No Core node of that name is
// fine and the usual case — the knife's points are LocationMarks, which Core
// never makes nodes of. A lookup that fails is a problem too: a check that
// cannot say "fine" must not say it.
func CleaningPointProblems(points []string, pts CleaningPointLookup, lanes LaneLookup) map[string]string {
	var out map[string]string
	bad := func(i int, msg string) {
		if out == nil {
			out = map[string]string{}
		}
		out[fmt.Sprintf("points.%d", i)] = msg
	}
	for i, p := range points {
		class, err := pts.ClassOfPoint(p)
		if err != nil {
			bad(i, fmt.Sprintf("%s could not be checked against the scene: %v", p, err))
			continue
		}
		if class == "" {
			bad(i, p+" is not in the scene")
			continue
		}
		n, err := pts.GetByName(p)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && n == nil) {
			continue // not a Core node: nothing of Core's to collide with
		}
		if err != nil {
			bad(i, fmt.Sprintf("%s could not be checked against Core's nodes: %v", p, err))
			continue
		}
		lane, err := lanes.LaneForNode(n.ID)
		if err != nil {
			bad(i, fmt.Sprintf("%s could not be checked for a lane: %v", p, err))
			continue
		}
		if lane != nil {
			bad(i, fmt.Sprintf("%s is in lane %s", p, lane.Name))
		}
	}
	return out
}

// CleaningSetupRefusal is a setup the server would not save, field by field,
// keyed the way the settings page draws errors ("frequency", "active_from",
// "active_to", "min_battery", "points.<i>").
type CleaningSetupRefusal struct {
	Fields map[string]string
}

func (r *CleaningSetupRefusal) Error() string {
	keys := make([]string, 0, len(r.Fields))
	for k := range r.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+r.Fields[k])
	}
	return "cleaning setup refused: " + strings.Join(parts, "; ")
}

// CleaningSetupInput is a setup as the Cleaning tab sends it. Engine-owned
// rather than the store's type because www may not import the store.
// Frequency is text as typed ("4h", "4 h", or a Go duration string); the
// active hours are plant-local "HH:MM".
type CleaningSetupInput struct {
	Points     []string
	Frequency  string
	ActiveFrom string
	ActiveTo   string
	MinBattery int
}

// SaveCleaningSetup validates and stores the plant's cleaning mission, and
// audits the change. Points are trimmed and blanks dropped before they are
// numbered for errors; an empty list is a valid save and turns the loop off.
// A refusal is a *CleaningSetupRefusal and nothing is saved.
func (e *Engine) SaveCleaningSetup(in CleaningSetupInput, actor string) error {
	s := cleaning.Setup{Points: []string{}, ActiveFrom: strings.TrimSpace(in.ActiveFrom),
		ActiveTo: strings.TrimSpace(in.ActiveTo), MinBattery: in.MinBattery}
	for _, p := range in.Points {
		if p = strings.TrimSpace(p); p != "" {
			s.Points = append(s.Points, p)
		}
	}
	freq, freqOK := parseCleaningFrequency(in.Frequency)
	s.Frequency = freq
	fields := validateCleaningSetup(s)
	if !freqOK {
		fields["frequency"] = cleaningFrequencyWords
	}
	for k, v := range CleaningPointProblems(s.Points, e.nodeService, e.dispatcher) {
		fields[k] = v
	}
	if len(fields) > 0 {
		return &CleaningSetupRefusal{Fields: fields}
	}
	old, hadOld, err := cleaning.GetSetup(e.db.DB)
	if err != nil {
		return err
	}
	if err := cleaning.SaveSetup(e.db.DB, s); err != nil {
		return err
	}
	oldText := ""
	if hadOld {
		oldText = cleaningSetupText(old)
	}
	if err := e.auditService.Append("cleaning", 0, "setup", oldText, cleaningSetupText(s), actor); err != nil {
		e.dbg("cleaning: audit setup: %v", err)
	}
	return nil
}

const cleaningFrequencyWords = "Choose 1 h, 2 h, 4 h or 8 h."

// parseCleaningFrequency reads the frequency as typed, spaces allowed ("4 h"),
// and reports whether it is one of cleaningFrequencies.
func parseCleaningFrequency(text string) (time.Duration, bool) {
	d, err := time.ParseDuration(strings.ReplaceAll(strings.TrimSpace(text), " ", ""))
	if err != nil {
		return 0, false
	}
	for _, f := range cleaningFrequencies {
		if d == f {
			return d, true
		}
	}
	return d, false
}

// validateCleaningSetup checks everything but the points, which need the
// database. Always returns a non-nil map.
func validateCleaningSetup(s cleaning.Setup) map[string]string {
	fields := map[string]string{}
	okFreq := false
	for _, f := range cleaningFrequencies {
		if s.Frequency == f {
			okFreq = true
		}
	}
	if !okFreq {
		fields["frequency"] = cleaningFrequencyWords
	}
	if _, ok := parseClock(s.ActiveFrom); !ok {
		fields["active_from"] = "A time of day, HH:MM."
	}
	if _, ok := parseClock(s.ActiveTo); !ok {
		fields["active_to"] = "A time of day, HH:MM."
	}
	if s.MinBattery < 0 || s.MinBattery > 100 {
		fields["min_battery"] = "A percent, 0 to 100."
	}
	return fields
}

// cleaningSetupText is the audit row's rendering of a setup.
func cleaningSetupText(s cleaning.Setup) string {
	return fmt.Sprintf("points=%s every=%s hours=%s-%s min_battery=%d%%",
		strings.Join(s.Points, ","), s.Frequency, s.ActiveFrom, s.ActiveTo, s.MinBattery)
}

// parseClock reads "HH:MM" as minutes past midnight.
func parseClock(s string) (int, bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// inCleaningWindow reports whether now is inside the plant-local active hours.
// from == to is the whole day; from > to wraps midnight (22:00–06:00). A zone
// nobody set is UTC, nobody's wall clock, so the hours would mean nothing:
// the window is then always open and CleaningWords says so. Hours that do not
// parse (only possible by a hand-written row) also leave it open.
func inCleaningWindow(now time.Time, from, to string, z planttime.Zone) bool {
	if !z.Set() {
		return true
	}
	f, ok1 := parseClock(from)
	t, ok2 := parseClock(to)
	if !ok1 || !ok2 || f == t {
		return true
	}
	l := now.In(z.Loc)
	m := l.Hour()*60 + l.Minute()
	if f < t {
		return m >= f && m < t
	}
	return m >= f || m < t
}

// CleaningWordsFacts is what the Cleaning tab's live words are computed from.
type CleaningWordsFacts struct {
	Points        []string
	PointProblems map[string]string
	CanReport     bool
	FleetUp       bool
	Zone          planttime.Zone
	ActiveFrom    string
	ActiveTo      string
	Now           time.Time
}

// CleaningWords is the §33.2 live words for the clean mission's title:
// "off" with no points, "stopped: <reason>" when the loop cannot send, else
// "running" (with the active hours' state). The reasons are the tick's own
// stop conditions in the tick's order.
func CleaningWords(f CleaningWordsFacts) string {
	switch {
	case len(f.Points) == 0:
		return "off"
	case len(f.PointProblems) > 0:
		return "stopped: " + firstPointProblem(f.PointProblems)
	case !f.CanReport:
		return "stopped: the fleet cannot report a mission's state"
	case !f.FleetUp:
		return "stopped: the fleet is not connected"
	case !f.Zone.Set():
		return "running · active hours ignored: no plant timezone is set"
	case !inCleaningWindow(f.Now, f.ActiveFrom, f.ActiveTo, f.Zone):
		return "running · outside active hours"
	}
	return "running"
}
