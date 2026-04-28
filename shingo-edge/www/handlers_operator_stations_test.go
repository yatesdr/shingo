package www

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingoedge/store"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// ═══════════════════════════════════════════════════════════════════════
// Test router — mirrors the routes from router.go that bind to
// handlers_operator_stations.go. Routes unrelated to this file are
// deliberately omitted. Tests use this router exclusively.
// ═══════════════════════════════════════════════════════════════════════

func newOperatorStationsRouter(t *testing.T) (*Handlers, *chi.Mux) {
	t.Helper()
	h, r := newTestHandlers(t)

	// Public page handler (shop floor HMI). Template rendering would panic on
	// a nil tmpl field, so tests only exercise its early-return error paths.
	r.Get("/operator/station/{id}", h.handleOperatorStationDisplay)

	r.Route("/api", func(r chi.Router) {
		// ── Public API (shop floor) ────────────────────────
		r.Get("/operator-stations/{id}/view", h.apiGetOperatorStationView)

		r.Post("/process-nodes/{id}/request", h.apiRequestNodeMaterial)
		r.Post("/process-nodes/{id}/release-empty", h.apiReleaseNodeEmpty)
		r.Post("/process-nodes/{id}/release-partial", h.apiReleaseNodePartial)
		r.Post("/process-nodes/{id}/release-staged", h.apiReleaseNodeStagedOrders)
		r.Post("/process-nodes/{id}/manifest/confirm", h.apiConfirmNodeManifest)
		r.Post("/process-nodes/{id}/finalize", h.apiFinalizeProduceNode)
		r.Post("/process-nodes/{id}/load-bin", h.apiLoadBin)
		r.Post("/process-nodes/{id}/clear-bin", h.apiClearBin)
		r.Post("/process-nodes/{id}/request-empty", h.apiRequestEmptyBin)
		r.Post("/process-nodes/{id}/request-full", h.apiRequestFullBin)
		r.Post("/process-nodes/{id}/clear-orders", h.apiClearNodeOrders)
		r.Post("/process-nodes/{id}/flip-ab", h.apiFlipABNode)

		r.Post("/processes/{id}/changeover/start", h.apiStartProcessChangeover)
		r.Post("/processes/{id}/changeover/cutover", h.apiCompleteProcessProductionCutover)
		r.Post("/processes/{id}/changeover/cancel", h.apiCancelProcessChangeover)
		r.Post("/processes/{id}/changeover/release-wait", h.apiReleaseChangeoverWait)
		r.Post("/processes/{id}/changeover/stage-node/{nodeID}", h.apiStageNodeChangeoverMaterial)
		r.Post("/processes/{id}/changeover/empty-node/{nodeID}", h.apiEmptyNodeForToolChange)
		r.Post("/processes/{id}/changeover/release-node/{nodeID}", h.apiReleaseNodeIntoProduction)
		r.Post("/processes/{id}/changeover/switch-station/{stationID}", h.apiSwitchOperatorStationToTarget)
		r.Post("/processes/{id}/changeover/switch-node/{nodeID}", h.apiSwitchNodeToTarget)

		r.Get("/orders/active", h.apiGetActiveOrders)

		// ── Admin API ──────────────────────────────────────
		r.Group(func(r chi.Router) {
			r.Use(h.adminMiddleware)

			r.Get("/operator-stations", h.apiListOperatorStations)
			r.Post("/operator-stations", h.apiCreateOperatorStation)
			r.Put("/operator-stations/{id}", h.apiUpdateOperatorStation)
			r.Post("/operator-stations/{id}/move", h.apiMoveOperatorStation)
			r.Delete("/operator-stations/{id}", h.apiDeleteOperatorStation)
			r.Get("/operator-stations/{id}/claimed-nodes", h.apiGetStationClaimedNodes)
			r.Put("/operator-stations/{id}/claimed-nodes", h.apiSetStationClaimedNodes)

			r.Get("/process-nodes", h.apiListConfiguredProcessNodes)
			r.Get("/process-nodes/station/{stationID}", h.apiListConfiguredProcessNodesByStation)
			r.Post("/process-nodes", h.apiCreateProcessNode)
			r.Put("/process-nodes/{id}", h.apiUpdateProcessNode)
			r.Delete("/process-nodes/{id}", h.apiDeleteProcessNode)
		})
	})

	return h, r
}

// ═══════════════════════════════════════════════════════════════════════
// Operator Stations — List, Create, Update, Delete, Move
// DB call sites: ListOperatorStations, CreateOperatorStation,
//                UpdateOperatorStation, DeleteOperatorStation,
//                MoveOperatorStation (+ GetOperatorStation internally)
// ═══════════════════════════════════════════════════════════════════════

func TestOperatorStations_ListStations_Success(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "ListStationsLine")
	seedOperatorStation(t, pid, "OS-LS-1", "ListStation-Alpha")
	seedOperatorStation(t, pid, "OS-LS-2", "ListStation-Bravo")

	resp := doRequest(t, router, "GET", "/api/operator-stations", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var stations []stations.Station
	decodeJSON(t, resp, &stations)

	codes := make(map[string]bool)
	for _, s := range stations {
		codes[s.Code] = true
	}
	if !codes["OS-LS-1"] || !codes["OS-LS-2"] {
		t.Errorf("expected OS-LS-1 and OS-LS-2 in list, got codes=%v", codes)
	}
}

func TestOperatorStations_CreateStation_Success(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "CreateStationLine")
	body := stations.Input{
		ProcessID:  pid,
		Code:       "OS-CREATE-1",
		Name:       "CreatedStation",
		AreaLabel:  "Zone A",
		Sequence:   1,
		DeviceMode: "fixed_hmi",
		Enabled:    true,
	}
	resp := doRequest(t, router, "POST", "/api/operator-stations", body, cookie)
	assertStatus(t, resp, http.StatusOK)

	var out map[string]int64
	decodeJSON(t, resp, &out)
	stationID := out["id"]
	if stationID == 0 {
		t.Fatal("expected non-zero station id")
	}

	// Verify DB state
	station, err := testDB.GetOperatorStation(stationID)
	if err != nil {
		t.Fatalf("GetOperatorStation: %v", err)
	}
	if station.Code != "OS-CREATE-1" {
		t.Errorf("station code: got %q, want %q", station.Code, "OS-CREATE-1")
	}
	if station.Name != "CreatedStation" {
		t.Errorf("station name: got %q, want %q", station.Name, "CreatedStation")
	}
	if station.ProcessID != pid {
		t.Errorf("station process_id: got %d, want %d", station.ProcessID, pid)
	}
}

func TestOperatorStations_CreateStation_InvalidJSON(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	// Syntactically valid JSON, but process_id is the wrong type so it
	// fails to decode into OperatorStationInput{ProcessID int64}.
	body := map[string]interface{}{"process_id": "not-a-number"}
	resp := doRequest(t, router, "POST", "/api/operator-stations", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestOperatorStations_UpdateStation_Success(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "UpdateStationLine")
	sid := seedOperatorStation(t, pid, "OS-UPD-1", "OriginalName")

	body := stations.Input{
		ProcessID: pid,
		Code:      "OS-UPD-1",
		Name:      "UpdatedName",
		AreaLabel: "Zone-B",
		Sequence:  2,
		Enabled:   true,
	}
	resp := doRequest(t, router, "PUT", "/api/operator-stations/"+itoa(sid), body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB state
	station, err := testDB.GetOperatorStation(sid)
	if err != nil {
		t.Fatalf("GetOperatorStation: %v", err)
	}
	if station.Name != "UpdatedName" {
		t.Errorf("station name after update: got %q, want %q", station.Name, "UpdatedName")
	}
	if station.AreaLabel != "Zone-B" {
		t.Errorf("station area_label: got %q, want %q", station.AreaLabel, "Zone-B")
	}
}

func TestOperatorStations_UpdateStation_InvalidID(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	body := stations.Input{Name: "x"}
	resp := doRequest(t, router, "PUT", "/api/operator-stations/notanumber", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid id")
}

func TestOperatorStations_DeleteStation_Success(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "DeleteStationLine")
	sid := seedOperatorStation(t, pid, "OS-DEL-1", "DoomedStation")

	resp := doRequest(t, router, "DELETE", "/api/operator-stations/"+itoa(sid), nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB state: station is gone
	if _, err := testDB.GetOperatorStation(sid); err == nil {
		t.Error("expected error getting deleted station")
	}
}

func TestOperatorStations_MoveStation_Up(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "MoveStationLine")
	first := seedOperatorStation(t, pid, "OS-MV-1", "First")  // sequence=1
	second := seedOperatorStation(t, pid, "OS-MV-2", "Second") // sequence=2

	body := map[string]string{"direction": "up"}
	resp := doRequest(t, router, "POST", "/api/operator-stations/"+itoa(second)+"/move", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB state: sequences swapped
	firstStation, err := testDB.GetOperatorStation(first)
	if err != nil {
		t.Fatalf("GetOperatorStation(first): %v", err)
	}
	secondStation, err := testDB.GetOperatorStation(second)
	if err != nil {
		t.Fatalf("GetOperatorStation(second): %v", err)
	}
	if secondStation.Sequence >= firstStation.Sequence {
		t.Errorf("after move-up: second.sequence=%d, first.sequence=%d (expected second<first)",
			secondStation.Sequence, firstStation.Sequence)
	}
}

func TestOperatorStations_MoveStation_InvalidDirection(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "MoveBadDirLine")
	sid := seedOperatorStation(t, pid, "OS-MVD-1", "Single")

	body := map[string]string{"direction": "sideways"}
	resp := doRequest(t, router, "POST", "/api/operator-stations/"+itoa(sid)+"/move", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "direction must be up or down")
}

func TestOperatorStations_MoveStation_InvalidID(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	resp := doRequest(t, router, "POST", "/api/operator-stations/bad/move", map[string]string{"direction": "up"}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid id")
}

// ═══════════════════════════════════════════════════════════════════════
// Operator Station View — GET /api/operator-stations/{id}/view
// DB call sites: BuildOperatorStationView (→ GetOperatorStation etc),
//                TouchOperatorStation
// ═══════════════════════════════════════════════════════════════════════

func TestOperatorStations_GetOperatorStationView_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	pid := seedProcess(t, "ViewLine")
	sid := seedOperatorStation(t, pid, "OS-VIEW-1", "ViewStation")

	resp := doRequest(t, router, "GET", "/api/operator-stations/"+itoa(sid)+"/view", nil, nil)
	assertStatus(t, resp, http.StatusOK)

	var view store.OperatorStationView
	decodeJSON(t, resp, &view)
	if view.Station.ID != sid {
		t.Errorf("view station id: got %d, want %d", view.Station.ID, sid)
	}
	if view.Station.Code != "OS-VIEW-1" {
		t.Errorf("view station code: got %q, want %q", view.Station.Code, "OS-VIEW-1")
	}
	if view.Process.ID != pid {
		t.Errorf("view process id: got %d, want %d", view.Process.ID, pid)
	}

	// Verify TouchOperatorStation DB side effect
	station, err := testDB.GetOperatorStation(sid)
	if err != nil {
		t.Fatalf("GetOperatorStation after view: %v", err)
	}
	if station.HealthStatus != "online" {
		t.Errorf("station health_status after view: got %q, want %q",
			station.HealthStatus, "online")
	}
	if station.LastSeenAt == nil {
		t.Error("expected last_seen_at to be set after view")
	}
}

func TestOperatorStations_GetOperatorStationView_InvalidID(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "GET", "/api/operator-stations/bad/view", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid station id")
}

func TestOperatorStations_GetOperatorStationView_NotFound(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "GET", "/api/operator-stations/999999/view", nil, nil)
	assertStatus(t, resp, http.StatusNotFound)
	assertJSONPath(t, resp, "error", "station not found")
}

// ═══════════════════════════════════════════════════════════════════════
// Operator Display page — GET /operator/station/{id}
// Only error paths are covered (success path renders a template, which
// is not wired in the test harness).
// ═══════════════════════════════════════════════════════════════════════

func TestOperatorStations_Display_InvalidID(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "GET", "/operator/station/bad", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestOperatorStations_Display_NotFound(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "GET", "/operator/station/999999", nil, nil)
	assertStatus(t, resp, http.StatusNotFound)
}

// ═══════════════════════════════════════════════════════════════════════
// Active Orders — GET /api/orders/active
// DB call site: ListActiveOrders
// ═══════════════════════════════════════════════════════════════════════

func TestOperatorStations_ListActiveOrders_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "GET", "/api/orders/active", nil, nil)
	assertStatus(t, resp, http.StatusOK)

	// Body must decode as a JSON array (possibly empty/null).
	var orders []orders.Order
	decodeJSON(t, resp, &orders)
}

// ═══════════════════════════════════════════════════════════════════════
// Station claimed nodes — Get, Set
// DB call sites: GetStationNodeNames, SetStationNodes
// ═══════════════════════════════════════════════════════════════════════

func TestOperatorStations_GetStationClaimedNodes_Success(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "ClaimedNodesLine")
	sid := seedOperatorStation(t, pid, "OS-CN-1", "ClaimedNodesStation")
	seedProcessNode(t, pid, sid, "core-node-alpha")
	seedProcessNode(t, pid, sid, "core-node-bravo")

	resp := doRequest(t, router, "GET", "/api/operator-stations/"+itoa(sid)+"/claimed-nodes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var names []string
	decodeJSON(t, resp, &names)
	if len(names) != 2 {
		t.Fatalf("expected 2 node names, got %d (%v)", len(names), names)
	}
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if !found["core-node-alpha"] || !found["core-node-bravo"] {
		t.Errorf("expected core-node-alpha and core-node-bravo in %v", names)
	}
}

func TestOperatorStations_SetStationClaimedNodes_Success(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "SetClaimedNodesLine")
	sid := seedOperatorStation(t, pid, "OS-SCN-1", "SetClaimedStation")

	body := map[string]interface{}{
		"nodes": []string{"set-node-a", "set-node-b", "set-node-c"},
	}
	resp := doRequest(t, router, "PUT", "/api/operator-stations/"+itoa(sid)+"/claimed-nodes", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB state: three process nodes created
	names, err := testDB.GetStationNodeNames(sid)
	if err != nil {
		t.Fatalf("GetStationNodeNames: %v", err)
	}
	if len(names) != 3 {
		t.Errorf("expected 3 nodes after set, got %d (%v)", len(names), names)
	}
}

func TestOperatorStations_SetStationClaimedNodes_InvalidID(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	body := map[string]interface{}{"nodes": []string{"x"}}
	resp := doRequest(t, router, "PUT", "/api/operator-stations/bad/claimed-nodes", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid station id")
}

// ═══════════════════════════════════════════════════════════════════════
// Process Nodes — List, Create, Update, Delete, Clear Orders
// DB call sites: ListProcessNodes, ListProcessNodesByStation,
//                CreateProcessNode, EnsureProcessNodeRuntime,
//                UpdateProcessNode, DeleteProcessNode,
//                UpdateProcessNodeRuntimeOrders
// ═══════════════════════════════════════════════════════════════════════

func TestOperatorStations_ListProcessNodes_Success(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "ListPNLine")
	sid := seedOperatorStation(t, pid, "OS-LPN-1", "ListPNStation")
	nodeID := seedProcessNode(t, pid, sid, "pn-list-alpha")

	resp := doRequest(t, router, "GET", "/api/process-nodes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var nodes []processes.Node
	decodeJSON(t, resp, &nodes)
	found := false
	for _, n := range nodes {
		if n.ID == nodeID && n.CoreNodeName == "pn-list-alpha" {
			found = true
		}
	}
	if !found {
		t.Errorf("seeded process node (id=%d, core-name=pn-list-alpha) not found in list", nodeID)
	}
}

func TestOperatorStations_ListProcessNodesByStation_Success(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "ListPNByStationLine")
	sid := seedOperatorStation(t, pid, "OS-LPNB-1", "ListPNByStation")
	seedProcessNode(t, pid, sid, "pn-bystation-alpha")
	seedProcessNode(t, pid, sid, "pn-bystation-bravo")

	resp := doRequest(t, router, "GET", "/api/process-nodes/station/"+itoa(sid), nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var nodes []processes.Node
	decodeJSON(t, resp, &nodes)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes for station %d, got %d", sid, len(nodes))
	}
}

func TestOperatorStations_ListProcessNodesByStation_InvalidID(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	resp := doRequest(t, router, "GET", "/api/process-nodes/station/bad", nil, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid station id")
}

func TestOperatorStations_CreateProcessNode_Success(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "CreatePNLine")
	sid := seedOperatorStation(t, pid, "OS-CPN-1", "CreatePNStation")

	body := processes.NodeInput{
		ProcessID:         pid,
		OperatorStationID: &sid,
		CoreNodeName:      "pn-create-alpha",
		Name:              "CreatedNode",
		Enabled:           true,
	}
	resp := doRequest(t, router, "POST", "/api/process-nodes", body, cookie)
	assertStatus(t, resp, http.StatusOK)

	var out map[string]int64
	decodeJSON(t, resp, &out)
	nodeID := out["id"]
	if nodeID == 0 {
		t.Fatal("expected non-zero process node id")
	}

	// Verify DB state: node exists
	node, err := testDB.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("GetProcessNode: %v", err)
	}
	if node.CoreNodeName != "pn-create-alpha" {
		t.Errorf("core_node_name: got %q, want %q", node.CoreNodeName, "pn-create-alpha")
	}

	// Verify DB state: runtime row was ensured
	rt, err := testDB.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("GetProcessNodeRuntime after create: %v", err)
	}
	if rt.ProcessNodeID != nodeID {
		t.Errorf("runtime process_node_id: got %d, want %d", rt.ProcessNodeID, nodeID)
	}
}

func TestOperatorStations_CreateProcessNode_InvalidJSON(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	// process_id must be an integer; sending a string triggers a decode error.
	body := map[string]interface{}{"process_id": "nope"}
	resp := doRequest(t, router, "POST", "/api/process-nodes", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestOperatorStations_UpdateProcessNode_Success(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "UpdatePNLine")
	sid := seedOperatorStation(t, pid, "OS-UPN-1", "UpdatePNStation")
	nodeID := seedProcessNode(t, pid, sid, "pn-upd-orig")

	body := processes.NodeInput{
		ProcessID:         pid,
		OperatorStationID: &sid,
		CoreNodeName:      "pn-upd-new",
		Name:              "RenamedNode",
		Enabled:           false,
	}
	resp := doRequest(t, router, "PUT", "/api/process-nodes/"+itoa(nodeID), body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB state
	node, err := testDB.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("GetProcessNode: %v", err)
	}
	if node.CoreNodeName != "pn-upd-new" {
		t.Errorf("core_node_name after update: got %q, want %q", node.CoreNodeName, "pn-upd-new")
	}
	if node.Name != "RenamedNode" {
		t.Errorf("name after update: got %q, want %q", node.Name, "RenamedNode")
	}
	if node.Enabled {
		t.Error("expected enabled=false after update")
	}
}

func TestOperatorStations_UpdateProcessNode_InvalidID(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	resp := doRequest(t, router, "PUT", "/api/process-nodes/bad",
		processes.NodeInput{Name: "x"}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid id")
}

func TestOperatorStations_DeleteProcessNode_Success(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "DeletePNLine")
	sid := seedOperatorStation(t, pid, "OS-DPN-1", "DeletePNStation")
	nodeID := seedProcessNode(t, pid, sid, "pn-delete-me")

	resp := doRequest(t, router, "DELETE", "/api/process-nodes/"+itoa(nodeID), nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB state
	if _, err := testDB.GetProcessNode(nodeID); err == nil {
		t.Error("expected error getting deleted process node")
	}
}

func TestOperatorStations_ClearNodeOrders_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	pid := seedProcess(t, "ClearOrdersLine")
	sid := seedOperatorStation(t, pid, "OS-CO-1", "ClearOrdersStation")
	nodeID := seedProcessNode(t, pid, sid, "pn-clear-orders")

	// Seed a runtime row with a stub active order id, then clear it.
	if _, err := testDB.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("EnsureProcessNodeRuntime: %v", err)
	}
	stubOrder := int64(42)
	if err := testDB.UpdateProcessNodeRuntimeOrders(nodeID, &stubOrder, nil); err != nil {
		t.Fatalf("seed active order: %v", err)
	}

	resp := doRequest(t, router, "POST", "/api/process-nodes/"+itoa(nodeID)+"/clear-orders", nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB state: active_order_id is cleared
	rt, err := testDB.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("GetProcessNodeRuntime: %v", err)
	}
	if rt.ActiveOrderID != nil {
		t.Errorf("expected active_order_id=nil after clear, got %v", *rt.ActiveOrderID)
	}
}

func TestOperatorStations_ClearNodeOrders_InvalidID(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/process-nodes/bad/clear-orders", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid node id")
}

// ═══════════════════════════════════════════════════════════════════════
// Engine passthrough — process node operations
// These handlers delegate to the engine (stubbed to return nil/no error).
// We cover the happy path (parseID → engine call → writeJSON) and the
// parseID error path.
// ═══════════════════════════════════════════════════════════════════════

func TestOperatorStations_RequestNodeMaterial_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/process-nodes/1/request", nil, nil)
	assertStatus(t, resp, http.StatusOK)
}

func TestOperatorStations_RequestNodeMaterial_InvalidID(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/process-nodes/bad/request", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid node id")
}

func TestOperatorStations_ReleaseNodeEmpty_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/process-nodes/1/release-empty", nil, nil)
	assertStatus(t, resp, http.StatusOK)
}

func TestOperatorStations_ReleaseNodePartial_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	body := map[string]int64{"qty": 5}
	resp := doRequest(t, router, "POST", "/api/process-nodes/1/release-partial", body, nil)
	assertStatus(t, resp, http.StatusOK)
}

func TestOperatorStations_ReleaseNodePartial_InvalidJSON(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	// qty must be int64; sending a string triggers a decode error.
	body := map[string]interface{}{"qty": "nope"}
	resp := doRequest(t, router, "POST", "/api/process-nodes/1/release-partial", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestOperatorStations_ReleaseStagedOrders_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	body := map[string]interface{}{"called_by": "test-station"}
	resp := doRequest(t, router, "POST", "/api/process-nodes/1/release-staged", body, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestOperatorStations_ReleaseStagedOrders_InvalidID(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	body := map[string]interface{}{"called_by": "test-station"}
	resp := doRequest(t, router, "POST", "/api/process-nodes/bad/release-staged", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid node id")
}

// Phase 7 (lineside): the release-staged endpoint accepts qty_by_part in the
// body so the two-robot HMI button can forward lineside captures like the
// single-order path does. A well-formed body should decode and succeed.
func TestOperatorStations_ReleaseStagedOrders_AcceptsQtyByPart(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	body := map[string]interface{}{
		"qty_by_part": map[string]int{"PART-A": 5, "PART-B": 2},
		"called_by":   "test-station",
	}
	resp := doRequest(t, router, "POST", "/api/process-nodes/1/release-staged", body, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

// A malformed body is rejected with a 400 — the handler defends against bad
// JSON rather than silently falling through to a no-op release.
func TestOperatorStations_ReleaseStagedOrders_BadBody(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	// qty_by_part must be a map; sending an int triggers a decode error.
	body := map[string]interface{}{"qty_by_part": 123, "called_by": "test-station"}
	resp := doRequest(t, router, "POST", "/api/process-nodes/1/release-staged", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

// Post-2026-04-27 guard: a release-staged POST with no body or empty
// called_by is rejected. Mirrors the same guard on apiReleaseOrder so
// neither release endpoint accepts the disposition-bypass fingerprint.
func TestOperatorStations_ReleaseStagedOrders_RejectsBareBody(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/process-nodes/1/release-staged", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)

	body := map[string]interface{}{"disposition": "capture_lineside", "called_by": ""}
	resp = doRequest(t, router, "POST", "/api/process-nodes/1/release-staged", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestOperatorStations_ConfirmNodeManifest_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/process-nodes/1/manifest/confirm", nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestOperatorStations_FinalizeProduceNode_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/process-nodes/1/finalize", nil, nil)
	assertStatus(t, resp, http.StatusOK)
}

func TestOperatorStations_LoadBin_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	body := map[string]interface{}{
		"payload_code": "BIN-X",
		"uop_count":    10,
	}
	resp := doRequest(t, router, "POST", "/api/process-nodes/1/load-bin", body, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestOperatorStations_LoadBin_InvalidJSON(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	// uop_count must be int64; sending a string triggers a decode error.
	body := map[string]interface{}{"uop_count": "nope"}
	resp := doRequest(t, router, "POST", "/api/process-nodes/1/load-bin", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestOperatorStations_RequestEmptyBin_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	body := map[string]string{"payload_code": "BIN-E"}
	resp := doRequest(t, router, "POST", "/api/process-nodes/1/request-empty", body, nil)
	assertStatus(t, resp, http.StatusOK)
}

func TestOperatorStations_RequestFullBin_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	body := map[string]string{"payload_code": "BIN-F"}
	resp := doRequest(t, router, "POST", "/api/process-nodes/1/request-full", body, nil)
	assertStatus(t, resp, http.StatusOK)
}

func TestOperatorStations_ClearBin_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/process-nodes/1/clear-bin", nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestOperatorStations_FlipABNode_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/process-nodes/1/flip-ab", nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestOperatorStations_FlipABNode_InvalidID(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/process-nodes/bad/flip-ab", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid node id")
}

// ═══════════════════════════════════════════════════════════════════════
// Engine passthrough — changeover lifecycle
// ═══════════════════════════════════════════════════════════════════════

func TestOperatorStations_StartChangeover_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	body := map[string]interface{}{
		"to_style_id": 1,
		"called_by":   "tester",
		"notes":       "unit-test",
	}
	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/start", body, nil)
	assertStatus(t, resp, http.StatusOK)
}

func TestOperatorStations_StartChangeover_InvalidID(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/processes/bad/changeover/start", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid process id")
}

func TestOperatorStations_CancelChangeover_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/cancel", nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestOperatorStations_CancelChangeover_AsRedirect(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	nextStyle := int64(7)
	body := map[string]interface{}{"next_style_id": nextStyle}
	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/cancel", body, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "action", "redirected")
}

func TestOperatorStations_ReleaseChangeoverWait_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	body := map[string]interface{}{"called_by": "test-station"}
	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/release-wait", body, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

// TestOperatorStations_ReleaseChangeoverWait_ThreadsCalledBy verifies that
// the called_by field in the request body flows through to the engine call,
// closing the gap where Phase 8 added body parsing but no test asserted the
// value was actually plumbed (regression coverage for item 15 in the
// release-manifest follow-ups).
func TestOperatorStations_ReleaseChangeoverWait_ThreadsCalledBy(t *testing.T) {
	h, router := newOperatorStationsRouter(t)

	body := map[string]interface{}{"called_by": "stephen-station-7"}
	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/release-wait", body, nil)
	assertStatus(t, resp, http.StatusOK)

	stub := h.engine.(*stubEngine)
	if stub.lastReleaseChangeoverWaitCalledBy != "stephen-station-7" {
		t.Errorf("ReleaseChangeoverWait called_by: got %q, want %q",
			stub.lastReleaseChangeoverWaitCalledBy, "stephen-station-7")
	}
}

// TestOperatorStations_ReleaseChangeoverWait_RejectsBareBody verifies the
// post-2026-04-27 contract on the third release endpoint: a missing body
// or empty called_by produces 400, mirroring apiReleaseOrder and
// apiReleaseNodeStagedOrders. The original c56ceb9 fix added this guard to
// the other two release endpoints; this endpoint had the same shape (empty
// body silently produced an empty audit trail at Core) and was closed
// during the release-flow cleanup.
func TestOperatorStations_ReleaseChangeoverWait_RejectsBareBody(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	// Bare-body POST -> 400.
	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/release-wait", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)

	// Body with empty called_by -> 400.
	body := map[string]interface{}{"called_by": ""}
	resp = doRequest(t, router, "POST", "/api/processes/1/changeover/release-wait", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)

	// Body with whitespace-only called_by -> 400 (TrimSpace check).
	body = map[string]interface{}{"called_by": "  "}
	resp = doRequest(t, router, "POST", "/api/processes/1/changeover/release-wait", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestOperatorStations_CompleteProductionCutover_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/cutover", nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestOperatorStations_StageNodeChangeoverMaterial_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/stage-node/2", nil, nil)
	assertStatus(t, resp, http.StatusOK)
}

func TestOperatorStations_StageNodeChangeoverMaterial_InvalidNodeID(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/stage-node/bad", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid node id")
}

func TestOperatorStations_EmptyNodeForToolChange_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	body := map[string]int64{"qty": 3}
	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/empty-node/2", body, nil)
	assertStatus(t, resp, http.StatusOK)
}

func TestOperatorStations_ReleaseNodeIntoProduction_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/release-node/2", nil, nil)
	assertStatus(t, resp, http.StatusOK)
}

func TestOperatorStations_SwitchNodeToTarget_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/switch-node/2", nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestOperatorStations_SwitchOperatorStationToTarget_Success(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/switch-station/2", nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestOperatorStations_SwitchOperatorStationToTarget_InvalidStationID(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/switch-station/bad", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid station id")
}

// ═══════════════════════════════════════════════════════════════════════
// Auth gating — admin-protected endpoints reject unauthenticated requests
// ═══════════════════════════════════════════════════════════════════════

func TestOperatorStations_AdminAuth_RequiresLogin(t *testing.T) {
	_, router := newOperatorStationsRouter(t)

	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/api/operator-stations"},
		{"POST", "/api/operator-stations"},
		{"PUT", "/api/operator-stations/1"},
		{"DELETE", "/api/operator-stations/1"},
		{"POST", "/api/operator-stations/1/move"},
		{"GET", "/api/operator-stations/1/claimed-nodes"},
		{"PUT", "/api/operator-stations/1/claimed-nodes"},
		{"GET", "/api/process-nodes"},
		{"GET", "/api/process-nodes/station/1"},
		{"POST", "/api/process-nodes"},
		{"PUT", "/api/process-nodes/1"},
		{"DELETE", "/api/process-nodes/1"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+"_"+ep.path, func(t *testing.T) {
			resp := doRequest(t, router, ep.method, ep.path, nil, nil)
			// adminMiddleware returns 303 (browser redirect) or 401 (HTMX).
			if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("unauthenticated %s %s: got %d, want 303 or 401",
					ep.method, ep.path, resp.StatusCode)
			}
		})
	}
}
