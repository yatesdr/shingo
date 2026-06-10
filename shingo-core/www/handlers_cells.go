package www

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/shared/clock"
)

// Production-heartbeat read endpoints (plan §12 slice 5). Cell id = station.
// since/until accept RFC3339 or a bare date; default window is the last 8h
// (the run/stop strip span).

func parseCellWindow(r *http.Request) (since, until time.Time) {
	now := clock.Now().UTC()
	since, until = now.Add(-8*time.Hour), now
	if t, ok := parseTimeParam(r.URL.Query().Get("since")); ok {
		since = t
	}
	if t, ok := parseTimeParam(r.URL.Query().Get("until")); ok {
		until = t
	}
	return since, until
}

func parseTimeParam(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// GET /api/cells/{id}/heartbeat?since=&until= — the cell's windowed pulse
// history split per Process (primary + subs), each with its events and loss
// metrics. Powers the cell drill. cell_id resolves through cell_config; an
// unconfigured id falls back to the station-grain whole stream.
func (h *Handlers) apiCellHeartbeat(w http.ResponseWriter, r *http.Request) {
	cellID := chi.URLParam(r, "id")
	since, until := parseCellWindow(r)
	hb, err := h.engine.HeartbeatService().ResolveCellHeartbeat(cellID, since, until)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(hb)
}

// GET /api/cells/{id}/stops?since=&until= — discrete stop events for the cell,
// at the station grain (cell_id resolved to its station).
func (h *Handlers) apiCellStops(w http.ResponseWriter, r *http.Request) {
	cellID := chi.URLParam(r, "id")
	since, until := parseCellWindow(r)
	stops, err := h.engine.HeartbeatService().StopsForCell(cellID, since, until)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"cell_id": cellID, "stops": stops})
}

// GET /api/cells/{id}/state — live cell state resolved through cell_config:
// the primary Process's rhythm plus each configured sub-Process (Phase E,
// Q-025). When {id} isn't a configured cell it falls back to the station-grain
// whole-stream state (Phase B behavior). Called on SSE updates.
func (h *Handlers) apiCellState(w http.ResponseWriter, r *http.Request) {
	cellID := chi.URLParam(r, "id")
	state, err := h.engine.HeartbeatService().ResolveCellState(cellID, clock.Now().UTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

// handleCellsAdmin renders the /admin/cells management page (auth-gated). The
// page lists cell_config rows and provides create/edit/delete with a live
// Process picker (Phase E, Q-025).
func (h *Handlers) handleCellsAdmin(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "cells.html", map[string]any{"Page": "cells"})
}

// handleHeartbeatKiosk renders the chromeless /heartbeat wall display (Phase F)
// — a grid of cell tiles plus a live rhythm strip, fed by cell-heartbeat SSE.
// Public (no nav); meant to run full-screen on a floor monitor.
func (h *Handlers) handleHeartbeatKiosk(w http.ResponseWriter, r *http.Request) {
	h.renderBare(w, "heartbeat.html", map[string]any{})
}

// GET /api/cells — every configured cell (Phase E, Q-025). Empty list on first
// deploy (cells are configured via /admin/cells). Public read: the /missions
// Cells D section and the /heartbeat kiosk consume it.
func (h *Handlers) apiCellsList(w http.ResponseWriter, r *http.Request) {
	cells, err := h.engine.HeartbeatService().ListCells()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, cells)
}

// GET /api/cells/processes?station= — the Processes ticking for a station, for
// the /admin/cells picker. process_id is opaque on its own, so each option
// carries a style/payload hint to recognize which Process it is.
func (h *Handlers) apiCellProcesses(w http.ResponseWriter, r *http.Request) {
	station := strings.TrimSpace(r.URL.Query().Get("station"))
	if station == "" {
		h.jsonError(w, "station is required", http.StatusBadRequest)
		return
	}
	procs, err := h.engine.HeartbeatService().CellProcesses(station)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, procs)
}

// POST /api/cells — create or update a cell (admin). Keyed on cell_id.
func (h *Handlers) apiCellUpsert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CellID           string  `json:"cell_id"`
		Station          string  `json:"station"`
		PrimaryProcessID int64   `json:"primary_process_id"`
		SubProcessIDs    []int64 `json:"sub_process_ids"`
		DisplayName      string  `json:"display_name"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	req.CellID = strings.TrimSpace(req.CellID)
	req.Station = strings.TrimSpace(req.Station)
	if req.CellID == "" || req.Station == "" {
		h.jsonError(w, "cell_id and station are required", http.StatusBadRequest)
		return
	}
	if req.PrimaryProcessID == 0 {
		h.jsonError(w, "primary_process_id is required", http.StatusBadRequest)
		return
	}
	if err := h.engine.HeartbeatService().UpsertCell(req.CellID, req.Station, req.PrimaryProcessID, req.SubProcessIDs, req.DisplayName); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// DELETE /api/cells/{id} — remove a cell (admin).
func (h *Handlers) apiCellDelete(w http.ResponseWriter, r *http.Request) {
	cellID := strings.TrimSpace(chi.URLParam(r, "id"))
	if cellID == "" {
		h.jsonError(w, "cell id is required", http.StatusBadRequest)
		return
	}
	if err := h.engine.HeartbeatService().DeleteCell(cellID); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}
