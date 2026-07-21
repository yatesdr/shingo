//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// gateChoreoLane builds a gate_choreography group + lane with a configured wait
// point, and returns the lane id plus its shallow (depth-0) and deep (depth-1)
// slots.
func gateChoreoLane(t *testing.T, db *store.DB, name, gatePoint string) (laneID int64, s0, s1 *nodes.Node) {
	t.Helper()
	_, laneID, s0 = gatedLane(t, db, name, string(LaneEnforceGateChoreography))
	if gatePoint != "" {
		if err := db.SetNodeProperty(laneID, PropLaneGatePoint, gatePoint); err != nil {
			t.Fatalf("set gate point: %v", err)
		}
	}
	slots, err := db.ListLaneSlots(laneID)
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	for _, s := range slots {
		if dpt, _ := db.GetSlotDepth(s.ID); dpt == 1 {
			s1 = s
		}
	}
	if s1 == nil {
		t.Fatal("fixture should have a depth-1 slot")
	}
	return laneID, s0, s1
}

// TestGateChoreo_OpenValveCreatesUnsealedThenAppends is the uniform-shape gate.
//
// ⚖ The ruling is that EVERY lane-bound order on a gate_choreography group ships
// unsealed ending at the wait point — there is NO bypass class for the
// uncontended case. So even with the lane completely clear, the create must be
// Complete:false with a Wait block and NO dropoff, and the dropoff must arrive as
// a separate sealing append. That pair is the whole assertion: it is what proves
// the open and contended paths are one code path rather than two.
func TestGateChoreo_OpenValveCreatesUnsealedThenAppends(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	_, s0, _ := gateChoreoLane(t, db, "GCOPEN", "GCOPEN-WAIT")
	line := lineNode(t, db, "GCOPEN-LINE")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s0.Name
		o.Status = "sourcing"
	})

	vendorID, err := d.DispatchDirect(order, line, s0)
	if err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}

	creates := backend.CreateRequests()
	if len(creates) != 1 {
		t.Fatalf("create calls = %d, want 1", len(creates))
	}
	c := creates[0]
	if c.Complete {
		t.Error("a lane-bound order must be created UNSEALED (Complete=false) even when the lane is clear — no bypass class")
	}
	if len(c.Blocks) != 2 {
		t.Fatalf("create blocks = %d (%v), want 2: [pickup@source, wait@gate]", len(c.Blocks), c.Blocks)
	}
	if c.Blocks[0].Location != line.Name || c.Blocks[1].Location != "GCOPEN-WAIT" {
		t.Errorf("create blocks = [%s, %s], want [%s, GCOPEN-WAIT]", c.Blocks[0].Location, c.Blocks[1].Location, line.Name)
	}
	if c.Blocks[1].BinTask != "Wait" {
		t.Errorf("second create block binTask = %q, want Wait", c.Blocks[1].BinTask)
	}
	for _, b := range c.Blocks {
		if b.Location == s0.Name {
			t.Error("the create must NOT contain the dropoff — the slot is bound at append time")
		}
	}

	// The valve was open, so the tail went out immediately, in the same call.
	appends := backend.ReleaseCalls()
	if len(appends) != 1 {
		t.Fatalf("append calls = %d, want 1 (an open valve appends immediately)", len(appends))
	}
	a := appends[0]
	if a.VendorOrderID != vendorID {
		t.Errorf("append targeted %q, want the created order %q", a.VendorOrderID, vendorID)
	}
	if !a.Complete {
		t.Error("the tail append must SEAL the order (complete=true)")
	}
	if len(a.Blocks) != 1 || a.Blocks[0].Location != s0.Name {
		t.Fatalf("append blocks = %v, want exactly [dropoff@%s]", a.Blocks, s0.Name)
	}
	// Block ids must continue the create's numbering, not restart — a duplicate id
	// is the one thing SEER rejects outright.
	if a.Blocks[0].BlockID == c.Blocks[0].BlockID || a.Blocks[0].BlockID == c.Blocks[1].BlockID {
		t.Errorf("appended block id %q collides with a create block id", a.Blocks[0].BlockID)
	}

	// Durable truth on the order row: the tail landed, so wait_index advanced and
	// the order is no longer gate-staged.
	reloaded, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.WaitIndex != 1 {
		t.Errorf("wait_index = %d, want 1 after the tail sealed the order", reloaded.WaitIndex)
	}
	if IsGateStaged(reloaded) {
		t.Error("an order whose valve opened must not read as gate-staged")
	}
	if reloaded.StepsJSON == "" {
		t.Fatal("the gated plan must be persisted for restart recovery")
	}
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(reloaded.StepsJSON), &steps); err != nil {
		t.Fatalf("stored plan is not parseable: %v", err)
	}
	if len(steps) != 3 || steps[1].Action != protocol.ActionWait {
		t.Errorf("stored plan = %+v, want [pickup, wait, dropoff]", steps)
	}
	if reloaded.Coordinated {
		t.Error("a plain gated order must stay NOT coordinated — steps_json presence must not flip provenance")
	}
}

// TestGateChoreo_ContendedCreatesUnsealedAndHolds: same create, no tail. The
// order is left gate-staged for the release evaluator (increment 4).
func TestGateChoreo_ContendedCreatesUnsealedAndHolds(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	_, s0, s1 := gateChoreoLane(t, db, "GCHOLD", "GCHOLD-WAIT")
	line := lineNode(t, db, "GCHOLD-LINE")

	// A deeper cross-origin store is in flight and has not placed: Tier 2 parks
	// the shallow entrant, so its valve is CLOSED.
	deeper := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s1.Name
		o.Status = "in_transit"
	})
	if adm, _, _, err := d.AcquireLanesForOrder(deeper.ID, line, s1); err != nil || !adm {
		t.Fatalf("deeper store must take its inbound mouth row: adm=%v err=%v", adm, err)
	}
	if err := db.UpdateOrderVendor(deeper.ID, "sg-gchold-deep", "RUNNING", ""); err != nil {
		t.Fatalf("update deeper vendor: %v", err)
	}

	shallow := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s0.Name
		o.Status = "sourcing"
	})
	if _, err := d.DispatchDirect(shallow, line, s0); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}

	creates := backend.CreateRequests()
	if len(creates) != 1 || creates[0].Complete {
		t.Fatalf("want exactly 1 UNSEALED create, got %d (complete=%v)", len(creates), len(creates) > 0 && creates[0].Complete)
	}
	if n := len(backend.ReleaseCalls()); n != 0 {
		t.Fatalf("append calls = %d, want 0 — a contended order must hold its tail", n)
	}

	reloaded, err := db.GetOrder(shallow.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.WaitIndex != 0 {
		t.Errorf("wait_index = %d, want 0 — no tail was appended", reloaded.WaitIndex)
	}
	if !IsGateStaged(reloaded) {
		t.Errorf("a contended gated order must read as gate-staged (steps=%q wait=%d vendor=%q coordinated=%v)",
			reloaded.StepsJSON, reloaded.WaitIndex, reloaded.VendorOrderID, reloaded.Coordinated)
	}
}

// TestGateChoreo_MissingGatePointIsAnError: a group configured for the arm whose
// lane has no wait point is a MISCONFIGURATION and must fail loudly. Falling back
// to the sealed shape would silently recreate the bypass class the uniform ruling
// exists to forbid — on the one lane an operator explicitly asked to be gated.
func TestGateChoreo_MissingGatePointIsAnError(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	_, s0, _ := gateChoreoLane(t, db, "GCNOPT", "") // no gate point configured
	line := lineNode(t, db, "GCNOPT-LINE")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s0.Name
		o.Status = "sourcing"
	})

	if _, err := d.DispatchDirect(order, line, s0); err == nil {
		t.Fatal("a gate_choreography lane with no wait point must fail dispatch, not silently ship the sealed shape")
	}
	if n := len(backend.CreateRequests()); n != 0 {
		t.Errorf("create calls = %d, want 0 — nothing should reach the fleet on a misconfigured lane", n)
	}
}

// TestGateChoreo_NonGatedLaneIsUnchanged: the arm is opt-in per group. A mouth
// (or unset) group still takes the sealed single-shot create with no wait block.
func TestGateChoreo_NonGatedLaneIsUnchanged(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	for _, mode := range []string{"", "mouth", "delegated"} {
		name := "GCOFF-" + mode
		if mode == "" {
			name = "GCOFF-none"
		}
		_, _, s0 := gatedLane(t, db, name, mode)
		line := lineNode(t, db, name+"-LINE")
		order := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.DeliveryNode = s0.Name
			o.Status = "sourcing"
		})
		if _, err := d.DispatchDirect(order, line, s0); err != nil {
			t.Fatalf("[%s] DispatchDirect: %v", name, err)
		}
	}

	for i, c := range backend.CreateRequests() {
		if !c.Complete {
			t.Errorf("create %d: non-gate-choreography lane must stay SEALED (Complete=true)", i)
		}
		if len(c.Blocks) != 2 {
			t.Errorf("create %d: blocks = %d, want the unchanged 2-block shape", i, len(c.Blocks))
		}
		for _, b := range c.Blocks {
			if b.BinTask == "Wait" {
				t.Errorf("create %d: non-gated lane must emit no Wait block", i)
			}
		}
	}
	if n := len(backend.ReleaseCalls()); n != 0 {
		t.Errorf("append calls = %d, want 0 for non-gated lanes", n)
	}
}
