package www

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"shingocore/engine"
	"shingocore/fleet"
	"shingocore/service"
)

// ── The Robots page's Fleet and Cleaning tabs ──────────────────────────────
//
// Reads are public, like every other robots read; the three writes sit behind
// requireAuth with the robot controls. Every write leaves an audit row: Clean
// and the setup in the engine, the opt-out in NodeService.

// hoursPriorDays is the Fleet tab's history span before today. The only value
// the page asks for; the parameter exists so a reader of the API can ask for
// another, bounded by the service.
const hoursPriorDays = 7

// apiRobotHours is the Fleet tab's history: per robot, today and the prior
// days, summed from the hourly record in the plant's zone (Q-004).
// GET /api/robots/hours?days=7
func (h *Handlers) apiRobotHours(w http.ResponseWriter, r *http.Request) {
	days := hoursPriorDays
	if s := r.URL.Query().Get("days"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			h.jsonError(w, "days must be a whole number", http.StatusBadRequest)
			return
		}
		days = n
	}
	view, err := h.engine.NodeService().RobotHoursAt(plantLocation, time.Now(), days)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.jsonOK(w, view)
}

// cleaningRobotLine is one robot on the Cleaning tab and in the robot pop-up:
// the record's line, and why the robot cannot be cleaned now ("" when it can).
type cleaningRobotLine struct {
	service.CleaningRobotView
	Refusal string `json:"refusal"`
}

// cleaningPage is GET /api/robots/cleaning's answer.
type cleaningPage struct {
	Setup    service.CleaningSetupView     `json:"setup"`
	Words    string                        `json:"words"`
	Problems map[string]string             `json:"problems"`
	Robots   []cleaningRobotLine           `json:"robots"`
	Missions []service.CleaningMissionView `json:"missions"`
	Since    time.Time                     `json:"since"`
}

// apiRobotCleaning is the Cleaning tab and the robot pop-up's Clean state.
//
// THE REFUSAL IS THE POST'S. Each robot's refusal is engine.CleanNowRefusal
// over the same facts CleanNow judges, so a disabled Clean button's reason and
// the words a POST refuses with are one sentence. The one input that can
// differ is the robot's status: the page reads the robot cache, CleanNow the
// latest poll, and the two disagree only about a robot the fleet stopped
// listing, which CleanNow refuses as "not in the fleet's latest report".
// GET /api/robots/cleaning
func (h *Handlers) apiRobotCleaning(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	rec, err := h.engine.NodeService().CleaningRecordAt(plantLocation, now)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setup := rec.Setup
	problems := engine.CleaningPointProblems(setup.Points, h.engine.NodeService(), h.engine.Dispatcher())
	canReport := engine.CanReportCleaningMissions(h.engine.Fleet())

	lines := map[string]*cleaningRobotLine{}
	for _, l := range rec.Robots {
		lines[l.VehicleID] = &cleaningRobotLine{CleaningRobotView: l}
	}
	cached := map[string]fleet.RobotStatus{}
	for _, rs := range h.engine.GetAllCachedRobots() {
		cached[rs.VehicleID] = rs
		if lines[rs.VehicleID] == nil {
			lines[rs.VehicleID] = &cleaningRobotLine{CleaningRobotView: service.CleaningRobotView{VehicleID: rs.VehicleID}}
		}
	}
	out := cleaningPage{
		Setup: setup, Problems: problems,
		Missions: rec.Missions, Since: rec.Since,
		Words: engine.CleaningWords(engine.CleaningWordsFacts{
			Points: setup.Points, PointProblems: problems, CanReport: canReport,
		}),
	}
	for vid, l := range lines {
		f := engine.CleanNowFacts{
			SetUp: len(setup.Points) > 0, PointProblems: problems, CanReport: canReport,
			AnyOpen: rec.AnyOpen, OptedOut: l.OptedOut, MinBattery: setup.MinBattery,
		}
		if rs, ok := cached[vid]; ok {
			f.Robot = &rs
		}
		l.Refusal = engine.CleanNowRefusal(f)
		out.Robots = append(out.Robots, *l)
	}
	sort.Slice(out.Robots, func(i, j int) bool { return out.Robots[i].VehicleID < out.Robots[j].VehicleID })
	if out.Robots == nil {
		out.Robots = []cleaningRobotLine{}
	}
	h.jsonOK(w, out)
}

// apiRobotCleaningSetup saves the plant's one cleaning mission. A refusal is
// the config page's shape, {ok: false, error, errors: {field: msg}}, so the
// setup block draws each message under its row (points.<i> under point i).
// POST /api/robots/cleaning/setup {points[], frequency, min_battery}
func (h *Handlers) apiRobotCleaningSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Points     []string `json:"points"`
		Frequency  string   `json:"frequency"`
		MinBattery int      `json:"min_battery"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	err := h.orchestration.SaveCleaningSetup(engine.CleaningSetupInput{
		Points: req.Points, Frequency: req.Frequency, MinBattery: req.MinBattery,
	}, h.getUsername(r))
	var refusal *engine.CleaningSetupRefusal
	switch {
	case errors.As(err, &refusal):
		writeConfigJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "Not saved. Fix the marked fields.", "errors": refusal.Fields,
		})
	case err != nil:
		writeConfigJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
	default:
		writeConfigJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// apiRobotCleanNow sends one robot through the knife now. No confirmation on
// the page (§11: nothing is deleted or lost); the refusal is CleanNowRefusal's.
// POST /api/robots/cleaning/clean-now {vehicle_id}
func (h *Handlers) apiRobotCleanNow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VehicleID string `json:"vehicle_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.VehicleID == "" {
		h.jsonError(w, "vehicle_id is required", http.StatusBadRequest)
		return
	}
	if err := h.orchestration.CleanNow(req.VehicleID, h.getUsername(r)); err != nil {
		h.jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	h.jsonSuccess(w)
}

// apiRobotCleaningOptOut records or clears one robot's opt-out, saved on
// change like every other robot control.
// POST /api/robots/cleaning/opt-out {vehicle_id, out}
func (h *Handlers) apiRobotCleaningOptOut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VehicleID string `json:"vehicle_id"`
		Out       bool   `json:"out"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.VehicleID == "" {
		h.jsonError(w, "vehicle_id is required", http.StatusBadRequest)
		return
	}
	if err := h.engine.NodeService().SetCleaningOptOut(req.VehicleID, req.Out, h.getUsername(r)); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}
