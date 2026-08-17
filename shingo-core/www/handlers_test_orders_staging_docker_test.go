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

// handlers_test_orders_staging_docker_test.go — the behaviour half of unit 2d,
// pinning what the fence (handlers_test_orders_staging_test.go) checks at the
// source level, on the PERSISTED steps of a real submission.
//
// The fence walks the builders; this drives the handler. Both matter for the
// reason the operator-door test's own doc records: the fix's first pass was
// written against one author (the Edge builders) while a second author (the
// operator door) kept the whole bug. A fence that walks builders and a test
// that drives the door together say the plan that reached the DATABASE is the
// plan the fence approved.
//
// steps_json is the view that counts: slotNeeds reads the persisted steps on
// every scanner replay, not the struct the handler built.

// submitDirectTwoRobotSwap drives apiDirectComplexOrderSubmit in two_robot
// mode and returns the two created orders.
func submitDirectTwoRobotSwap(t *testing.T, h *Handlers, location, staging, source, destination string) int {
	t.Helper()
	rec := postJSON(t, h.apiDirectComplexOrderSubmit, "/api/test-orders/direct/complex",
		map[string]any{
			"cycle_mode":           "two_robot",
			"location":             location,
			"inbound_staging":      staging,
			"inbound_source":       source,
			"outbound_destination": destination,
			"payload_code":         "PART-A",
			"priority":             1,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Orders []struct {
			OrderUUID string `json:"order_uuid"`
		} `json:"orders"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	return len(resp.Orders)
}

// TestDirectSwap_ResupplyLegDeclaresItsStagingDropoff is the persisted-steps
// pin on the direct route: the two-robot resupply leg parks a bin at the
// staging node, and that dropoff must be declared exclusive in steps_json or
// nothing reserves the node against the next order.
func TestDirectSwap_ResupplyLegDeclaresItsStagingDropoff(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	staging := &nodes.Node{Name: "STAGE-DIRECT-SWAP", Enabled: true}
	if err := db.CreateNode(staging); err != nil {
		t.Fatalf("create staging node: %v", err)
	}

	if n := submitDirectTwoRobotSwap(t, h, sd.LineNode.Name, staging.Name, sd.StorageNode.Name, sd.StorageNode.Name); n != 2 {
		t.Fatalf("two_robot produced %d orders, want 2 (supply + removal)", n)
	}

	rows, err := db.ListOrdersByStation("core-direct", 10)
	if err != nil {
		t.Fatalf("list direct orders: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d orders, want 2", len(rows))
	}

	var steps []struct {
		Action        string `json:"action"`
		Node          string `json:"node"`
		ExclusiveSlot bool   `json:"exclusive_slot"`
	}
	// The resupply leg is the one whose plan names the staging node. Walk both
	// orders and pin the FIRST dropoff at the staging node that is found —
	// there is exactly one, on exactly one leg.
	sawStaging := false
	for _, o := range rows {
		steps = steps[:0]
		if err := json.Unmarshal([]byte(o.StepsJSON), &steps); err != nil {
			t.Fatalf("unmarshal steps_json for order %d: %v", o.ID, err)
		}
		for i, s := range steps {
			if s.Action != "dropoff" || s.Node != staging.Name {
				continue
			}
			sawStaging = true
			if !s.ExclusiveSlot {
				t.Errorf("order %d step %d: the staging dropoff at %s is not declared exclusive in "+
					"steps_json. slotNeeds reads THESE bytes on every scanner replay, so undeclared "+
					"means the node is reserved by nothing — a second order takes it and the first "+
					"robot arrives to a full node holding a bin", o.ID, i, staging.Name)
			}
		}
		// AND THE CELL DROPOFF ON THE SAME LEG MUST NOT BE DECLARED — the
		// location field is the cell the swap happens at; gating it re-creates
		// the 2b05dce deadlock.
		for i, s := range steps {
			if s.Action == "dropoff" && s.Node == sd.LineNode.Name && s.ExclusiveSlot {
				t.Errorf("order %d step %d: the CELL dropoff at %s is declared exclusive — gating a "+
					"line/cell dropoff re-creates the deadlock Core's 2b05dce fixed", o.ID, i, s.Node)
			}
		}
	}
	if !sawStaging {
		t.Fatal("no dropoff at the staging node in either leg's steps_json — the fixture stopped " +
			"exercising the staging dropoff and this test is vacuous")
	}
}
