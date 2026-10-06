//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/orders"
	"shingocore/store/telemetry"
)

// Characterization tests for handlers_missions.go — the missions page, the
// per-mission detail page, and the three JSON endpoints (list, get, stats).
// Uses the `chiReq` helper from handlers_telemetry_test.go for the orderID
// URL param on the two chi-bound handlers.

// --- handleMissions ---------------------------------------------------------

// TestHandleMissions_RendersHTML pins that the missions page renders via the
// templates loaded by loadTestTemplates.
func TestHandleMissions_RendersHTML(t *testing.T) {
	t.Parallel()
	h, _ := testHandlersForPages(t)

	req := httptest.NewRequest(http.MethodGet, "/missions", nil)
	rec := httptest.NewRecorder()
	h.handleMissions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Errorf("expected HTML output, got prefix %q", body[:min(120, len(body))])
	}
}

// --- handleMissionDetail ----------------------------------------------------

// TestHandleMissionDetail_InvalidID pins that a non-integer orderID returns
// 400. The handler uses http.Error (plain text), not a JSON envelope.
func TestHandleMissionDetail_InvalidID(t *testing.T) {
	t.Parallel()
	h, _ := testHandlersForPages(t)

	req := chiReq(http.MethodGet, "/missions/not-a-number",
		map[string]string{"orderID": "not-a-number"})
	rec := httptest.NewRecorder()
	h.handleMissionDetail(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid order id") {
		t.Errorf("body: got %q, want 'invalid order id'", rec.Body.String())
	}
}

// TestHandleMissionDetail_UnknownOrder pins the 404 path when the orderID is
// a valid integer but the order doesn't exist in the DB.
func TestHandleMissionDetail_UnknownOrder(t *testing.T) {
	t.Parallel()
	h, _ := testHandlersForPages(t)

	req := chiReq(http.MethodGet, "/missions/999999",
		map[string]string{"orderID": "999999"})
	rec := httptest.NewRecorder()
	h.handleMissionDetail(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleMissionDetail_HappyPath pins that an existing order renders its
// detail page as HTML with the mission-detail.html template.
func TestHandleMissionDetail_HappyPath(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForPages(t)
	o := &orders.Order{
		EdgeUUID:   "mission-detail-1",
		StationID:  "line-x",
		OrderType:  "move",
		Status:     "pending",
		Quantity:   1,
		SourceNode: "STORAGE-A1",
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")

	req := chiReq(http.MethodGet, fmt.Sprintf("/missions/%d", o.ID),
		map[string]string{"orderID": fmt.Sprint(o.ID)})
	rec := httptest.NewRecorder()
	h.handleMissionDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The detail template has a "Back to Missions" link — verify it rendered.
	if !strings.Contains(rec.Body.String(), "Back to Missions") {
		t.Errorf("mission-detail did not render; body excerpt=%q",
			rec.Body.String()[:min(200, rec.Body.Len())])
	}
}

// --- apiListMissions --------------------------------------------------------

// TestApiListMissions_EmptyDB pins the list shape on an empty DB: total=0
// and missions is a null/empty array. limit defaults to 50, offset to 0.
func TestApiListMissions_EmptyDB(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiListMissions, "/api/missions")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Missions []map[string]any `json:"missions"`
		Total    int              `json:"total"`
		Limit    int              `json:"limit"`
		Offset   int              `json:"offset"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if resp.Total != 0 {
		t.Errorf("total: got %d, want 0", resp.Total)
	}
	if resp.Limit != 50 {
		t.Errorf("limit: got %d, want default 50", resp.Limit)
	}
	if resp.Offset != 0 {
		t.Errorf("offset: got %d, want 0", resp.Offset)
	}
}

// TestApiListMissions_ParsesFilters pins the query-string parser: explicit
// limit, offset, station_id, robot_id, and the since/until date parser all
// echo into the JSON response.
func TestApiListMissions_ParsesFilters(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiListMissions,
		"/api/missions?limit=10&offset=20&station_id=line-1&robot_id=R1&since=2025-01-01&until=2025-02-01")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if resp.Limit != 10 {
		t.Errorf("limit: got %d, want 10", resp.Limit)
	}
	if resp.Offset != 20 {
		t.Errorf("offset: got %d, want 20", resp.Offset)
	}
}

// TestApiListMissions_IgnoresBogusLimitOffset pins parser tolerance: garbage
// limit/offset params fall back to defaults (limit=50, offset=0).
func TestApiListMissions_IgnoresBogusLimitOffset(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiListMissions, "/api/missions?limit=abc&offset=-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if resp.Limit != 50 || resp.Offset != 0 {
		t.Errorf("bogus params should fall back to defaults; got limit=%d offset=%d",
			resp.Limit, resp.Offset)
	}
}

// --- apiGetMission ----------------------------------------------------------

// TestApiGetMission_InvalidID pins that non-integer orderID → 400 (plain
// http.Error, not JSON envelope).
func TestApiGetMission_InvalidID(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	req := chiReq(http.MethodGet, "/api/missions/not-a-number",
		map[string]string{"orderID": "not-a-number"})
	rec := httptest.NewRecorder()
	h.apiGetMission(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiGetMission_UnknownOrder pins 404 for a valid-but-missing orderID.
func TestApiGetMission_UnknownOrder(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	req := chiReq(http.MethodGet, "/api/missions/99999",
		map[string]string{"orderID": "99999"})
	rec := httptest.NewRecorder()
	h.apiGetMission(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiGetMission_HappyPath pins the response shape for a known order:
// {order, telemetry, events, history} with the order object populated.
func TestApiGetMission_HappyPath(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	o := &orders.Order{
		EdgeUUID:   "mission-get-1",
		StationID:  "line-x",
		OrderType:  "move",
		Status:     "pending",
		Quantity:   1,
		SourceNode: "STORAGE-A1",
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")

	req := chiReq(http.MethodGet, fmt.Sprintf("/api/missions/%d", o.ID),
		map[string]string{"orderID": fmt.Sprint(o.ID)})
	rec := httptest.NewRecorder()
	h.apiGetMission(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Order     map[string]any   `json:"order"`
		Telemetry map[string]any   `json:"telemetry"`
		Events    []map[string]any `json:"events"`
		History   []map[string]any `json:"history"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if resp.Order == nil {
		t.Fatalf("order missing from response: %+v", resp)
	}
	if idFloat, ok := resp.Order["id"].(float64); !ok || int64(idFloat) != o.ID {
		t.Errorf("order.id: got %v, want %d", resp.Order["id"], o.ID)
	}
}

// --- apiMissionStats --------------------------------------------------------

// TestApiMissionStats_EmptyDB pins that stats endpoint returns 200 with a
// MissionStats-shaped JSON body on an empty DB (total=0, success=0).
func TestApiMissionStats_EmptyDB(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiMissionStats, "/api/missions/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var stats telemetry.Stats
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&stats), "decode")
	if stats.TotalMissions != 0 {
		t.Errorf("total: got %d, want 0", stats.TotalMissions)
	}
}

// TestApiMissionStats_AcceptsFilters pins that the handler reads the same
// filters as apiListMissions and returns a 200 with a valid body.
func TestApiMissionStats_AcceptsFilters(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiMissionStats,
		"/api/missions/stats?station_id=line-1&since=2025-01-01&until=2025-12-31")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var stats telemetry.Stats
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&stats), "decode")
}

// --- R6 / U5 characterisation pins (pre-change tree, 2026-10-05) ------------

// TestApiGetMission_LegRowBlockView pins the block view a leg row is served
// with. The leg row's blocks_json is the engine's blockLeg record (blockId,
// binTask, startTime…), but the view decodes it as fleet.BlockSnapshot
// (block_id, state), so the chip arrives with a location and a blank id and
// state — the "Blocks: UTN_013: -" chip the walkthrough found on every leg.
func TestApiGetMission_LegRowBlockView(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	o := &orders.Order{EdgeUUID: "mission-leg-1", StationID: "line-x", OrderType: "move",
		Status: "pending", Quantity: 1, SourceNode: "ALN_002"}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")
	testutil.MustNoErr(t, db.InsertMissionEvent(&telemetry.Event{
		OrderID: o.ID, NewState: "BLOCK_FINISHED", RobotID: "AMR-01", ErrorsJSON: "[]",
		BlocksJSON: `[{"blockId":"b1","location":"UTN_013","binTask":"JackUnload","startTime":100,"terminateTime":118,"durationSeconds":18}]`,
	}), "insert leg event")

	req := chiReq(http.MethodGet, fmt.Sprintf("/api/missions/%d", o.ID),
		map[string]string{"orderID": fmt.Sprint(o.ID)})
	rec := httptest.NewRecorder()
	h.apiGetMission(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Events []map[string]any `json:"events"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if len(resp.Events) != 1 {
		t.Fatalf("events: got %d, want 1", len(resp.Events))
	}
	ev := resp.Events[0]
	if ev["is_leg"] != true {
		t.Errorf("is_leg: got %v, want true", ev["is_leg"])
	}
	blocks, _ := ev["blocks"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("blocks: got %v, want one chip", ev["blocks"])
	}
	b := blocks[0].(map[string]any)
	if b["location"] != "UTN_013" || b["block_id"] != "" || b["state"] != "" {
		t.Errorf("leg chip: got %v, want location UTN_013 with blank block_id/state", b)
	}
}

// TestApiListMissions_InFlightAndState pins what the Missions list serves for
// one finished and one in-flight order: only the finished one (the list reads
// mission_telemetry, which has a row only at a terminal state), and its state
// as the vendor's terminal word.
func TestApiListMissions_InFlightAndState(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	done := &orders.Order{EdgeUUID: "mission-list-done", StationID: "line-x", OrderType: "retrieve",
		Status: "confirmed", Quantity: 1, SourceNode: "ALN_002", DeliveryNode: "UTN_010"}
	testutil.MustNoErr(t, db.CreateOrder(done), "create finished order")
	live := &orders.Order{EdgeUUID: "mission-list-live", StationID: "line-x", OrderType: "retrieve",
		Status: "staged", Quantity: 1, SourceNode: "ALN_008", DeliveryNode: "PLK_H1", RobotID: "AMR-13"}
	testutil.MustNoErr(t, db.CreateOrder(live), "create in-flight order")
	created := done.CreatedAt
	testutil.MustNoErr(t, db.UpsertMissionTelemetry(&telemetry.Mission{
		OrderID: done.ID, RobotID: "AMR-02", StationID: "line-x", OrderType: "retrieve",
		SourceNode: "ALN_002", DeliveryNode: "UTN_010", TerminalState: "FINISHED",
		CoreCreated: &created, CoreCompleted: &created, DurationMS: 1500000,
		BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]", RobotAlarmsJSON: "[]",
	}), "upsert telemetry")

	rec := getPlain(t, h.apiListMissions, "/api/missions")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Missions []map[string]any `json:"missions"`
		Total    int              `json:"total"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if resp.Total != 1 || len(resp.Missions) != 1 {
		t.Fatalf("list: got total %d / %d rows, want only the finished mission", resp.Total, len(resp.Missions))
	}
	m := resp.Missions[0]
	if int64(m["order_id"].(float64)) != done.ID {
		t.Errorf("row order_id: got %v, want %d", m["order_id"], done.ID)
	}
	if m["terminal_state"] != "FINISHED" {
		t.Errorf("terminal_state: got %v, want the vendor word FINISHED", m["terminal_state"])
	}
	if _, ok := m["status"]; ok {
		t.Errorf("row carries a status field before the change: %v", m["status"])
	}
}
