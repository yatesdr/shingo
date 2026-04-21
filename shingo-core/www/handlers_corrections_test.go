//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"shingocore/internal/testdb"
)

// Characterization tests for handlers_corrections.go — inventory correction
// endpoints backed by h.engine.ApplyCorrection and ApplyBatchCorrection.
// These exercise both the happy path (201-like OK + id) and the validation
// guards (missing reason for batch; invalid JSON).

// --- apiCreateCorrection ----------------------------------------------------

func TestApiCreateCorrection_HappyPath_AdjustQty(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-CORR-1")

	rec := postJSON(t, h.apiCreateCorrection, "/api/corrections/create",
		map[string]any{
			"correction_type": "adjust_qty",
			"node_id":         sd.StorageNode.ID,
			"bin_id":          bin.ID,
			"cat_id":          "PART-A",
			"description":     "counting fix",
			"quantity":        42,
			"reason":          "recount",
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
		t.Errorf("response missing id: %+v", resp)
	}

	// Verify a correction row was persisted on the node.
	corrs, err := db.ListCorrectionsByNode(sd.StorageNode.ID, 10)
	if err != nil {
		t.Fatalf("list corrections: %v", err)
	}
	if len(corrs) == 0 {
		t.Fatalf("expected at least one correction row, got 0")
	}
	found := false
	for _, c := range corrs {
		if c.ID == resp.ID {
			found = true
			if c.CorrectionType != "adjust_qty" || c.Reason != "recount" {
				t.Errorf("correction row: got %+v", c)
			}
		}
	}
	if !found {
		t.Errorf("correction id %d not found in node history", resp.ID)
	}
}

func TestApiCreateCorrection_HappyPath_AddItem(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-CORR-ADD")

	rec := postJSON(t, h.apiCreateCorrection, "/api/corrections/create",
		map[string]any{
			"correction_type": "add_item",
			"node_id":         sd.StorageNode.ID,
			"bin_id":          bin.ID,
			"cat_id":          "PART-B",
			"description":     "adding a surprise item",
			"quantity":        5,
			"reason":          "found",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Verify the bin's manifest now contains an entry for PART-B.
	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("reload bin: %v", err)
	}
	m, err := got.ParseManifest()
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	foundCat := false
	for _, it := range m.Items {
		if it.CatID == "PART-B" && it.Quantity == 5 {
			foundCat = true
		}
	}
	if !foundCat {
		t.Errorf("manifest missing PART-B entry after add_item: got %+v", m.Items)
	}
}

func TestApiCreateCorrection_InvalidJSON(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postRaw(t, h.apiCreateCorrection, "/api/corrections/create", []byte("not json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestApiCreateCorrection_MissingBin(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	// bin_id points to nothing — engine.ApplyCorrection's bin lookup fails and
	// the handler returns 500.
	rec := postJSON(t, h.apiCreateCorrection, "/api/corrections/create",
		map[string]any{
			"correction_type": "adjust_qty",
			"node_id":         sd.StorageNode.ID,
			"bin_id":          9999999,
			"cat_id":          "PART-A",
			"quantity":        1,
			"reason":          "r",
		})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "get bin")
}

// --- apiApplyBatchCorrection ------------------------------------------------

func TestApiApplyBatchCorrection_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-BATCH-1")

	rec := postJSON(t, h.apiApplyBatchCorrection, "/api/corrections/batch",
		map[string]any{
			"bin_id":  bin.ID,
			"node_id": sd.StorageNode.ID,
			"reason":  "physical recount",
			"items": []map[string]any{
				{"cat_id": "PART-A", "quantity": 7},
				{"cat_id": "PART-C", "quantity": 2},
			},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var env map[string]bool
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env["ok"] {
		t.Errorf("response: got %+v, want ok:true", env)
	}

	// At least one correction should be recorded for the node.
	corrs, err := db.ListCorrectionsByNode(sd.StorageNode.ID, 10)
	if err != nil {
		t.Fatalf("list corrections: %v", err)
	}
	if len(corrs) == 0 {
		t.Error("expected corrections after batch apply")
	}
}

func TestApiApplyBatchCorrection_MissingReason(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-BATCH-NOREASON")

	rec := postJSON(t, h.apiApplyBatchCorrection, "/api/corrections/batch",
		map[string]any{
			"bin_id":  bin.ID,
			"node_id": sd.StorageNode.ID,
			"reason":  "", // <-- required
			"items":   []map[string]any{},
		})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "reason is required")
}

func TestApiApplyBatchCorrection_InvalidJSON(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postRaw(t, h.apiApplyBatchCorrection, "/api/corrections/batch", []byte("broken"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// --- apiListNodeCorrections -------------------------------------------------

func TestApiListNodeCorrections_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-LIST-CORR")

	// Seed via the handler to produce a real correction row.
	rec := postJSON(t, h.apiCreateCorrection, "/api/corrections/create",
		map[string]any{
			"correction_type": "adjust_qty",
			"node_id":         sd.StorageNode.ID,
			"bin_id":          bin.ID,
			"cat_id":          "PART-A",
			"quantity":        3,
			"reason":          "recount",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed correction: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = getPlain(t, h.apiListNodeCorrections,
		"/api/corrections?node_id="+fmt.Sprint(sd.StorageNode.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) == 0 {
		t.Errorf("expected at least one correction for node %d", sd.StorageNode.ID)
	}
}

func TestApiListNodeCorrections_InvalidID(t *testing.T) {
	h, _ := testHandlers(t)
	rec := getPlain(t, h.apiListNodeCorrections, "/api/corrections?node_id=xyz")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}
