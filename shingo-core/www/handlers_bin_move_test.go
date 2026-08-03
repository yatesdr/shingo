//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// submitRetrieveSpecificTo POSTs a bin-move through the operator door and
// returns the decoded envelope. Separate from the existing helper so these
// tests can name their own destination.
func submitRetrieveSpecificTo(t *testing.T, h *Handlers, binLabel, deliveryNode string) (*retrieveSpecificResponse, int) {
	t.Helper()
	rec := postJSON(t, h.apiManualOrderSubmit, "/api/orders/spot",
		map[string]any{
			"order_type":    "retrieve_specific",
			"bin_label":     binLabel,
			"delivery_node": deliveryNode,
			"description":   "bin-move gate test",
			"priority":      1,
		})
	var resp retrieveSpecificResponse
	if rec.Body.Len() > 0 {
		_ = json.NewDecoder(rec.Body).Decode(&resp)
	}
	return &resp, rec.Code
}

// TestBinMove_OperatorDoorRefusesAnOccupiedDestination gates the operator's
// bin-move on the destination having room.
//
// Only STORAGE dropoffs were gated, through the slot reservation. A lineside
// destination was not: an operator could send a bin to a line node that already
// held one, and both bins would end up contending for the same physical spot.
// The gate is the same one the wire path and the scanner already consult, so
// this door stops being the exception.
func TestBinMove_OperatorDoorRefusesAnOccupiedDestination(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-MOVE-SRC")
	// The lineside destination is already occupied.
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "BIN-MOVE-BLOCKING")

	resp, status := submitRetrieveSpecificTo(t, h, bin.Label, sd.LineNode.Name)

	if status == http.StatusOK {
		t.Fatalf("move to an occupied lineside node returned 200 — two bins now contend for one spot. body=%+v", resp)
	}
	if status != http.StatusConflict {
		t.Errorf("status = %d, want %d: the destination being full is a conflict with plant state, not a malformed request", status, http.StatusConflict)
	}
	if resp.Error == "" {
		t.Error("rejection carried no message; the page prints the server's string verbatim, so a blank one shows the operator nothing")
	}

	// The source bin must be left exactly as it was — not claimed, not reserved.
	after, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("reload source bin: %v", err)
	}
	if after.ClaimedBy != nil {
		t.Errorf("source bin came back claimed by order %d after a refused move", *after.ClaimedBy)
	}
	if after.HasPendingReservation {
		t.Error("source bin came back holding a reservation after a refused move — the next move would be told it is unavailable")
	}
}

// TestBinMove_TestPageDoorRefusesAnOccupiedDestination is the same gate on the
// /test-orders direct tab.
//
// That door is a test harness, which is an argument for exempting it and the
// owner ruled against: the page is used occasionally and its orders move real
// bins with real robots, so they go through the same gates. An exemption here
// would also be the kind that outlives the reason for it.
func TestBinMove_TestPageDoorRefusesAnOccupiedDestination(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-TP-SRC")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "BIN-TP-BLOCKING")

	rec := postJSON(t, h.apiDirectOrderSubmit, "/api/test-orders/direct",
		map[string]any{
			"from_node_id": sd.StorageNode.ID,
			"to_node_id":   sd.LineNode.ID,
			"priority":     1,
		})
	if rec.Code == http.StatusOK {
		t.Fatalf("direct move to an occupied node returned 200; body=%s", rec.Body.String())
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d; body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

// TestBinMove_OperatorDoorAllowsAFreeDestination is the other half: the gate
// must not block the ordinary move.
func TestBinMove_OperatorDoorAllowsAFreeDestination(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	free := &nodes.Node{Name: "LINE-FREE-FOR-MOVE", Enabled: true}
	if err := db.CreateNode(free); err != nil {
		t.Fatalf("create free node: %v", err)
	}
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-MOVE-OK")

	resp, status := submitRetrieveSpecificTo(t, h, bin.Label, free.Name)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an empty destination; err=%q", status, resp.Error)
	}
	if resp.OrderID == 0 {
		t.Fatalf("no order id in response: %+v", resp)
	}
}
