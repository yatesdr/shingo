//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// --- R6 / U5: mission detail stages, missions list in flight -----------------

// TestApiGetMission_LegRowCarriesNoBlockView pins that a leg row is served with
// its raw blocks_json and no decoded block view.
//
// Before R6 every event carried a "blocks" view decoded as fleet.BlockSnapshot
// (block_id, state). A leg row's blocks_json is the engine's blockLeg record
// (blockId, binTask, startTime…), so the keys never lined up and the chip
// arrived with a location and a blank id and state — the "Blocks: UTN_013: -"
// chip on every leg. The stage view prints no chips, so the mapping is gone
// rather than fixed; the page reads blocks_json for the leg itself.
func TestApiGetMission_LegRowCarriesNoBlockView(t *testing.T) {
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
		Events  []map[string]any `json:"events"`
		History []map[string]any `json:"history"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if len(resp.Events) != 1 {
		t.Fatalf("events: got %d, want 1", len(resp.Events))
	}
	ev := resp.Events[0]
	if ev["is_leg"] != true {
		t.Errorf("is_leg: got %v, want true", ev["is_leg"])
	}
	if _, ok := ev["blocks"]; ok {
		t.Errorf("event still carries a decoded block view: %v", ev["blocks"])
	}
	if s, _ := ev["blocks_json"].(string); !strings.Contains(s, "JackUnload") || !strings.Contains(s, "startTime") {
		t.Errorf("blocks_json: got %v, want the stored leg record", ev["blocks_json"])
	}
	// The stage view's source: the order's birth row is in history.
	if len(resp.History) != 1 || resp.History[0]["status"] != "pending" {
		t.Errorf("history: got %v, want the pending birth row", resp.History)
	}
}

// TestApiListMissions_InFlightAndState pins that the Missions list carries the
// orders still in flight, first, and that every row's status is the order's
// status in the protocol vocabulary.
//
// Before: the list read mission_telemetry alone, which has a row only at a
// terminal state, so the in-flight order was absent (total 1), and the page
// labelled the finished one from the vendor word FINISHED as "completed" — a
// word no order status spells.
func TestApiListMissions_InFlightAndState(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	done := &orders.Order{EdgeUUID: "mission-list-done", StationID: "line-x", OrderType: "retrieve",
		Status: "confirmed", Quantity: 1, SourceNode: "ALN_002", DeliveryNode: "UTN_010"}
	testutil.MustNoErr(t, db.CreateOrder(done), "create finished order")
	live := &orders.Order{EdgeUUID: "mission-list-live", StationID: "line-x", OrderType: "retrieve",
		Status: "staged", Quantity: 1, SourceNode: "ALN_008", DeliveryNode: "PLK_H1", RobotID: "AMR-13"}
	testutil.MustNoErr(t, db.CreateOrder(live), "create in-flight order")
	created := time.Now().UTC()
	testutil.MustNoErr(t, db.UpsertMissionTelemetry(&telemetry.Mission{
		OrderID: done.ID, RobotID: "AMR-02", StationID: "line-x", OrderType: "retrieve",
		SourceNode: "ALN_002", DeliveryNode: "UTN_010", TerminalState: "FINISHED",
		CoreCreated: &created, CoreCompleted: &created, DurationMS: 1500000,
		BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]", RobotAlarmsJSON: "[]",
	}), "upsert telemetry")

	type listResp struct {
		Missions []map[string]any `json:"missions"`
		Total    int              `json:"total"`
	}
	list := func(url string) listResp {
		t.Helper()
		rec := getPlain(t, h.apiListMissions, url)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d; body=%s", url, rec.Code, rec.Body.String())
		}
		var resp listResp
		testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
		return resp
	}

	resp := list("/api/missions")
	if resp.Total != 2 || len(resp.Missions) != 2 {
		t.Fatalf("list: got total %d / %d rows, want the in-flight and the finished order", resp.Total, len(resp.Missions))
	}
	first, second := resp.Missions[0], resp.Missions[1]
	if int64(first["order_id"].(float64)) != live.ID || first["status"] != "staged" || first["in_flight"] != true {
		t.Errorf("first row: got order %v status %v in_flight %v, want the in-flight order %d, staged",
			first["order_id"], first["status"], first["in_flight"], live.ID)
	}
	if first["source_node"] != "ALN_008" || first["delivery_node"] != "PLK_H1" || first["core_completed"] != nil {
		t.Errorf("in-flight row: got source %v delivery %v completed %v", first["source_node"], first["delivery_node"], first["core_completed"])
	}
	if int64(second["order_id"].(float64)) != done.ID || second["status"] != "confirmed" || second["in_flight"] != false {
		t.Errorf("second row: got order %v status %v, want the finished order %d, confirmed",
			second["order_id"], second["status"], done.ID)
	}
	if second["terminal_state"] != "FINISHED" {
		t.Errorf("terminal_state: got %v, want the vendor word kept beside the status", second["terminal_state"])
	}

	// A state filter is a vendor terminal word: in-flight orders are outside it.
	if r := list("/api/missions?state=FINISHED"); r.Total != 1 {
		t.Errorf("state=FINISHED: got total %d, want 1", r.Total)
	}
	// A window that ended before now holds no order that is in flight now.
	if r := list("/api/missions?until=2025-01-01"); r.Total != 0 {
		t.Errorf("until=2025-01-01: got total %d, want 0", r.Total)
	}
	// Station filters both halves.
	if r := list("/api/missions?station_id=line-y"); r.Total != 0 {
		t.Errorf("station_id=line-y: got total %d, want 0", r.Total)
	}
}
