package www

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol/clock"
)

// Production-heartbeat read endpoints (plan §12 slice 5). A cell id is a catalog
// cell's label (the PLC an Edge reported); a bare station id reads the station
// grain.
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
// metrics. Powers the cell drill. cell_id resolves to the live catalog cell of
// that label; any other id falls back to the station-grain whole stream.
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

// GET /api/cells/{id}/state — live cell state resolved through the catalog
// cell of that label: the primary Process's rhythm plus each sub-Process
// (Phase E, Q-025). When {id} is no live catalog cell it falls back to the
// station-grain whole-stream state (Phase B behavior). Called on SSE updates.
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

// handleCellsAdmin answers the retired /admin/cells page with a redirect to
// the dashboard hub.
func (h *Handlers) handleCellsAdmin(w http.ResponseWriter, r *http.Request) {
	// Retired (refactor #4): cells auto-derive from the edge catalog and are
	// curated per-dashboard in the heartbeat board's setup, not on a separate
	// global page. Redirect old links to the dashboard hub.
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleHeartbeatKiosk renders the chromeless /heartbeat wall display (Phase F)
// — a grid of cell tiles plus a live rhythm strip, fed by cell-heartbeat SSE.
// Public (no nav); meant to run full-screen on a floor monitor.
func (h *Handlers) handleHeartbeatKiosk(w http.ResponseWriter, r *http.Request) {
	// Wave 2 (Q-035): prefer a configured heartbeat display so wall monitors
	// land on the scoped view; fall back to the unscoped kiosk so existing
	// displays don't break. First enabled heartbeat display by sort order wins.
	//
	// ?kiosk=1 IS REQUIRED ON THIS HOP AND WAS MISSING. /heartbeat is a
	// chromeless route — the fallback below is renderBare — but the redirect
	// landed on the FRAMED page, so a monitor pointed here grew Core's nav bar
	// and an iframe border. Same dropped-query failure the /dashboard/{id}
	// redirect is written to prevent, reached from the other direction: there
	// the query is lost in transit, here it was never attached.
	//
	// AND IT WAS NEVER CONDITIONAL, which is the part worth carrying.
	// engine.Start() calls SeedDefaultDashboards, which creates an enabled
	// full-plant "Plant Heartbeat" whenever no heartbeat display exists — so
	// every Core has had one since refactor #5, this branch has always been
	// taken, and the renderBare below has been unreachable on a started engine
	// for just as long. Both plants carry a row named exactly "Plant
	// Heartbeat": the seed, unrenamed. The fallback is left standing rather
	// than deleted — whether /heartbeat should still be able to render
	// unscoped is a design question, not a redirect fix — but nothing reaches
	// it today. TestHeartbeatKiosk_RedirectStaysChromeless pins both facts.
	if boards, err := h.engine.DashboardService().List(); err == nil {
		best := -1
		for i := range boards {
			d := boards[i]
			if d.Kind == "heartbeat" && d.Enabled && (best < 0 || d.SortOrder < boards[best].SortOrder) {
				best = i
			}
		}
		if best >= 0 {
			http.Redirect(w, r, "/wall-display/"+strconv.FormatInt(boards[best].ID, 10)+"?kiosk=1", http.StatusFound)
			return
		}
	}
	h.renderBare(w, "heartbeat.html", map[string]any{})
}

// GET /api/cells — every live catalog cell (Phase E, Q-034): a cell per PLC an
// Edge reported. Public read: the /missions Cells D section and the /heartbeat
// kiosk consume it.
func (h *Handlers) apiCellsList(w http.ResponseWriter, r *http.Request) {
	cells, err := h.engine.HeartbeatService().ListCells()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, cells)
}

// GET /api/cells/catalog?station= — the auto-derived cell catalog (Q-034): the
// PLC-grouped cells an edge reported at registration. station="" returns every
// station's. This is the universe a heartbeat dashboard's setup picks from; no
// manual config needed for it to exist.
func (h *Handlers) apiCellsCatalog(w http.ResponseWriter, r *http.Request) {
	station := strings.TrimSpace(r.URL.Query().Get("station"))
	cells, err := h.engine.HeartbeatService().CellCatalog(station)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, cells)
}
