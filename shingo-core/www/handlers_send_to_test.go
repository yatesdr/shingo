//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

// sendToResponse covers both shapes this endpoint returns: readBackManualOrder's
// envelope on success and jsonError's single-key object on rejection.
type sendToResponse struct {
	OrderID     int64  `json:"order_id"`
	Status      string `json:"status"`
	ErrorDetail string `json:"error_detail"`
	Error       string `json:"error"`
}

// submitSendTo POSTs a send-to spot order through the public handler.
func submitSendTo(t *testing.T, h *Handlers, deliveryNode string) (*sendToResponse, int) {
	t.Helper()
	rec := postJSON(t, h.apiManualOrderSubmit, "/api/orders/spot",
		map[string]any{
			"order_type":    "send_to",
			"delivery_node": deliveryNode,
			"description":   "send-to occupancy test",
			"priority":      1,
		})
	var resp sendToResponse
	if rec.Body.Len() > 0 {
		_ = json.NewDecoder(rec.Body).Decode(&resp)
	}
	return &resp, rec.Code
}

// TestSubmitSpotSendTo_RefusesAnOccupiedDestination is the gate.
//
// Send-to moves a robot to a location and parks it there — no bin, a dwell held
// open with Complete:false on the fleet request. It had no occupancy check of
// any kind: an operator could send a robot to a node that already holds a bin,
// and find out by watching it arrive and stop.
//
// The check runs BEFORE the order row is written, which is not incidental. The
// handler creates the row and then calls the fleet, so rejecting after the
// insert would leave a pending order nothing will ever dispatch, fail, or clean
// up. Checking first also keeps the capacity call honest: it excludes no order
// id, and there is no order yet to exclude, so the order cannot collide with
// itself in the in-flight count.
func TestSubmitSpotSendTo_RefusesAnOccupiedDestination(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	// A bin is sitting at the destination.
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "BIN-SENDTO-OCCUPIED")

	resp, status := submitSendTo(t, h, sd.LineNode.Name)

	if status == http.StatusOK {
		t.Fatalf("send-to to an occupied node returned 200 — the robot would be dispatched to a spot that already holds a bin. body=%+v", resp)
	}
	if status != http.StatusConflict {
		t.Errorf("status = %d, want %d: an occupied destination is a conflict with the plant's current state, not a malformed request",
			status, http.StatusConflict)
	}
	if resp.Error == "" {
		t.Error("rejection carried no message. The page prints the server's error string verbatim into the status line, so a blank one shows the operator nothing.")
	}
	if strings.Contains(strings.ToLower(resp.Error), "capacity") && !strings.Contains(resp.Error, sd.LineNode.Name) {
		t.Errorf("rejection message %q does not name the destination — the operator has to know WHICH node to clear", resp.Error)
	}

	// And no order row was written.
	if resp.OrderID != 0 {
		t.Errorf("an order row (%d) was created before the rejection — it would sit pending forever, since nothing dispatches, fails or cleans it up", resp.OrderID)
	}
	rows, err := db.ListOrdersByStation("core-spot", 50)
	if err != nil {
		t.Fatalf("list core-spot orders: %v", err)
	}
	for _, o := range rows {
		if o.OrderType == "send_to" {
			t.Errorf("order %d (%s) was persisted despite the rejection, status=%s", o.ID, o.EdgeUUID, o.Status)
		}
	}
}

// TestSubmitSpotSendTo_AllowsAFreeDestination is the other half: the gate has to
// reject the occupied case without blocking the ordinary one.
//
// It also pins the three things about this door that are deliberate and easy to
// mistake for oversights: the dwell (the fleet request is left incomplete on
// purpose), the hand-written "dispatched" status (this path bypasses the
// planner, so the handler stamps it), and the no_demand origin class (an
// operator action is structurally originless, and leaving it blank would file
// it as a lost demand link).
func TestSubmitSpotSendTo_AllowsAFreeDestination(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	resp, status := submitSendTo(t, h, sd.LineNode.Name)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an empty destination; err=%q", status, resp.Error)
	}
	if resp.OrderID == 0 {
		t.Fatalf("no order id in response: %+v", resp)
	}

	got, err := db.GetOrder(resp.OrderID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if got.Status != "dispatched" {
		t.Errorf("status = %q, want %q — this path bypasses the planner and stamps the status itself", got.Status, "dispatched")
	}
	if got.OriginClass != "no_demand" {
		t.Errorf("origin_class = %q, want %q — an unstamped row files as an orphan, i.e. as a lost demand link rather than a deliberate operator action",
			got.OriginClass, "no_demand")
	}
	if got.DeliveryNode != sd.LineNode.Name {
		t.Errorf("delivery_node = %q, want %q", got.DeliveryNode, sd.LineNode.Name)
	}
}
