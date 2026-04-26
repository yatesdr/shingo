package www

import (
	"net/http"
	"strconv"
	"testing"

	"shingo/protocol"
	"shingoedge/store/catalog"
	"shingoedge/store/counters"
	"shingoedge/store/processes"
)

// ═══════════════════════════════════════════════════════════════════════
// Anomalies — apiConfirmAnomaly, apiDismissAnomaly
// DB call sites: ConfirmAnomaly, DismissAnomaly
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_ConfirmAnomaly(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	// Seed a reporting point + snapshot with anomaly
	styleID := seedStyle(t, "Style-A", seedProcess(t, "Line A"))
	rpID := seedReportingPoint(t, styleID, "plc1", "tag1")
	snapID := seedAnomalySnapshot(t, rpID)

	resp := doRequest(t, router, "POST", "/api/confirm-anomaly/"+itoa(snapID), nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	// Verify DB state: operator_confirmed should be true
	var confirmed bool
	testDB.QueryRow("SELECT operator_confirmed FROM counter_snapshots WHERE id=?", snapID).Scan(&confirmed)
	if !confirmed {
		t.Error("expected operator_confirmed=true after confirm")
	}
}

func TestApiConfig_ConfirmAnomaly_BadID(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	resp := doRequest(t, router, "POST", "/api/confirm-anomaly/notanumber", nil, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid snapshot ID")
}

func TestApiConfig_DismissAnomaly(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	styleID := seedStyle(t, "Style-B", seedProcess(t, "Line B"))
	rpID := seedReportingPoint(t, styleID, "plc2", "tag2")
	snapID := seedAnomalySnapshot(t, rpID)

	resp := doRequest(t, router, "POST", "/api/dismiss-anomaly/"+itoa(snapID), nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	// Verify DB state: row should be deleted
	var count int
	testDB.QueryRow("SELECT COUNT(*) FROM counter_snapshots WHERE id=?", snapID).Scan(&count)
	if count != 0 {
		t.Error("expected snapshot to be deleted after dismiss")
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Processes — List, Create, Update, Delete, SetActiveStyle, ListStylesByProcess
// DB call sites: ListProcesses, CreateProcess, UpdateProcess, DeleteProcess,
//                SetActiveStyle, ListStylesByProcess
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_ListProcesses(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	// Seed two processes
	seedProcess(t, "Assembly")
	seedProcess(t, "Paint")

	resp := doRequest(t, router, "GET", "/api/processes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var procs []processes.Process
	decodeJSON(t, resp, &procs)
	if len(procs) < 2 {
		t.Fatalf("expected at least 2 processes, got %d", len(procs))
	}
	names := make(map[string]bool)
	for _, p := range procs {
		names[p.Name] = true
	}
	if !names["Assembly"] || !names["Paint"] {
		t.Errorf("expected Assembly and Paint in response, got %v", names)
	}
}

func TestApiConfig_CreateProcess(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]interface{}{
		"name":             "TestLine",
		"description":      "A test line",
		"production_state": "active_production",
	}
	resp := doRequest(t, router, "POST", "/api/processes", body, cookie)
	assertStatus(t, resp, http.StatusOK)

	var result map[string]int64
	decodeJSON(t, resp, &result)
	if result["id"] == 0 {
		t.Fatal("expected non-zero process id")
	}

	// Verify DB state
	p, err := testDB.GetProcess(result["id"])
	if err != nil {
		t.Fatalf("GetProcess: %v", err)
	}
	if p.Name != "TestLine" {
		t.Errorf("process name: got %q, want %q", p.Name, "TestLine")
	}
}

func TestApiConfig_CreateProcess_MissingName(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]interface{}{"description": "no name"}
	resp := doRequest(t, router, "POST", "/api/processes", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "name is required")
}

func TestApiConfig_UpdateProcess(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "ToUpdate")

	body := map[string]interface{}{
		"name":        "Updated",
		"description": "updated desc",
	}
	resp := doRequest(t, router, "PUT", "/api/processes/"+itoa(pid), body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB state
	p, err := testDB.GetProcess(pid)
	if err != nil {
		t.Fatalf("GetProcess: %v", err)
	}
	if p.Name != "Updated" {
		t.Errorf("process name: got %q, want %q", p.Name, "Updated")
	}
}

func TestApiConfig_DeleteProcess(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "ToDelete")

	resp := doRequest(t, router, "DELETE", "/api/processes/"+itoa(pid), nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB state
	_, err := testDB.GetProcess(pid)
	if err == nil {
		t.Error("expected error getting deleted process")
	}
}

func TestApiConfig_SetActiveStyle(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "ActiveStyleLine")
	sid := seedStyle(t, "ActiveStyle", pid)

	body := map[string]interface{}{"style_id": sid}
	resp := doRequest(t, router, "PUT", "/api/processes/"+itoa(pid)+"/active-style", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB state
	activeID, err := testDB.GetActiveStyleID(pid)
	if err != nil {
		t.Fatalf("GetActiveStyleID: %v", err)
	}
	if activeID == nil || *activeID != sid {
		t.Errorf("active_style_id: got %v, want %d", activeID, sid)
	}
}

func TestApiConfig_ListProcessStyles(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "StyleListLine")
	seedStyle(t, "S1", pid)
	seedStyle(t, "S2", pid)

	resp := doRequest(t, router, "GET", "/api/processes/"+itoa(pid)+"/styles", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var styles []processes.Style
	decodeJSON(t, resp, &styles)
	if len(styles) != 2 {
		t.Fatalf("expected 2 styles, got %d", len(styles))
	}
	names := make(map[string]bool)
	for _, s := range styles {
		names[s.Name] = true
	}
	if !names["S1"] || !names["S2"] {
		t.Errorf("expected S1 and S2, got %v", names)
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Styles — List, Create, Update, Delete
// DB call sites: ListStyles, CreateStyle, UpdateStyle, DeleteStyle
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_StylesCRUD(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid := seedProcess(t, "StyleCRUDLine")

	// --- List (empty) ---
	resp := doRequest(t, router, "GET", "/api/styles", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	var styles []processes.Style
	decodeJSON(t, resp, &styles)

	// --- Create ---
	body := map[string]interface{}{
		"name":        "Red Widget",
		"description": "A red one",
		"process_id":  pid,
	}
	resp = doRequest(t, router, "POST", "/api/styles", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	var createResult map[string]int64
	decodeJSON(t, resp, &createResult)
	styleID := createResult["id"]
	if styleID == 0 {
		t.Fatal("expected non-zero style id")
	}

	// Verify DB
	s, err := testDB.GetStyle(styleID)
	if err != nil {
		t.Fatalf("GetStyle: %v", err)
	}
	if s.Name != "Red Widget" {
		t.Errorf("style name: got %q, want %q", s.Name, "Red Widget")
	}
	if s.ProcessID != pid {
		t.Errorf("style process_id: got %d, want %d", s.ProcessID, pid)
	}

	// --- List (has one) ---
	resp = doRequest(t, router, "GET", "/api/styles", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	decodeJSON(t, resp, &styles)
	found := false
	for _, s := range styles {
		if s.ID == styleID {
			found = true
		}
	}
	if !found {
		t.Error("created style not found in list")
	}

	// --- Update ---
	updateBody := map[string]interface{}{
		"name":        "Blue Widget",
		"description": "Updated",
		"process_id":  pid,
	}
	resp = doRequest(t, router, "PUT", "/api/styles/"+itoa(styleID), updateBody, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	s, _ = testDB.GetStyle(styleID)
	if s.Name != "Blue Widget" {
		t.Errorf("updated style name: got %q, want %q", s.Name, "Blue Widget")
	}

	// --- Delete ---
	resp = doRequest(t, router, "DELETE", "/api/styles/"+itoa(styleID), nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	_, err = testDB.GetStyle(styleID)
	if err == nil {
		t.Error("expected error getting deleted style")
	}
}

func TestApiConfig_CreateStyle_MissingProcessID(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]interface{}{"name": "Orphan"}
	resp := doRequest(t, router, "POST", "/api/styles", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "process_id is required")
}

func TestApiConfig_UpdateStyle_MissingProcessID(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "StyleUpdateLine")
	sid := seedStyle(t, "NeedsPID", pid)

	body := map[string]interface{}{"name": "X"}
	resp := doRequest(t, router, "PUT", "/api/styles/"+itoa(sid), body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "process_id is required")
}

// ═══════════════════════════════════════════════════════════════════════
// Reporting Points — List, Create, Update, Delete
// DB call sites: ListReportingPoints, CreateReportingPoint,
//                GetReportingPoint, UpdateReportingPoint, DeleteReportingPoint
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_ReportingPointsCRUD(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid := seedProcess(t, "RPLine")
	sid := seedStyle(t, "RPStyle", pid)

	// --- Create ---
	body := map[string]interface{}{
		"plc_name": "plc-rp",
		"tag_name": "tag-rp",
		"style_id": sid,
	}
	resp := doRequest(t, router, "POST", "/api/reporting-points", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	var createResult map[string]int64
	decodeJSON(t, resp, &createResult)
	rpID := createResult["id"]
	if rpID == 0 {
		t.Fatal("expected non-zero reporting point id")
	}

	// Verify DB
	rp, err := testDB.GetReportingPoint(rpID)
	if err != nil {
		t.Fatalf("GetReportingPoint: %v", err)
	}
	if rp.PLCName != "plc-rp" || rp.TagName != "tag-rp" {
		t.Errorf("reporting point: plc=%q tag=%q, want plc=plc-rp tag=tag-rp", rp.PLCName, rp.TagName)
	}

	// --- List ---
	resp = doRequest(t, router, "GET", "/api/reporting-points", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	var rps []counters.ReportingPoint
	decodeJSON(t, resp, &rps)
	found := false
	for _, r := range rps {
		if r.ID == rpID {
			found = true
		}
	}
	if !found {
		t.Error("created reporting point not found in list")
	}

	// --- Update ---
	updateBody := map[string]interface{}{
		"plc_name": "plc-updated",
		"tag_name": "tag-updated",
		"style_id": sid,
		"enabled":  true,
	}
	resp = doRequest(t, router, "PUT", "/api/reporting-points/"+itoa(rpID), updateBody, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	rp, _ = testDB.GetReportingPoint(rpID)
	if rp.PLCName != "plc-updated" {
		t.Errorf("updated plc_name: got %q, want %q", rp.PLCName, "plc-updated")
	}

	// --- Delete ---
	resp = doRequest(t, router, "DELETE", "/api/reporting-points/"+itoa(rpID), nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	_, err = testDB.GetReportingPoint(rpID)
	if err == nil {
		t.Error("expected error getting deleted reporting point")
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Style Node Claims — List, Upsert, Delete
// DB call sites: ListStyleNodeClaims, UpsertStyleNodeClaim,
//                DeleteStyleNodeClaim
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_StyleNodeClaimsCRUD(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid := seedProcess(t, "ClaimLine")
	sid := seedStyle(t, "ClaimStyle", pid)

	// --- Upsert (insert) ---
	body := processes.NodeClaimInput{
		StyleID:       sid,
		CoreNodeName:  "node-1",
		Role:          "consume",
		SwapMode:      "simple",
		PayloadCode:   "BIN-A",
		UOPCapacity:   100,
		ReorderPoint:  10,
		AutoReorder:   true,
	}
	resp := doRequest(t, router, "POST", "/api/style-node-claims", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	var createResult map[string]int64
	decodeJSON(t, resp, &createResult)
	claimID := createResult["id"]
	if claimID == 0 {
		t.Fatal("expected non-zero claim id")
	}

	// Verify DB
	claim, err := testDB.GetStyleNodeClaim(claimID)
	if err != nil {
		t.Fatalf("GetStyleNodeClaim: %v", err)
	}
	if claim.CoreNodeName != "node-1" {
		t.Errorf("claim core_node_name: got %q, want %q", claim.CoreNodeName, "node-1")
	}

	// --- List ---
	resp = doRequest(t, router, "GET", "/api/styles/"+itoa(sid)+"/node-claims", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	var claims []processes.NodeClaim
	decodeJSON(t, resp, &claims)
	if len(claims) != 1 || claims[0].ID != claimID {
		t.Errorf("expected 1 claim with id %d, got %v", claimID, claims)
	}

	// --- Upsert (update same style_id+core_node_name) ---
	updateBody := processes.NodeClaimInput{
		StyleID:       sid,
		CoreNodeName:  "node-1",
		Role:          "consume",
		SwapMode:      "simple",
		PayloadCode:   "BIN-B",
		UOPCapacity:   200,
		ReorderPoint:  20,
		AutoReorder:   true,
	}
	resp = doRequest(t, router, "POST", "/api/style-node-claims", updateBody, cookie)
	assertStatus(t, resp, http.StatusOK)
	var updateResult map[string]int64
	decodeJSON(t, resp, &updateResult)
	if updateResult["id"] != claimID {
		t.Errorf("upsert should return same id: got %d, want %d", updateResult["id"], claimID)
	}

	// Verify updated DB state
	claim, _ = testDB.GetStyleNodeClaim(claimID)
	if claim.PayloadCode != "BIN-B" {
		t.Errorf("updated payload_code: got %q, want %q", claim.PayloadCode, "BIN-B")
	}

	// --- Delete ---
	resp = doRequest(t, router, "DELETE", "/api/style-node-claims/"+itoa(claimID), nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	_, err = testDB.GetStyleNodeClaim(claimID)
	if err == nil {
		t.Error("expected error getting deleted claim")
	}
}

func TestApiConfig_UpsertStyleNodeClaim_MissingStyleID(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := processes.NodeClaimInput{CoreNodeName: "node-x"}
	resp := doRequest(t, router, "POST", "/api/style-node-claims", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "style_id is required")
}

func TestApiConfig_UpsertStyleNodeClaim_MissingCoreNodeName(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	pid := seedProcess(t, "ClaimValidationLine")
	sid := seedStyle(t, "ClaimValStyle", pid)

	body := processes.NodeClaimInput{StyleID: sid}
	resp := doRequest(t, router, "POST", "/api/style-node-claims", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "core_node_name is required")
}

// ═══════════════════════════════════════════════════════════════════════
// Payload Catalog — List
// DB call site: ListPayloadCatalog
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_ListPayloadCatalog(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	// Seed catalog entries
	testDB.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 1, Name: "Bin S", Code: "BIN-S", Description: "Small bin", UOPCapacity: 24,
	})
	testDB.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 2, Name: "Bin L", Code: "BIN-L", Description: "Large bin", UOPCapacity: 48,
	})

	resp := doRequest(t, router, "GET", "/api/payload-catalog", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var entries []*catalog.CatalogEntry
	decodeJSON(t, resp, &entries)
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 catalog entries, got %d", len(entries))
	}
	codes := make(map[string]bool)
	for _, e := range entries {
		codes[e.Code] = true
	}
	if !codes["BIN-S"] || !codes["BIN-L"] {
		t.Errorf("expected BIN-S and BIN-L in catalog, got %v", codes)
	}
}

// ═══════════════════════════════════════════════════════════════════════
// PLC / WarLink — ListPLCs, WarLinkStatus, SyncCoreNodes, SyncPayloadCatalog
// TestCoreAPI (empty URL path), TestKafka (validation)
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_SyncCoreNodes(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	resp := doRequest(t, router, "POST", "/api/core-nodes/sync", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestApiConfig_SyncPayloadCatalog(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	resp := doRequest(t, router, "POST", "/api/payload-catalog/sync", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestApiConfig_TestCoreAPI_EmptyURL(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]string{"core_api": ""}
	resp := doRequest(t, router, "POST", "/api/config/core-api/test", body, cookie)
	assertStatus(t, resp, http.StatusOK)

	var result map[string]interface{}
	decodeJSON(t, resp, &result)
	if result["connected"] != false {
		t.Errorf("empty URL should not be connected, got %v", result["connected"])
	}
}

func TestApiConfig_TestKafka_EmptyBroker(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]string{"broker": ""}
	resp := doRequest(t, router, "POST", "/api/config/kafka/test", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "broker address required")
}

// ═══════════════════════════════════════════════════════════════════════
// Core Nodes — Get
// (no DB call, reads from engine.CoreNodes())
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_GetCoreNodes(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	h.engine.(*stubEngine).core = map[string]protocol.NodeInfo{
		"node-1": {Name: "node-1", NodeType: "storage"},
	}

	resp := doRequest(t, router, "GET", "/api/core-nodes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var nodes []protocol.NodeInfo
	decodeJSON(t, resp, &nodes)
	if len(nodes) != 1 || nodes[0].Name != "node-1" {
		t.Errorf("expected 1 node 'node-1', got %v", nodes)
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Config endpoints — UpdateCoreAPI, UpdateMessaging, UpdateStationID,
// UpdateAutoConfirm, UpdateWarLink
// These call AppConfig() + ConfigPath() + Save()
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_UpdateCoreAPI(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]string{"core_api": "http://core.test:8080"}
	resp := doRequest(t, router, "PUT", "/api/config/core-api", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestApiConfig_UpdateMessaging(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]interface{}{
		"kafka_brokers": []string{"broker1:9092", "broker2:9092"},
	}
	resp := doRequest(t, router, "PUT", "/api/config/messaging", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestApiConfig_UpdateStationID(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]string{"station_id": "plant-a.line-1"}
	resp := doRequest(t, router, "PUT", "/api/config/station-id", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestApiConfig_UpdateAutoConfirm(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]bool{"auto_confirm": true}
	resp := doRequest(t, router, "PUT", "/api/config/auto-confirm", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestApiConfig_UpdateWarLink(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]interface{}{
		"host":      "warlink.test",
		"port":      9090,
		"enabled":   true,
		"poll_rate": "2s",
		"mode":      "sse",
	}
	resp := doRequest(t, router, "PUT", "/api/config/warlink", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestApiConfig_UpdateWarLink_InvalidMode(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]interface{}{"mode": "invalid"}
	resp := doRequest(t, router, "PUT", "/api/config/warlink", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", `mode must be "poll" or "sse"`)
}

func TestApiConfig_UpdateWarLink_InvalidPollRate(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]interface{}{"poll_rate": "not-a-duration"}
	resp := doRequest(t, router, "PUT", "/api/config/warlink", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
}

// ═══════════════════════════════════════════════════════════════════════
// Change Password
// DB call sites: GetAdminUser, UpdateAdminPassword
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_ChangePassword(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]string{
		"old_password": "password",
		"new_password": "newpassword123",
	}
	resp := doRequest(t, router, "POST", "/api/config/password", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB: hash should have changed
	u, err := testDB.GetAdminUser("testadmin")
	if err != nil {
		t.Fatalf("GetAdminUser: %v", err)
	}
	if u.PasswordHash == "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy" {
		t.Error("password hash should have changed after update")
	}
}

func TestApiConfig_ChangePassword_WrongOldPassword(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]string{
		"old_password": "wrongpassword",
		"new_password": "newpassword123",
	}
	resp := doRequest(t, router, "POST", "/api/config/password", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "current password is incorrect")
}

// ═══════════════════════════════════════════════════════════════════════
// Auth gating — admin middleware rejects unauthenticated requests
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_AdminAuth_RequiresLogin(t *testing.T) {
	_, router := newAdminRouter(t)

	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/api/processes"},
		{"GET", "/api/styles"},
		{"GET", "/api/reporting-points"},
		{"GET", "/api/warlink/status"},
		{"PUT", "/api/config/core-api"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+"_"+ep.path, func(t *testing.T) {
			resp := doRequest(t, router, ep.method, ep.path, nil, nil)
			// adminMiddleware redirects (303) or returns 401 for HTMX
			if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusSeeOther {
				t.Errorf("unauthenticated %s %s: got status %d, want 401 or 303", ep.method, ep.path, resp.StatusCode)
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Bad path parameters — parseID failures
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_BadPathParams(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"update_process_bad_id", "PUT", "/api/processes/abc"},
		{"delete_process_bad_id", "DELETE", "/api/processes/abc"},
		{"active_style_bad_id", "PUT", "/api/processes/abc/active-style"},
		{"process_styles_bad_id", "GET", "/api/processes/abc/styles"},
		{"update_style_bad_id", "PUT", "/api/styles/abc"},
		{"delete_style_bad_id", "DELETE", "/api/styles/abc"},
		{"list_claims_bad_id", "GET", "/api/styles/abc/node-claims"},
		{"update_rp_bad_id", "PUT", "/api/reporting-points/abc"},
		{"delete_rp_bad_id", "DELETE", "/api/reporting-points/abc"},
		{"delete_claim_bad_id", "DELETE", "/api/style-node-claims/abc"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]string{"name": "x"}
			resp := doRequest(t, router, tc.method, tc.path, body, cookie)
			assertStatus(t, resp, http.StatusBadRequest)
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Test helpers (seeding, conversion)
// ═══════════════════════════════════════════════════════════════════════

func seedReportingPoint(t *testing.T, styleID int64, plcName, tagName string) int64 {
	t.Helper()
	id, err := testDB.CreateReportingPoint(plcName, tagName, styleID)
	if err != nil {
		t.Fatalf("seed reporting point: %v", err)
	}
	return id
}

func seedAnomalySnapshot(t *testing.T, rpID int64) int64 {
	t.Helper()
	id, err := testDB.InsertCounterSnapshot(rpID, 100, 50, "jump", false)
	if err != nil {
		t.Fatalf("seed anomaly snapshot: %v", err)
	}
	return id
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
