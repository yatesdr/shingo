package www

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"

	"shingocore/domain"
	"shingocore/service"
	"shingo/protocol"
)

// dashboardTemplates maps a wall-display kind to the chromeless template that
// renders it. This is the platform's extensibility seam: the platform itself
// is kind-agnostic; adding a kind means registering a renderer template here
// (and a matching branch in the page JS).
//
// FOUR KINDS, and the map is the count. The comment here read "v1 ships one
// kind" while the literal below it registered four — a line that was true when
// written and has been wrong through three additions since, which is exactly
// how a reader ends up trusting prose over the code beside it. Both plants run
// three of the four today (Springfield also runs node-report).
var dashboardTemplates = map[string]string{
	"task-board":  "dashboard-display.html",
	"robot-map":   "dashboard-map.html",
	"heartbeat":   "heartbeat.html",
	"node-report": "dashboard-node-report.html",
}

// handleWallDisplay renders one wall display. By default it renders INSIDE
// Core's chrome (nav stays — you're never stranded off-core, SB's call): a thin
// frame with a Fullscreen link around the kiosk page in an iframe. With
// ?kiosk=1 it renders the chromeless wall-monitor page itself (what the iframe
// loads, and what Fullscreen opens). The config is baked in server-side; the
// kiosk JS pulls live data from the public board API.
//
// Each kiosk template renders the display's own NAME in its header — see
// dashboard-frame.html's comment, which records that the frame drops its title
// precisely because all four kiosks carry theirs. A chromeless screen has to
// say what it is; the framed one must not say it twice.
func (h *Handlers) handleWallDisplay(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid wall display id", http.StatusBadRequest)
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

// handleWallDisplayMoved answers the old /dashboard/{id} with a permanent
// redirect to /wall-display/{id}.
//
// IT CARRIES THE QUERY STRING, and that is the whole reason this is a handler
// and not a one-line closure. ?kiosk=1 selects the chromeless page; the bare
// path selects the framed one. A wall monitor is pointed at the kiosk form and
// a person clicks the framed form, so a redirect that drops the query does not
// 404 and does not error — it quietly returns every floor screen in the plant
// with Core's nav bar across the top, and nobody finds out until they look at
// a monitor. Both plants run three enabled displays each; there is no version
// of this worth shipping a round later than the rename.
//
// The id is re-parsed rather than pasted through, so the Location this emits
// is always a number this route could have produced.
func (h *Handlers) handleWallDisplayMoved(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid wall display id", http.StatusBadRequest)
		return
	}
	target := "/wall-display/" + strconv.FormatInt(id, 10)
	if q := r.URL.RawQuery; q != "" {
		target += "?" + q
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// handleBoardKindRedirect resolves /board/{kind} to the first enabled wall
// display of that kind by sort order, for a typed or bookmarked URL. With none
// configured it falls through to the hub so the operator can create one rather
// than hit a dead 404. Heartbeat has its own route (/heartbeat) because it
// additionally falls back to a bare kiosk.
//
// The framed form is right here: this route is reached by a person, never by a
// monitor. It used to be described as powering "the nav's Dashboards dropdown
// entries (Flight Board / Robot Map)" — there is no such dropdown in
// layout.html and nothing in the repo links to /board/{kind} at all.
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
			http.Redirect(w, r, "/wall-display/"+strconv.FormatInt(boards[best].ID, 10), http.StatusFound)
			return
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther) // none configured → the hub
}

// handleWallDisplaysMoved backs both /wall-displays and the old /dashboards.
// Neither is a page: the standalone Manage table was retired in refactor #3 and
// wall displays are made and edited on the hub at "/", so both send you there.
//
// /wall-displays is registered so the URL owner decision 11 names resolves to
// something. It is NOT a new management page — decision 11 read as if
// /dashboards still managed them, and it has not since refactor #3.
//
// Both are PUBLIC; see the routing comment for why /dashboards moved out of the
// auth group. This handler reads nothing and can leak nothing.
func (h *Handlers) handleWallDisplaysMoved(w http.ResponseWriter, r *http.Request) {
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
	// ONE ENTRY PER REGISTERED EDGE, ITS STATION ID, AND NOTHING COMPOSED.
	//
	// This used to also emit e.StationID + "." + ln for every line id on the
	// row, which at Springfield produced 'plant-a.line-1.line-1' — an option in
	// the dashboard scope picker that NO row from ListOrderStations() can ever
	// match, so selecting it scoped a board to nothing. It was not a typo: the
	// register payload sent []string{cfg.LineID} regardless of any station
	// override, so 'line-1' was the only value it could ever carry and the
	// composition was wrong for every plant, always. The field is retired.
	edges, eErr := h.engine.NodeService().ListEdges()
	for _, e := range edges {
		if e.StationID == "" {
			continue
		}
		set[e.StationID] = true
	}
	if oErr != nil && eErr != nil {
		h.jsonError(w, oErr.Error(), http.StatusInternalServerError)
		return
	}
	// THE ID IS WHAT ROUND-TRIPS; THE LABEL IS ONLY EVER READ.
	//
	// Every consumer of this list is a picker or a filter, and each one sends a
	// value back: dashboards.stations_json stores the selected ids,
	// cell_config.station stores the typed one, and the missions/overview
	// filters put theirs in a query string that is matched against
	// orders.station_id exactly. So the option carries both — the caller binds
	// `id` to the value it will submit and `label` to the text a person reads.
	// Collapsing them back into one string is how a display name ends up in a
	// stored column, which is the failure the whole display-name split exists
	// to prevent (store/registry/registry.go:345-350).
	//
	// Unenrolled stations resolve to themselves, so core-spot / core-direct /
	// core-test render as they always have.
	type stationOption struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	ids := make([]string, 0, len(set))
	for s := range set {
		ids = append(ids, s)
	}
	sort.Strings(ids)

	ns := h.engine.NodeService()
	out := make([]stationOption, 0, len(ids))
	for _, id := range ids {
		out = append(out, stationOption{ID: id, Label: ns.StationName(id)})
	}
	h.jsonOK(w, out)
}

// apiDashboardNodeReport returns the live bin state for every node in the
// loader referenced by a node-report dashboard's config_json. Public: the
// chromeless kiosk reads it.
func (h *Handlers) apiDashboardNodeReport(w http.ResponseWriter, r *http.Request) {
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

	var cfg struct {
		LoaderID int64 `json:"loader_id"`
	}
	if err := json.Unmarshal(d.Config, &cfg); err != nil || cfg.LoaderID == 0 {
		h.jsonError(w, "dashboard config missing loader_id", http.StatusBadRequest)
		return
	}

	svc := h.engine.LoaderService()
	loader, err := svc.Get(cfg.LoaderID)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if loader == nil {
		h.jsonError(w, "loader not found", http.StatusNotFound)
		return
	}

	nodeSvc := h.engine.NodeService()

	type nodeRow struct {
		NodeName     string `json:"node_name"`
		GroupName    string `json:"group_name"`
		Occupied     bool   `json:"occupied"`
		PayloadCode  string `json:"payload_code"`
		UOPRemaining int    `json:"uop_remaining"`
	}
	type payloadRow struct {
		PayloadCode  string `json:"payload_code"`
		Occupied     bool   `json:"occupied"`
		NodeName     string `json:"node_name"`
		GroupName    string `json:"group_name"`
		UOPRemaining int    `json:"uop_remaining"`
	}

	type transitRow struct {
		PayloadCode  string `json:"payload_code"`
		DestNode     string `json:"dest_node"`
		RobotID      string `json:"robot_id,omitempty"`
		UOPRemaining int    `json:"uop_remaining"`
	}

	resp := map[string]any{
		"loader_name": loader.Name,
		"layout":      loader.Layout,
	}

	if loader.Layout == "shared_window" {
		payloads, pErr := svc.Payloads(cfg.LoaderID)
		if pErr != nil {
			h.jsonError(w, pErr.Error(), http.StatusInternalServerError)
			return
		}
		resp["payloads_count"] = len(payloads)
		allBins, bErr := h.engine.BinService().ListBins()
		if bErr != nil {
			h.jsonError(w, bErr.Error(), http.StatusInternalServerError)
			return
		}
		allNodes, _ := nodeSvc.ListNodes()
		nodeParent := make(map[string]string, len(allNodes))
		nodeTypeByName := make(map[string]string, len(allNodes))
		for _, n := range allNodes {
			if n.ParentName != "" {
				nodeParent[n.Name] = n.ParentName
			}
			nodeTypeByName[n.Name] = n.NodeTypeCode
		}
		binByPayload := make(map[string]*domain.Bin, len(allBins))
		for i := range allBins {
			b := allBins[i]
			if b.PayloadCode != "" && b.Status != "retired" {
				if nodeTypeByName[b.NodeName] != protocol.NodeClassSTOR {
					continue
				}
				if _, exists := binByPayload[b.PayloadCode]; !exists {
					binByPayload[b.PayloadCode] = b
				}
			}
		}
		rows := make([]payloadRow, 0, len(payloads))
		for _, p := range payloads {
			row := payloadRow{PayloadCode: p.PayloadCode}
			if b, ok := binByPayload[p.PayloadCode]; ok {
				row.Occupied = true
				row.UOPRemaining = b.UOPRemaining
				row.NodeName = b.NodeName
				row.GroupName = nodeParent[b.NodeName]
			}
			rows = append(rows, row)
		}
		resp["rows"] = rows
		activeOrders, _ := h.engine.OrderService().ListActiveOrders()
		orderDest := make(map[int64]string, len(activeOrders))
		orderRobot := make(map[int64]string, len(activeOrders))
		for _, o := range activeOrders {
			if o.BinID != nil && o.DeliveryNode != "" {
				orderDest[*o.BinID] = o.DeliveryNode
				orderRobot[*o.BinID] = o.RobotID
			}
		}
		payloadSet := make(map[string]bool, len(payloads))
		for _, p := range payloads {
			payloadSet[p.PayloadCode] = true
		}
		transit := make([]transitRow, 0)
		for _, b := range allBins {
			if b.PayloadCode == "" || b.Status == "retired" {
				continue
			}
			if !payloadSet[b.PayloadCode] {
				continue
			}
			if b.NodeName != domain.TransitNodeName {
				continue
			}
			dest := ""
			robot := ""
			if b.ClaimedBy != nil {
				dest = orderDest[*b.ClaimedBy]
				robot = orderRobot[*b.ClaimedBy]
			}
			transit = append(transit, transitRow{
				PayloadCode:  b.PayloadCode,
				DestNode:     dest,
				RobotID:      robot,
				UOPRemaining: b.UOPRemaining,
			})
		}
		resp["transit"] = transit
	} else {
		homes, err := svc.Homes(cfg.LoaderID)
		if err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows := make([]nodeRow, 0, len(homes))
		for _, hm := range homes {
			node, nErr := nodeSvc.GetNode(hm.PositionNodeID)
			if nErr != nil || node == nil {
				continue
			}
			if node.NodeTypeCode != protocol.NodeClassSTOR {
				continue
			}
			row := nodeRow{NodeName: node.Name}
			if node.ParentName != "" {
				row.GroupName = node.ParentName
			}
			bins, bErr := nodeSvc.ListBinsByNode(hm.PositionNodeID)
			if bErr == nil {
				for _, b := range bins {
					if b.PayloadCode != "" && b.Status != "retired" {
						row.Occupied = true
						row.PayloadCode = b.PayloadCode
						row.UOPRemaining = b.UOPRemaining
						break
					}
				}
			}
			if row.PayloadCode == "" && hm.PayloadCode != "" {
				row.PayloadCode = hm.PayloadCode
			}
			rows = append(rows, row)
		}
		resp["homes_count"] = len(homes)
		resp["rows"] = rows
		allBins, bErr := h.engine.BinService().ListBins()
		if bErr == nil {
			activeOrders, _ := h.engine.OrderService().ListActiveOrders()
			orderDest := make(map[int64]string, len(activeOrders))
			orderRobot := make(map[int64]string, len(activeOrders))
			for _, o := range activeOrders {
				if o.BinID != nil && o.DeliveryNode != "" {
					orderDest[*o.BinID] = o.DeliveryNode
					orderRobot[*o.BinID] = o.RobotID
				}
			}
			transit := make([]transitRow, 0)
			for _, b := range allBins {
				if b.PayloadCode == "" || b.Status == "retired" {
					continue
				}
				if b.NodeName != domain.TransitNodeName {
					continue
				}
				dest := ""
				robot := ""
				if b.ClaimedBy != nil {
					dest = orderDest[*b.ClaimedBy]
					robot = orderRobot[*b.ClaimedBy]
				}
				transit = append(transit, transitRow{
					PayloadCode:  b.PayloadCode,
					DestNode:     dest,
					RobotID:      robot,
					UOPRemaining: b.UOPRemaining,
				})
			}
			resp["transit"] = transit
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	h.jsonOK(w, resp)
}
