//go:build docker

package www

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Characterization tests for handlers_demand.go — pinned before the Stage 1
// refactor that replaces h.engine.DB() with named query methods. The demand
// handlers are simple CRUD passthroughs, but the chi.URLParam-based id
// extraction and the produced-qty guard branch are easy to drop in a
// large-scale rename, so they are pinned here.

// postJSONWithChi mirrors postJSON but injects a chi route context with the
// supplied URL params. Required by handlers that use chi.URLParam (e.g. the
// :id-bound demand handlers).
func postJSONWithChi(t *testing.T, handler http.HandlerFunc, path string, params map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf := &bytes.Buffer{}
	if err := json.NewEncoder(buf).Encode(body); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, buf)
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// --- apiCreateDemand --------------------------------------------------------

func TestApiCreateDemand_HappyPath(t *testing.T) {
	h, db := testHandlers(t)

	rec := postJSON(t, h.apiCreateDemand, "/api/demand",
		map[string]any{
			"cat_id":      "PART-1",
			"description": "first demand",
			"demand_qty":  int64(10),
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID == 0 {
		t.Fatalf("response missing id: %+v", resp)
	}

	got, err := db.GetDemand(resp.ID)
	if err != nil {
		t.Fatalf("get demand: %v", err)
	}
	if got.CatID != "PART-1" || got.Description != "first demand" || got.DemandQty != 10 || got.ProducedQty != 0 {
		t.Errorf("demand row: got %+v", got)
	}
}

func TestApiCreateDemand_MissingCatID(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiCreateDemand, "/api/demand",
		map[string]any{"cat_id": "", "description": "no cat", "demand_qty": int64(1)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- apiUpdateDemand --------------------------------------------------------

func TestApiUpdateDemand_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	id, err := db.CreateDemand("PART-2", "orig", 5)
	if err != nil {
		t.Fatalf("seed demand: %v", err)
	}

	rec := postJSONWithChi(t, h.apiUpdateDemand, "/api/demand/{id}",
		map[string]string{"id": strconv.FormatInt(id, 10)},
		map[string]any{
			"cat_id":       "PART-2-UPD",
			"description":  "updated",
			"demand_qty":   int64(20),
			"produced_qty": int64(7),
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, err := db.GetDemand(id)
	if err != nil {
		t.Fatalf("get demand: %v", err)
	}
	if got.CatID != "PART-2-UPD" || got.Description != "updated" || got.DemandQty != 20 || got.ProducedQty != 7 {
		t.Errorf("demand after update: got %+v", got)
	}
}

func TestApiUpdateDemand_InvalidID(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postJSONWithChi(t, h.apiUpdateDemand, "/api/demand/{id}",
		map[string]string{"id": "not-a-number"},
		map[string]any{"cat_id": "X", "demand_qty": int64(1)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- apiApplyDemand ---------------------------------------------------------

// TestApiApplyDemand_ResetsProduced pins the
// UpdateDemandAndResetProduced contract: produced_qty MUST be zeroed, demand
// fields are overwritten with the request payload.
func TestApiApplyDemand_ResetsProduced(t *testing.T) {
	h, db := testHandlers(t)
	id, err := db.CreateDemand("PART-APPLY", "before", 5)
	if err != nil {
		t.Fatalf("seed demand: %v", err)
	}
	if err := db.SetProduced(id, 3); err != nil {
		t.Fatalf("seed produced: %v", err)
	}

	rec := postJSONWithChi(t, h.apiApplyDemand, "/api/demand/{id}/apply",
		map[string]string{"id": strconv.FormatInt(id, 10)},
		map[string]any{"description": "after", "demand_qty": int64(50)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, err := db.GetDemand(id)
	if err != nil {
		t.Fatalf("get demand: %v", err)
	}
	if got.Description != "after" || got.DemandQty != 50 {
		t.Errorf("demand fields: got %+v", got)
	}
	if got.ProducedQty != 0 {
		t.Errorf("produced_qty: got %d, want reset to 0", got.ProducedQty)
	}
}

// --- apiDeleteDemand --------------------------------------------------------

func TestApiDeleteDemand_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	id, err := db.CreateDemand("PART-DEL", "to be removed", 1)
	if err != nil {
		t.Fatalf("seed demand: %v", err)
	}

	rec := postJSONWithChi(t, h.apiDeleteDemand, "/api/demand/{id}",
		map[string]string{"id": strconv.FormatInt(id, 10)}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if _, err := db.GetDemand(id); err == nil {
		t.Errorf("demand %d still exists after delete", id)
	}
}

// --- apiApplyAllDemands -----------------------------------------------------

// TestApiApplyAllDemands_BatchesAllRows pins the loop contract: every row in
// the request triggers UpdateDemandAndResetProduced, all produced counts go
// to 0. A refactor that broke partway through would leave inconsistent rows;
// the handler currently aborts on first error, so we also verify all rows
// are updated when no error occurs.
func TestApiApplyAllDemands_BatchesAllRows(t *testing.T) {
	h, db := testHandlers(t)
	id1, _ := db.CreateDemand("PART-ALL-1", "a", 5)
	id2, _ := db.CreateDemand("PART-ALL-2", "b", 6)
	_ = db.SetProduced(id1, 4)
	_ = db.SetProduced(id2, 5)

	rec := postJSON(t, h.apiApplyAllDemands, "/api/demand/apply-all",
		map[string]any{
			"rows": []map[string]any{
				{"id": id1, "description": "a-new", "demand_qty": int64(11)},
				{"id": id2, "description": "b-new", "demand_qty": int64(22)},
			},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	for _, want := range []struct {
		id     int64
		desc   string
		demand int64
	}{
		{id1, "a-new", 11},
		{id2, "b-new", 22},
	} {
		got, err := db.GetDemand(want.id)
		if err != nil {
			t.Fatalf("get demand %d: %v", want.id, err)
		}
		if got.Description != want.desc || got.DemandQty != want.demand || got.ProducedQty != 0 {
			t.Errorf("demand %d: got %+v, want desc=%q demand=%d produced=0",
				want.id, got, want.desc, want.demand)
		}
	}
}

// --- apiSetDemandProduced ---------------------------------------------------

// TestApiSetDemandProduced_HappyPath pins the SetProduced write.
func TestApiSetDemandProduced_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	id, _ := db.CreateDemand("PART-SET", "x", 50)

	rec := postJSONWithChi(t, h.apiSetDemandProduced, "/api/demand/{id}/produced",
		map[string]string{"id": strconv.FormatInt(id, 10)},
		map[string]any{"produced_qty": int64(33)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, _ := db.GetDemand(id)
	if got.ProducedQty != 33 {
		t.Errorf("produced_qty: got %d, want 33", got.ProducedQty)
	}
}

// TestApiSetDemandProduced_NegativeRejected pins the explicit guard
// (produced_qty < 0 → 400). A refactor that drops this guard would let
// callers wedge the demand into an invalid state.
func TestApiSetDemandProduced_NegativeRejected(t *testing.T) {
	h, db := testHandlers(t)
	id, _ := db.CreateDemand("PART-NEG", "x", 50)
	_ = db.SetProduced(id, 5)

	rec := postJSONWithChi(t, h.apiSetDemandProduced, "/api/demand/{id}/produced",
		map[string]string{"id": strconv.FormatInt(id, 10)},
		map[string]any{"produced_qty": int64(-1)})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Value unchanged after rejection.
	got, _ := db.GetDemand(id)
	if got.ProducedQty != 5 {
		t.Errorf("produced_qty after 400: got %d, want unchanged 5", got.ProducedQty)
	}
}

// --- apiClearDemandProduced + apiClearAllProduced ---------------------------

func TestApiClearDemandProduced_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	id, _ := db.CreateDemand("PART-CLR", "x", 10)
	_ = db.SetProduced(id, 7)

	rec := postJSONWithChi(t, h.apiClearDemandProduced, "/api/demand/{id}/clear-produced",
		map[string]string{"id": strconv.FormatInt(id, 10)}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, _ := db.GetDemand(id)
	if got.ProducedQty != 0 {
		t.Errorf("produced_qty: got %d, want 0", got.ProducedQty)
	}
}

func TestApiClearAllProduced_ZeroesEveryDemand(t *testing.T) {
	h, db := testHandlers(t)
	id1, _ := db.CreateDemand("PART-ALL-CLR-1", "a", 10)
	id2, _ := db.CreateDemand("PART-ALL-CLR-2", "b", 20)
	_ = db.SetProduced(id1, 3)
	_ = db.SetProduced(id2, 9)

	rec := postJSON(t, h.apiClearAllProduced, "/api/demand/clear-all-produced", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	for _, id := range []int64{id1, id2} {
		got, _ := db.GetDemand(id)
		if got.ProducedQty != 0 {
			t.Errorf("demand %d produced_qty: got %d, want 0", id, got.ProducedQty)
		}
	}
}

// --- apiDemandLog -----------------------------------------------------------

// TestApiDemandLog_NotFoundReturns404 pins the GetDemand-miss branch.
func TestApiDemandLog_NotFoundReturns404(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postJSONWithChi(t, h.apiDemandLog, "/api/demand/{id}/log",
		map[string]string{"id": "9999999"}, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
