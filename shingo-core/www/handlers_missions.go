package www

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"shingocore/store"
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

	order, err := h.engine.GetOrder(orderID)
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
	missions, total, err := h.engine.ListMissions(f)
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

	order, err := h.engine.GetOrder(orderID)
	if err != nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}

	telemetry, _ := h.engine.GetMissionTelemetry(orderID)
	events, _ := h.engine.ListMissionEvents(orderID)
	history, _ := h.engine.ListOrderHistory(orderID)

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
	stats, err := h.engine.GetMissionStats(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func parseMissionFilter(r *http.Request) store.MissionFilter {
	f := store.MissionFilter{
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
	if s := r.URL.Query().Get("since"); s != "" {
		if t, err := time.Parse("2006-01-02", s); err == nil {
			f.Since = &t
		}
	}
	if u := r.URL.Query().Get("until"); u != "" {
		if t, err := time.Parse("2006-01-02", u); err == nil {
			end := t.Add(24*time.Hour - time.Nanosecond)
			f.Until = &end
		}
	}
	return f
}
