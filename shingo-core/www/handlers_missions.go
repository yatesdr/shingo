package www

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/shared/clock"
	"shingocore/domain"
	"shingocore/engine"
	"shingocore/fleet"
)

func (h *Handlers) handleMissions(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Page": "missions",
	}
	h.render(w, r, "missions.html", data)
}

func (h *Handlers) handleMissionDetail(w http.ResponseWriter, r *http.Request) {
	orderIDStr := chi.URLParam(r, "orderID")
	orderID, err := strconv.ParseInt(orderIDStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}

	order, err := h.engine.OrderService().GetOrder(orderID)
	if err != nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}

	data := map[string]any{
		"Page":    "missions",
		"OrderID": order.ID,
	}
	h.render(w, r, "mission-detail.html", data)
}

func (h *Handlers) apiListMissions(w http.ResponseWriter, r *http.Request) {
	f := parseMissionFilter(r)
	missions, total, err := h.engine.MissionService().List(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// station_names is the uid→label dictionary, shipped once per response
	// rather than decorated onto every mission row. The rows keep the key —
	// station_id is what the filter, the CSV export and the drill-down all send
	// back — and the page renders the label beside it. Denormalising the name
	// onto each row would put a copy of a mutable value in a payload that gets
	// cached, exported and compared, which is the failure this split exists to
	// end. A station with no enrolled row is simply absent from the map and the
	// page falls back to the raw id.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"missions":      missions,
		"total":         total,
		"limit":         f.Limit,
		"offset":        f.Offset,
		"station_names": h.engine.NodeService().StationNames(),
	})
}

func (h *Handlers) apiGetMission(w http.ResponseWriter, r *http.Request) {
	orderIDStr := chi.URLParam(r, "orderID")
	orderID, err := strconv.ParseInt(orderIDStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}

	order, err := h.engine.OrderService().GetOrder(orderID)
	if err != nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}

	telemetry, _ := h.engine.MissionService().Telemetry(orderID)
	events, _ := h.engine.MissionService().ListEvents(orderID)
	history, _ := h.engine.OrderService().ListOrderHistory(orderID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"order":     order,
		"telemetry": telemetry,
		"events":    h.missionEventViews(events),
		"history":   history,
	})
}

// missionEventView is one mission event plus the Core status its vendor state
// means. old_state / new_state are kept verbatim beside it.
//
// BOTH, NOT EITHER. The page speaks Core's vocabulary because that is what
// every other surface speaks and what an operator is looking at elsewhere; but
// this is the FLEET view, and when a mission stalls the thing you need to know
// is what RDS actually said. Dropping the raw state to tidy the labels would
// take the diagnostic value out of the one page that exists to carry it.
type missionEventView struct {
	*domain.TelemetryEvent
	OldStatus string `json:"old_status"`
	NewStatus string `json:"new_status"`
	// IsLeg marks a per-block completion row rather than an order transition.
	// The page used to decide this by comparing new_state against its own copy
	// of the marker string; asking here removes that spelling and keeps the
	// value in one place (engine.BlockLegState).
	IsLeg bool `json:"is_leg"`
	// Blocks is blocks_json decoded and stamped with the same mapping. The
	// chips under a timeline row are the same vocabulary as the row itself, so
	// leaving them in vendor words while the badge above them speaks Core's
	// would be a worse mixture than what this change set out to fix. The raw
	// blocks_json stays on the row untouched.
	Blocks []missionBlockView `json:"blocks"`
}

// missionBlockView is one block snapshot plus the Core status its state means.
type missionBlockView struct {
	fleet.BlockSnapshot
	Status string `json:"status"`
}

// missionEventViews stamps each event with the Core status its vendor state
// maps onto.
//
// MAPPED HERE RATHER THAN IN THE PAGE, AND THAT IS THE POINT OF THE CHANGE.
// mission-detail.js carried its own hand-written copy of this mapping and the
// two had drifted apart on three of seven rows: RDS CREATED read as "created"
// where Core says dispatched, FINISHED as "completed" where Core says
// delivered, and — the one that matters — FAILED as "failed" where Core says
// FAULTED. faulted is the non-terminal grace state with a recovery timer
// running; failed is terminal. The page was telling an engineer a mission was
// dead while Core still expected it back. It also collapsed Core's delivered
// and confirmed into one invented word, losing "the bin arrived" versus "the
// operator signed for it".
//
// So the mapping now comes from the SAME seam the engine dispatches on —
// fleet.Backend.MapState, which wiring_vendor_status.go uses to decide the
// order's real status. Two spellings of one mapping is the failure this
// codebase keeps paying for; there is now one, and the page renders what Core
// actually believes rather than a second opinion about it.
//
// An empty state maps to empty, never through MapState. Leg rows carry no
// old_state — nothing transitioned — and MapState's default arm answers
// "dispatched" for anything it does not recognise AND logs it, so passing ""
// through would both invent a transition and print a line per leg per mission.
func (h *Handlers) missionEventViews(events []*domain.TelemetryEvent) []missionEventView {
	out := make([]missionEventView, 0, len(events))
	for _, ev := range events {
		view := missionEventView{
			TelemetryEvent: ev,
			IsLeg:          ev.NewState == engine.BlockLegState,
			Blocks:         h.missionBlockViews(ev.BlocksJSON),
		}
		// A leg row gets NO status, and that is not an omission. Its new_state
		// is Core's own per-block marker rather than a vendor state, so there is
		// no transition to name — the row says "this block finished", and the
		// page renders it as a leg. Mapping it anyway would put every leg row
		// through MapState's unrecognised arm and invent "dispatched" for it.
		if !view.IsLeg {
			view.OldStatus = h.coreStatusFor(ev.OldState)
			view.NewStatus = h.coreStatusFor(ev.NewState)
		}
		out = append(out, view)
	}
	return out
}

// missionBlockViews decodes one event's blocks_json and stamps each block with
// the Core status its vendor state means.
//
// Unparseable JSON yields no blocks rather than an error: this is a display
// detail on a diagnostic page, and a malformed snapshot from one poll must not
// cost the operator the whole timeline. The raw string is still on the row for
// anyone who needs to look at it.
func (h *Handlers) missionBlockViews(blocksJSON string) []missionBlockView {
	if blocksJSON == "" || blocksJSON == "[]" {
		return nil
	}
	var snaps []fleet.BlockSnapshot
	if err := json.Unmarshal([]byte(blocksJSON), &snaps); err != nil {
		return nil
	}
	out := make([]missionBlockView, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, missionBlockView{BlockSnapshot: s, Status: h.coreStatusFor(s.State)})
	}
	return out
}

// coreStatusFor translates one vendor state through the fleet adapter. Empty in,
// empty out. A nil backend (bare test fixtures) degrades to the raw string
// rather than panicking on a display path.
func (h *Handlers) coreStatusFor(vendorState string) string {
	if vendorState == "" {
		return ""
	}
	backend := h.engine.Fleet()
	if backend == nil {
		return vendorState
	}
	return backend.MapState(vendorState)
}

func (h *Handlers) apiMissionStats(w http.ResponseWriter, r *http.Request) {
	f := parseMissionFilter(r)
	stats, err := h.engine.MissionService().Stats(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// apiMissionStatsV2 serves the corrected dashboard mission stats (plan §3.A /
// §8 #5). A sibling endpoint, not a replacement, so the legacy
// /api/missions/stats keeps returning the old number for current consumers.
// The hero's delta is computed client-side by calling this twice (current +
// previous equal-length window).
func (h *Handlers) apiMissionStatsV2(w http.ResponseWriter, r *http.Request) {
	f := parseMissionFilter(r)
	stats, err := h.engine.MissionService().StatsV2(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// apiMissionTimeseries serves bucketed mission metrics for the trend charts
// (plan §3.B / §15.B). bucket is hour (default) or day. One response carries
// every metric per bucket so the 2×2 grid and hero sparklines share a fetch.
func (h *Handlers) apiMissionTimeseries(w http.ResponseWriter, r *http.Request) {
	f := parseMissionFilter(r)
	bucket := r.URL.Query().Get("bucket")
	if bucket != "day" {
		bucket = "hour"
	}
	points, err := h.engine.MissionService().Timeseries(f, bucket)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if points == nil {
		points = []domain.TelemetryBucket{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"bucket": bucket, "points": points})
}

// apiMissionBreakdown serves the §3.F breakdown panels: top-10 missions
// grouped by robot or route. ?by=robot (default) | route.
//
// The by=robot response carries U3's route index — duration ÷ that route's median
// — alongside two things the client cannot work out for itself:
//
//	index_available   false when NO route had enough missions to be a denominator.
//	                  The client drops the whole column, rather than rendering one
//	                  that is empty for every row: an empty column reads as "these
//	                  robots have no index", which is a claim about the robots
//	                  instead of about the sample.
//	min_route_samples the floor that decision was made against, echoed so the
//	                  panel can say WHY the column is gone rather than just
//	                  omitting it. A surface that silently loses a column is
//	                  indistinguishable from one that never had it.
//
// Per-row, route_index is null (never 0) where the robot ran on no qualifying
// route, and index_samples says how many of its missions the index is actually
// over.
func (h *Handlers) apiMissionBreakdown(w http.ResponseWriter, r *http.Request) {
	f := parseMissionFilter(r)
	by := r.URL.Query().Get("by")
	if by != "route" {
		by = "robot"
	}

	resp := map[string]any{"by": by}
	var rows []domain.TelemetryBreakdownRow
	var err error
	if by == "robot" {
		disp := h.engine.AppConfig().DisplayConstants()
		var available bool
		rows, available, err = h.engine.MissionService().BreakdownByRobot(f, disp.RouteIndexMinRouteSamples)
		resp["index_available"] = available
		resp["min_route_samples"] = disp.RouteIndexMinRouteSamples
		resp["min_robot_samples"] = disp.RouteIndexMinRobotSamples
	} else {
		rows, err = h.engine.MissionService().Breakdown(f, by)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []domain.TelemetryBreakdownRow{}
	}
	resp["rows"] = rows
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// apiMissionDwell serves per-state dwell: p50/p95/count for each leg of an
// order's life — time-to-dispatch, transit, staged dwell, operator fill.
//
// Answers "where did the time go", which no surface answered before: every
// existing mission stat is a single created→terminal duration, so a mission
// that spent eight of its nine minutes queued behind material looks identical
// to one that spent them driving.
//
// Window/filter args match the rest of the missions API (?since=&until=
// as plant-local dates, ?payload_code=, ?order_type=). Defaults to the last
// 30 days. Each row carries its sample count — read the percentiles with it,
// not without it.
func (h *Handlers) apiMissionDwell(w http.ResponseWriter, r *http.Request) {
	f := parseMissionFilter(r)
	end := clock.Now().UTC()
	if f.Until != nil {
		end = *f.Until
	}
	start := end.AddDate(0, 0, -30)
	if f.Since != nil {
		start = *f.Since
	}

	rows, err := h.engine.MissionService().DwellStats(nil,
		r.URL.Query().Get("payload_code"), r.URL.Query().Get("order_type"), start, end)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []domain.DwellStat{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"since": start,
		"until": end,
		"rows":  rows,
	})
}

// apiMissionFailures serves the §3.G failure Pareto: classified failure
// reasons with counts and sample order IDs, sorted desc.
func (h *Handlers) apiMissionFailures(w http.ResponseWriter, r *http.Request) {
	f := parseMissionFilter(r)
	reasons, err := h.engine.MissionService().Failures(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if reasons == nil {
		reasons = []domain.FailureReason{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"reasons": reasons})
}

// apiMissionsActive returns the live count of non-terminal orders — the
// hero "in flight" KPI (plan §3.A / §15.A). Cheap count; the page also
// refreshes it on SSE order-update.
// apiMissionsActive returns the in-flight order count for the Overview tile
// (item 10). station_id/robot_id make it respect the global filter; without
// them it stays on the fast unfiltered count. The active set is small
// (currently-executing orders), so listing + filtering in-process is cheap —
// no bespoke filtered-count query needed.
func (h *Handlers) apiMissionsActive(w http.ResponseWriter, r *http.Request) {
	station := r.URL.Query().Get("station_id")
	robot := r.URL.Query().Get("robot_id")

	if station == "" && robot == "" {
		n, err := h.engine.OrderService().CountActiveOrders()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"count": n})
		return
	}

	active, err := h.engine.OrderService().ListActiveOrders()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n := 0
	for _, o := range active {
		if station != "" && o.StationID != station {
			continue
		}
		if robot != "" && o.RobotID != robot {
			continue
		}
		n++
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"count": n})
}

// apiMissionsAlerts powers the conditional hero alerts banner (plan §3.A /
// §15.A): robots blocked/emergency/error from the fleet cache, plus active
// missions stuck beyond 2× the recent P95 duration. Quiet days return
// total:0 and the banner stays hidden.
func (h *Handlers) apiMissionsAlerts(w http.ResponseWriter, r *http.Request) {
	var blocked, emergency, errored int
	for _, rb := range h.engine.GetAllCachedRobots() {
		if rb.Blocked {
			blocked++
		}
		if rb.Emergency {
			emergency++
		}
		if rb.IsError {
			errored++
		}
	}

	// Stuck threshold = 2× the recent (7-day) P95 mission duration, with a
	// 30-minute fallback before any window has data (cold start, §8 #19).
	thresholdMS := int64(30 * 60 * 1000)
	since := clock.Now().AddDate(0, 0, -7)
	if st, err := h.engine.MissionService().StatsV2(domain.TelemetryFilter{Since: &since}); err == nil && st.P95DurationMS > 0 {
		thresholdMS = 2 * st.P95DurationMS
	}
	cutoff := clock.Now().Add(-time.Duration(thresholdMS) * time.Millisecond)

	var stuck int
	stuckItems := make([]map[string]any, 0, 10)
	if active, err := h.engine.OrderService().ListActiveOrders(); err == nil {
		for _, o := range active {
			if o.CreatedAt.Before(cutoff) {
				stuck++
				if len(stuckItems) < 10 {
					stuckItems = append(stuckItems, map[string]any{
						"order_id":   o.ID,
						"status":     o.Status,
						"created_at": o.CreatedAt,
					})
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"total":            blocked + emergency + errored + stuck,
		"robots_blocked":   blocked,
		"robots_emergency": emergency,
		"robots_error":     errored,
		"stuck_missions":   stuck,
		"stuck_items":      stuckItems,
	})
}

func parseMissionFilter(r *http.Request) domain.TelemetryFilter {
	f := domain.TelemetryFilter{
		StationID: r.URL.Query().Get("station_id"),
		RobotID:   r.URL.Query().Get("robot_id"),
		State:     r.URL.Query().Get("state"),
		Limit:     50,
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			f.Limit = n
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			f.Offset = n
		}
	}
	// Bare dates resolve in the plant timezone (plant-local-at-server, Q-004),
	// then normalize to UTC for the timestamptz comparison. Without the
	// location, a UTC server read "Today" as starting at the plant's previous
	// evening.
	if s := r.URL.Query().Get("since"); s != "" {
		if t, err := time.ParseInLocation("2006-01-02", s, plantLocation); err == nil {
			utc := t.UTC()
			f.Since = &utc
		}
	}
	if u := r.URL.Query().Get("until"); u != "" {
		if t, err := time.ParseInLocation("2006-01-02", u, plantLocation); err == nil {
			end := t.Add(24*time.Hour - time.Nanosecond).UTC()
			f.Until = &end
		}
	}
	return f
}
