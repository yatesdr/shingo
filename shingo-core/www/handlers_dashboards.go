package www

import (
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"

	"shingocore/service"
)

// dashboardTemplates maps a dashboard kind to the chromeless template that
// renders it. This is the platform's extensibility seam: the platform itself
// is kind-agnostic; adding a new dashboard kind means registering a renderer
// template here (and a matching branch in dashboard.js). v1 ships one kind.
var dashboardTemplates = map[string]string{
	"task-board": "dashboard-display.html",
	"robot-map":  "dashboard-map.html",
	"heartbeat":  "heartbeat.html",
}

// handleDashboardDisplay renders a dashboard. By default it renders INSIDE
// Core's chrome (nav stays — you're never stranded off-core, SB's call): a thin
// frame with a Fullscreen link around the kiosk page in an iframe. With
// ?kiosk=1 it renders the chromeless wall-monitor page itself (what the iframe
// loads, and what Fullscreen opens). The dashboard config is baked in
// server-side; the kiosk JS pulls live data from the public board API.
func (h *Handlers) handleDashboardDisplay(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid dashboard id", http.StatusBadRequest)
		return
	}
	d, err := h.engine.DashboardService().Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if d == nil {
		http.Error(w, "dashboard not found", http.StatusNotFound)
		return
	}
	tmpl, ok := dashboardTemplates[d.Kind]
	if !ok {
		http.Error(w, "unsupported dashboard kind: "+d.Kind, http.StatusNotImplemented)
		return
	}
	if r.URL.Query().Get("kiosk") == "1" {
		h.renderBare(w, tmpl, map[string]any{"Dashboard": d})
		return
	}
	h.render(w, r, "dashboard-frame.html", map[string]any{"Page": "dashboard", "Dashboard": d})
}

// handleBoardKindRedirect powers the nav's Dashboards dropdown entries
// (Flight Board / Robot Map): redirect to the first enabled board of {kind} by
// sort order. With none configured, fall through to the manage page so the
// operator can create one (rather than a dead 404). Heartbeat has its own
// route (/heartbeat) because it additionally falls back to a bare kiosk.
func (h *Handlers) handleBoardKindRedirect(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	if boards, err := h.engine.DashboardService().List(); err == nil {
		best := -1
		for i := range boards {
			d := boards[i]
			if d.Kind == kind && d.Enabled && (best < 0 || d.SortOrder < boards[best].SortOrder) {
				best = i
			}
		}
		if best >= 0 {
			http.Redirect(w, r, "/dashboard/"+strconv.FormatInt(boards[best].ID, 10), http.StatusFound)
			return
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther) // none configured → the hub
}

// handleDashboardsAdmin is retired (refactor #3): the standalone Manage table is
// replaced by the dashboard hub on the Dashboard page. Kept as a redirect so old
// bookmarks/links land on the hub rather than 404.
func (h *Handlers) handleDashboardsAdmin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/", http.StatusFound)
}

// apiListDashboards returns every dashboard definition. Public read so a
// future standalone display host can pull the catalog over the wire.
func (h *Handlers) apiListDashboards(w http.ResponseWriter, r *http.Request) {
	list, err := h.engine.DashboardService().List()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, list)
}

// apiGetDashboard returns one dashboard definition by id.
func (h *Handlers) apiGetDashboard(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	d, err := h.engine.DashboardService().Get(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if d == nil {
		h.jsonError(w, "dashboard not found", http.StatusNotFound)
		return
	}
	h.jsonOK(w, d)
}

// apiDashboardCells returns the cells a heartbeat dashboard shows — scoped to
// its stations with its per-dashboard overrides applied (refactor #4). Public:
// the heartbeat kiosk reads it (in place of /api/cells) when rendered as a board.
func (h *Handlers) apiDashboardCells(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	d, err := h.engine.DashboardService().Get(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if d == nil {
		h.jsonError(w, "dashboard not found", http.StatusNotFound)
		return
	}
	cells, err := h.engine.HeartbeatService().DashboardCells(d.Stations, d.Config)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, cells)
}

// apiCreateDashboard inserts a dashboard (auth-gated). Validation failures
// (empty name, bad config JSON) surface as 400.
func (h *Handlers) apiCreateDashboard(w http.ResponseWriter, r *http.Request) {
	var in service.DashboardInput
	if !h.parseJSON(w, r, &in) {
		return
	}
	id, err := h.engine.DashboardService().Create(in)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.jsonOK(w, map[string]int64{"id": id})
}

// apiUpdateDashboard overwrites a dashboard (auth-gated).
func (h *Handlers) apiUpdateDashboard(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	var in service.DashboardInput
	if !h.parseJSON(w, r, &in) {
		return
	}
	if err := h.engine.DashboardService().Update(id, in); err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.jsonSuccess(w)
}

// apiDeleteDashboard removes a dashboard (auth-gated).
func (h *Handlers) apiDeleteDashboard(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.engine.DashboardService().Delete(id); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiStations returns the selectable station IDs for dashboard area scoping.
// The board filter matches orders.station_id exactly, so the list is built
// from values that can actually match: the distinct stations seen on orders,
// plus registered edges (station ID, and station.line composites for each
// registered line — so a fresh line is offerable before its first order).
// The dashboards admin renders these as checkboxes instead of a free-text
// field, where a typo silently scoped a board to nothing.
func (h *Handlers) apiStations(w http.ResponseWriter, r *http.Request) {
	set := map[string]bool{}
	fromOrders, oErr := h.engine.OrderService().ListOrderStations()
	for _, s := range fromOrders {
		if s != "" {
			set[s] = true
		}
	}
	edges, eErr := h.engine.NodeService().ListEdges()
	for _, e := range edges {
		if e.StationID == "" {
			continue
		}
		if len(e.LineIDs) == 0 {
			set[e.StationID] = true
		}
		for _, ln := range e.LineIDs {
			set[e.StationID+"."+ln] = true
		}
	}
	if oErr != nil && eErr != nil {
		h.jsonError(w, oErr.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	h.jsonOK(w, out)
}
