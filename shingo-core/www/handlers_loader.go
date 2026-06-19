package www

import "net/http"

// Loader-config write API (loader refactor: Core authors the bin_loaders
// aggregate). Mirrors handlers_nodegroup.go — parse → LoaderService call →
// jsonOK. Handlers stay store-free (depguard): they pass primitives to the
// service, which constructs the aggregate rows, re-derives demand_registry, and
// nudges the threshold monitor; the Edge re-pulls LoaderInfos on its next
// node-list sync.

// apiCreateLoader creates a loader (shared_window/auto default in the service).
func (h *Handlers) apiCreateLoader(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string `json:"name"`
		Role          string `json:"role"`
		Layout        string `json:"layout"`
		Replenishment string `json:"replenishment"`
		OutboundDest  string `json:"outbound_dest"`
		InboundSource string `json:"inbound_source"`
		BufferDest    string `json:"buffer_dest"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.Name == "" || req.Role == "" {
		h.jsonError(w, "name and role are required", http.StatusBadRequest)
		return
	}
	id, err := h.engine.LoaderService().Create(req.Name, req.Role, req.Layout,
		req.Replenishment, req.OutboundDest, req.InboundSource, req.BufferDest)
	if err != nil {
		h.jsonError(w, "create loader: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]any{"id": id, "name": req.Name})
}

// apiUpdateLoader edits a loader's mutable fields (name + shared_window flow
// endpoints). role + core_node are the identity and are not editable here.
func (h *Handlers) apiUpdateLoader(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID            int64  `json:"id"`
		Name          string `json:"name"`
		Layout        string `json:"layout"`
		Replenishment string `json:"replenishment"`
		OutboundDest  string `json:"outbound_dest"`
		InboundSource string `json:"inbound_source"`
		BufferDest    string `json:"buffer_dest"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.ID == 0 || req.Name == "" {
		h.jsonError(w, "id and name are required", http.StatusBadRequest)
		return
	}
	if err := h.engine.LoaderService().Update(req.ID, req.Name, req.Layout, req.Replenishment,
		req.OutboundDest, req.InboundSource, req.BufferDest); err != nil {
		h.jsonError(w, "update loader: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiSetLoaderPayload assigns/updates a shared_window payload binding + threshold.
func (h *Handlers) apiSetLoaderPayload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoaderID     int64  `json:"loader_id"`
		PayloadCode  string `json:"payload_code"`
		UOPThreshold int    `json:"uop_threshold"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.LoaderID == 0 || req.PayloadCode == "" {
		h.jsonError(w, "loader_id and payload_code are required", http.StatusBadRequest)
		return
	}
	if err := h.engine.LoaderService().SetPayload(req.LoaderID, req.PayloadCode, req.UOPThreshold); err != nil {
		h.jsonError(w, "set payload: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiSetLoaderHome assigns/updates a dedicated position (one payload per node) + threshold.
func (h *Handlers) apiSetLoaderHome(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoaderID       int64  `json:"loader_id"`
		PositionNodeID int64  `json:"position_node_id"`
		PayloadCode    string `json:"payload_code"`
		HomeKind       string `json:"home_kind"` // "" / home / buffer (zone the tile dropped into)
		UOPThreshold   int    `json:"uop_threshold"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	// payload_code is optional: the grid-drag adds the position first (empty
	// payload) and the operator assigns its payload afterward via the picker.
	// home_kind is optional too: absent → home (the store normalises it).
	if req.LoaderID == 0 || req.PositionNodeID == 0 {
		h.jsonError(w, "loader_id and position_node_id are required", http.StatusBadRequest)
		return
	}
	if err := h.engine.LoaderService().SetHome(req.LoaderID, req.PositionNodeID, req.PayloadCode, req.HomeKind, req.UOPThreshold); err != nil {
		h.jsonError(w, "set home: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiRemoveLoaderHome clears a dedicated position (grid-drag "×" on a member).
func (h *Handlers) apiRemoveLoaderHome(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoaderID       int64 `json:"loader_id"`
		PositionNodeID int64 `json:"position_node_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.LoaderID == 0 || req.PositionNodeID == 0 {
		h.jsonError(w, "loader_id and position_node_id are required", http.StatusBadRequest)
		return
	}
	if err := h.engine.LoaderService().RemoveHome(req.LoaderID, req.PositionNodeID); err != nil {
		h.jsonError(w, "remove home: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiReorderLoaderHomes rewrites the dedicated-position order (grid-drag reorder).
func (h *Handlers) apiReorderLoaderHomes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoaderID   int64   `json:"loader_id"`
		OrderedIDs []int64 `json:"ordered_ids"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.LoaderID == 0 {
		h.jsonError(w, "loader_id is required", http.StatusBadRequest)
		return
	}
	if err := h.engine.LoaderService().ReorderHomes(req.LoaderID, req.OrderedIDs); err != nil {
		h.jsonError(w, "reorder homes: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiRemoveLoaderPayload drops a shared_window payload binding (chip "×").
func (h *Handlers) apiRemoveLoaderPayload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoaderID    int64  `json:"loader_id"`
		PayloadCode string `json:"payload_code"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.LoaderID == 0 || req.PayloadCode == "" {
		h.jsonError(w, "loader_id and payload_code are required", http.StatusBadRequest)
		return
	}
	if err := h.engine.LoaderService().RemovePayload(req.LoaderID, req.PayloadCode); err != nil {
		h.jsonError(w, "remove payload: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiDeleteLoader removes a loader and its bindings.
func (h *Handlers) apiDeleteLoader(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.ID == 0 {
		h.jsonError(w, "id is required", http.StatusBadRequest)
		return
	}
	if err := h.engine.LoaderService().Delete(req.ID); err != nil {
		h.jsonError(w, "delete loader: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiListLoaders returns every loader with its payloads + homes (the Nodes-page
// Create-Loader panel + the Inventory threshold view). Uses only service return
// values, naming no store types, so the handler stays store-free.
func (h *Handlers) apiListLoaders(w http.ResponseWriter, _ *http.Request) {
	svc := h.engine.LoaderService()
	ls, err := svc.List()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(ls))
	for _, l := range ls {
		payloads, _ := svc.Payloads(l.ID)
		homes, _ := svc.Homes(l.ID)
		out = append(out, map[string]any{"loader": l, "payloads": payloads, "homes": homes})
	}
	h.jsonOK(w, map[string]any{"loaders": out})
}

// apiCalculateThreshold runs the ported lead-time calculator over a lookback
// window and returns suggested thresholds + a confidence score, for the loader
// panel's Calculate button.
func (h *Handlers) apiCalculateThreshold(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CoreNodeName   string  `json:"core_node_name"`
		PayloadCode    string  `json:"payload_code"`
		Days           int     `json:"days"`
		SafetyFactor   float64 `json:"safety_factor"`
		CycleSeconds   float64 `json:"cycle_seconds"`
		BinCapacityUOP int     `json:"bin_capacity_uop"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.PayloadCode == "" {
		h.jsonError(w, "payload_code is required", http.StatusBadRequest)
		return
	}
	res, err := h.engine.CalculatorService().CalculateDays(req.CoreNodeName, req.PayloadCode,
		req.Days, req.SafetyFactor, req.CycleSeconds, req.BinCapacityUOP)
	if err != nil {
		h.jsonError(w, "calculate: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, res)
}
