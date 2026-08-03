//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// submitStaged POSTs a staged (source → staging → delivery) order through the
// operator door and returns the decoded envelope.
func submitStaged(t *testing.T, h *Handlers, source, staging, delivery, payloadCode string) (*retrieveSpecificResponse, int) {
	t.Helper()
	rec := postJSON(t, h.apiManualOrderSubmit, "/api/orders/spot",
		map[string]any{
			"order_type":    "staged",
			"source_node":   source,
			"staging_node":  staging,
			"delivery_node": delivery,
			"payload_code":  payloadCode,
			"description":   "staged payload test",
			"priority":      1,
		})
	var resp retrieveSpecificResponse
	if rec.Body.Len() > 0 {
		_ = json.NewDecoder(rec.Body).Decode(&resp)
	}
	return &resp, rec.Code
}

// TestStagedOrder_TakesThePayloadFromTheBinNotTheOperator pins that a staged
// order created from a named source node carries what is actually in the bin
// there.
//
// The screen used to ask for the payload beside the source node, which is the
// same question twice — the source names the place, the place holds the bin,
// and the bin knows what it is. Two answers to one question can disagree, and
// the order used to carry the operator's. That answer is not decoration: it
// picks the robot group and the load sequence at dispatch, so a mis-click sent
// the wrong robot to a real bin.
func TestStagedOrder_TakesThePayloadFromTheBinNotTheOperator(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	// A second real payload, so the operator's wrong answer is a valid code and
	// gets past the references check rather than being refused for not existing.
	other := &payloads.Payload{Code: "PART-B", Description: "The other part", UOPCapacity: 1000}
	if err := db.CreatePayload(other); err != nil {
		t.Fatalf("create second payload: %v", err)
	}

	staging := &nodes.Node{Name: "STAGE-FOR-PAYLOAD-TEST", Enabled: true}
	if err := db.CreateNode(staging); err != nil {
		t.Fatalf("create staging node: %v", err)
	}
	delivery := &nodes.Node{Name: "DELIV-FOR-PAYLOAD-TEST", Enabled: true}
	if err := db.CreateNode(delivery); err != nil {
		t.Fatalf("create delivery node: %v", err)
	}

	// The bin actually sitting at the source holds PART-A.
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-STAGED-PAYLOAD")

	// The operator says PART-B.
	resp, status := submitStaged(t, h, sd.StorageNode.Name, staging.Name, delivery.Name, other.Code)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; err=%q", status, resp.Error)
	}

	order, err := db.GetOrder(resp.OrderID)
	if err != nil {
		t.Fatalf("reload order %d: %v", resp.OrderID, err)
	}
	if order.PayloadCode != sd.Payload.Code {
		t.Errorf("order carries payload %q, want %q — the bin at %s holds %q, and that is what is being moved",
			order.PayloadCode, sd.Payload.Code, sd.StorageNode.Name, sd.Payload.Code)
	}
}

// TestStagedOrder_KeepsTheOperatorsPayloadWhenTheSourceIsAGroup is the other
// half of the rule, and the reason it is not "always derive".
//
// A group source is a different question. There the payload is not a
// description of a known bin — it is the SELECTOR the resolver uses to choose
// one among the group's children (resolveStepNode in dispatch/complex_steps.go
// consults payloadCode only for synthetic NGRPs). Overwriting it would be
// answering the question the operator was actually asked.
func TestStagedOrder_KeepsTheOperatorsPayloadWhenTheSourceIsAGroup(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	other := &payloads.Payload{Code: "PART-B", Description: "The other part", UOPCapacity: 1000}
	if err := db.CreatePayload(other); err != nil {
		t.Fatalf("create second payload: %v", err)
	}

	svc := h.engine.NodeService()
	groupID, err := svc.CreateNodeGroup("SUPERMARKET-FOR-PAYLOAD-TEST")
	if err != nil {
		t.Fatalf("create node group: %v", err)
	}
	group, err := db.GetNode(groupID)
	if err != nil {
		t.Fatalf("reload node group: %v", err)
	}
	laneID, err := svc.AddLane(groupID, "SUPERMARKET-FOR-PAYLOAD-TEST-L1")
	if err != nil {
		t.Fatalf("add lane: %v", err)
	}

	// One PART-B bin parked inside the group, so the selector has something to
	// select. The bin's own node is a slot in a lane, not the group itself.
	slot := &nodes.Node{Name: "SUPERMARKET-FOR-PAYLOAD-TEST-L1-S1", Enabled: true, ParentID: &laneID}
	if err := db.CreateNode(slot); err != nil {
		t.Fatalf("create group slot: %v", err)
	}
	testdb.CreateBinAtNode(t, db, other.Code, slot.ID, "BIN-STAGED-GROUP")

	staging := &nodes.Node{Name: "STAGE-FOR-GROUP-TEST", Enabled: true}
	if err := db.CreateNode(staging); err != nil {
		t.Fatalf("create staging node: %v", err)
	}
	delivery := &nodes.Node{Name: "DELIV-FOR-GROUP-TEST", Enabled: true}
	if err := db.CreateNode(delivery); err != nil {
		t.Fatalf("create delivery node: %v", err)
	}
	_ = sd

	resp, status := submitStaged(t, h, group.Name, staging.Name, delivery.Name, other.Code)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; err=%q", status, resp.Error)
	}

	order, err := db.GetOrder(resp.OrderID)
	if err != nil {
		t.Fatalf("reload order %d: %v", resp.OrderID, err)
	}
	if order.PayloadCode != other.Code {
		t.Errorf("order carries payload %q, want %q — a group source has no single bin to read, so the operator's pick is the input, not a guess to be corrected",
			order.PayloadCode, other.Code)
	}
}
