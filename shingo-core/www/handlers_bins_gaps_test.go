//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/orders"
)

// Characterization tests for the write-path handlers in handlers_bins.go that
// are NOT already exercised by handlers_bins_test.go (which focuses on
// executeBinAction dispatch). Pinned before the Stage 1 refactor:
//
//   - apiBulkBinAction: the size guard (1..100), locked-bin skip (action !=
//     "unlock"), and per-bin error reporting structure.
//   - apiRequestBinTransport: claim-guard (409), node-presence guards, and
//     the happy-path response shape.

// bulkResp mirrors the envelope emitted by apiBulkBinAction.
type bulkResp struct {
	Results []struct {
		ID    int64  `json:"id"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	} `json:"results"`
}

// transportResp mirrors the envelope emitted by apiRequestBinTransport.
type transportResp struct {
	Message string `json:"message"`
	BinID   int64  `json:"bin_id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Error   string `json:"error"`
}

// --- apiBulkBinAction -------------------------------------------------------

// TestApiBulkBinAction_EmptyIDsReturns400 pins the lower bound of the size
// guard. The handler rejects zero-length id slices.
func TestApiBulkBinAction_EmptyIDsReturns400(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiBulkBinAction, "/api/bin/bulk",
		map[string]any{"ids": []int64{}, "action": "flag"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiBulkBinAction_TooManyIDsReturns400 pins the upper bound: 101+
// ids are rejected without any per-bin processing.
func TestApiBulkBinAction_TooManyIDsReturns400(t *testing.T) {
	h, _ := testHandlers(t)

	ids := make([]int64, 101)
	for i := range ids {
		ids[i] = int64(i + 1)
	}

	rec := postJSON(t, h.apiBulkBinAction, "/api/bin/bulk",
		map[string]any{"ids": ids, "action": "flag"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiBulkBinAction_LockedSkipsNonUnlockActions pins the
// "locked-by-X" per-bin error: bins that are locked must be skipped for
// any action except "unlock", and the skip surfaces in the results array
// with ok=false and an explanatory error. Unlocked bins in the same batch
// must still succeed — this is the main reason the handler loops per-id
// rather than batching everything into one SQL statement.
func TestApiBulkBinAction_LockedSkipsNonUnlockActions(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	locked := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-BULK-LOCKED")
	open := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-BULK-OPEN")

	if err := db.LockBin(locked.ID, "qa"); err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	rec := postJSON(t, h.apiBulkBinAction, "/api/bin/bulk",
		map[string]any{
			"ids":    []int64{locked.ID, open.ID},
			"action": "flag",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (per-bin results); body=%s", rec.Code, rec.Body.String())
	}

	var resp bulkResp
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results len: got %d, want 2 — %+v", len(resp.Results), resp)
	}

	byID := map[int64]struct {
		OK  bool
		Err string
	}{}
	for _, r := range resp.Results {
		byID[r.ID] = struct {
			OK  bool
			Err string
		}{r.OK, r.Error}
	}

	lockedResult := byID[locked.ID]
	if lockedResult.OK {
		t.Errorf("locked bin %d: ok should be false", locked.ID)
	}
	if lockedResult.Err == "" {
		t.Errorf("locked bin %d: error should be populated (locked by ...)", locked.ID)
	}

	openResult := byID[open.ID]
	if !openResult.OK {
		t.Errorf("open bin %d: ok=false, err=%q (expected success)", open.ID, openResult.Err)
	}

	// Locked bin's status must remain untouched (flag did NOT apply).
	got, err := db.GetBin(locked.ID)
	if err != nil {
		t.Fatalf("get locked bin: %v", err)
	}
	if got.Status == "flagged" {
		t.Errorf("locked bin should not have been flagged; status=%q", got.Status)
	}
}

// TestApiBulkBinAction_UnlockActionProceedsOnLockedBin pins the inverse of
// the skip: when the action IS "unlock" the handler must NOT skip locked
// bins — unlocking a locked bin is exactly the operation the caller wants.
func TestApiBulkBinAction_UnlockActionProceedsOnLockedBin(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-BULK-UNLOCK")
	if err := db.LockBin(bin.ID, "qa"); err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	rec := postJSON(t, h.apiBulkBinAction, "/api/bin/bulk",
		map[string]any{"ids": []int64{bin.ID}, "action": "unlock"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp bulkResp
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Results) != 1 || !resp.Results[0].OK {
		t.Fatalf("unlock result: %+v, want ok=true", resp.Results)
	}

	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if got.Locked {
		t.Errorf("bin should be unlocked after action; Locked=%v", got.Locked)
	}
}

// TestApiBulkBinAction_BinNotFoundAppearsInResults pins the per-bin
// not-found branch: a missing id produces ok=false/error="not found" in
// the results envelope without failing the whole request.
func TestApiBulkBinAction_BinNotFoundAppearsInResults(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiBulkBinAction, "/api/bin/bulk",
		map[string]any{"ids": []int64{9999999}, "action": "flag"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (errors reported per-bin); body=%s", rec.Code, rec.Body.String())
	}
	var resp bulkResp
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Results) != 1 || resp.Results[0].OK || resp.Results[0].Error == "" {
		t.Errorf("results: got %+v, want one entry with ok=false and an error", resp.Results)
	}
}

// --- apiRequestBinTransport -------------------------------------------------

// TestApiRequestBinTransport_HappyPath pins the happy-path envelope:
// un-claimed bin with a known source + destination node returns 200 with a
// message like "Transport requested: SRC → DST".
func TestApiRequestBinTransport_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-TRANSPORT-OK")

	rec := postJSON(t, h.apiRequestBinTransport, "/api/bin/transport",
		map[string]any{
			"bin_id":              bin.ID,
			"destination_node_id": sd.LineNode.ID,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp transportResp
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.BinID != bin.ID {
		t.Errorf("bin_id: got %d, want %d", resp.BinID, bin.ID)
	}
	if resp.From != sd.StorageNode.Name || resp.To != sd.LineNode.Name {
		t.Errorf("from/to: got %q/%q, want %q/%q",
			resp.From, resp.To, sd.StorageNode.Name, sd.LineNode.Name)
	}
	if resp.Message == "" {
		t.Error("response message should be populated")
	}
}

// TestApiRequestBinTransport_BinNotFoundReturns404 pins the GetBin-miss
// branch.
func TestApiRequestBinTransport_BinNotFoundReturns404(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	rec := postJSON(t, h.apiRequestBinTransport, "/api/bin/transport",
		map[string]any{"bin_id": 9999999, "destination_node_id": sd.LineNode.ID})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiRequestBinTransport_ClaimedBinReturns409 pins the
// claim-guard: a bin already claimed by an active order returns 409
// Conflict with no writes. This is the critical "don't trample a live
// order" invariant.
func TestApiRequestBinTransport_ClaimedBinReturns409(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-TRANSPORT-CLAIMED")

	// Seed a claim via an existing order.
	priorOrder := &orders.Order{
		EdgeUUID: "xport-prior-1", StationID: "line-1",
		OrderType: "move", Status: "pending", Quantity: 1,
	}
	if err := db.CreateOrder(priorOrder); err != nil {
		t.Fatalf("create prior order: %v", err)
	}
	if err := db.ClaimBin(bin.ID, priorOrder.ID); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	rec := postJSON(t, h.apiRequestBinTransport, "/api/bin/transport",
		map[string]any{"bin_id": bin.ID, "destination_node_id": sd.LineNode.ID})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// Claim is preserved — no side-effects.
	testdb.RequireBinClaimedBy(t, db, bin.ID, priorOrder.ID)
}

// TestApiRequestBinTransport_NoNodeReturns400 pins the "bin has no current
// location" branch: a bin without a node_id cannot be routed.
func TestApiRequestBinTransport_NoNodeReturns400(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	bt, _ := db.GetBinTypeByCode("DEFAULT")
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-TRANSPORT-NONODE", Status: "available"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create orphan bin: %v", err)
	}

	rec := postJSON(t, h.apiRequestBinTransport, "/api/bin/transport",
		map[string]any{"bin_id": bin.ID, "destination_node_id": sd.LineNode.ID})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiRequestBinTransport_SameLocationReturns400 pins the no-op guard:
// destination == current node is rejected so callers don't queue pointless
// moves.
func TestApiRequestBinTransport_SameLocationReturns400(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-TRANSPORT-SAME")

	rec := postJSON(t, h.apiRequestBinTransport, "/api/bin/transport",
		map[string]any{"bin_id": bin.ID, "destination_node_id": sd.StorageNode.ID})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiRequestBinTransport_UnknownDestinationReturns404 pins the
// destination-not-found branch.
func TestApiRequestBinTransport_UnknownDestinationReturns404(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-TRANSPORT-NODEST")

	rec := postJSON(t, h.apiRequestBinTransport, "/api/bin/transport",
		map[string]any{"bin_id": bin.ID, "destination_node_id": 9999999})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

