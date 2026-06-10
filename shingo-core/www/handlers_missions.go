package www

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/shared/clock"
	"shingocore/domain"
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"missions": missions,
		"total":    total,
		"limit":    f.Limit,
		"offset":   f.Offset,
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
		"events":    events,
		"history":   history,
	})
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
func (h *Handlers) apiMissionBreakdown(w http.ResponseWriter, r *http.Request) {
	f := parseMissionFilter(r)
	by := r.URL.Query().Get("by")
	if by != "route" {
		by = "robot"
	}
	rows, err := h.engine.MissionService().Breakdown(f, by)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []domain.TelemetryBreakdownRow{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"by": by, "rows": rows})
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
func (h *Handlers) apiMissionsActive(w http.ResponseWriter, r *http.Request) {
	n, err := h.engine.OrderService().CountActiveOrders()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
