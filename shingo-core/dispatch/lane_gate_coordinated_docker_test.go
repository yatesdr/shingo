//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// lane_gate_coordinated_docker_test.go — the other side of the same valve.
//
// The retrieve branch of dispatchToFleetCore is guarded by !order.Coordinated. The
// store branch two lines above it is not, and resolveLaneGateTarget keys only on
// the destination node's lane group — nothing about the order. So a coordinated
// order that reaches the plain dispatch path with a lane-slot destination takes
// dispatchGated, which OVERWRITES steps_json with a three-step gated plan and
// destroys the Edge-authored choreography.
//
// It is safe today only because coordinated orders do not normally take that path,
// and the thing asserting that is AssertSimpleNotCoordinated — which only logs. A
// tripwire, not a guard.
//
// ── The live way in, and the ruling ───────────────────────────────────────
// HandleOrderRedirect → PrepareRedirect (cancels the vendor order, rewrites
// delivery_node, moves to sourcing — no coordinated check) → dispatchToFleet.
//
// THE FIX IS TO REFUSE THE REDIRECT, not merely to guard the valve, and the
// reason is that the guard alone silently picks the worse of two failures:
//
//   - guard only → the redirect falls through to UNGATED dispatch, which puts a
//     robot into a gate_choreography lane with no gate at all. Mixed ungated
//     traffic in a single-file lane is precisely what the gate exists to prevent.
//   - refuse → the order keeps its plan and its choreography, and the operator
//     gets told no.
//
// And the damage is not gate-specific. dispatchToFleetCore builds a two-block
// transport from the source/delivery COLUMNS; a coordinated order sent through it
// loses its step plan whether or not a lane is involved. The gated valve only adds
// the steps_json overwrite on top. Refusing fixes both; guarding the valve fixes
// neither.
//
// Refused at CORE, not at Edge's API: whether a destination is a gated lane is
// Core's configuration, and asking Edge to know it would put the same decision in
// two places. Blast radius is small — there is no operator-board affordance for
// redirect at all, only POST /api/orders/{id}/redirect with no UI caller and the
// manual-message diagnostic page.
//
// The valve guard lands anyway, as symmetry with the retrieve branch and so the
// valve's precondition is stated where the valve is rather than only upstream.

// TestRedirect_CoordinatedOrderIsRefused is the red. The assertion is on the PLAN,
// because that is the thing destroyed — a status check would pass for several
// unrelated reasons.
//
// Vacuity: asserting "steps_json is unchanged" would also pass if the redirect
// never reached dispatch — a bad node name, a failed lookup. So the test first
// redirects the same order to an ORDINARY node and asserts that path still runs
// (before the fix) / is refused for the same reason (after), and pins the gated
// destination as genuinely gated by dispatching a plain order into it.
//
// MUTATIONS (both verified, and the pair is the argument for the ruling):
//
//   - remove the refusal, KEEP the valve guard → the "plan survived" assertion
//     passes (the guard does its job) and the delivery_node assertion fires
//     instead. That is the fall-through outcome in the flesh: the order keeps its
//     plan, is re-pointed at a gated lane slot, and is dispatched UNGATED into a
//     gate_choreography lane. The guard alone does not fix this; it changes which
//     way it breaks, from a destroyed plan to an ungated robot in a gated lane.
//   - remove both → the "plan survived" assertion fires, with
//     [pickup, wait@GCREDIR-WAIT, dropoff@GCREDIR-S1] in place of the original.
//     That was the state of the tree before this commit.
func TestRedirect_CoordinatedOrderIsRefused(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "REDIR-BIN")
	_, _, s1 := gateChoreoLane(t, db, "GCREDIR", "GCREDIR-WAIT")

	env := testEnvelope()
	o := submitComplexAndDispatch(t, d, db, env, &protocol.ComplexOrderRequest{
		OrderUUID:   "redirect-coordinated",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "wait"},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})
	if o.VendorOrderID == "" {
		t.Fatalf("complex order was not dispatched (status %s)", o.Status)
	}
	if !o.Coordinated {
		t.Fatal("fixture is not coordinated — this test is about the coordinated branch")
	}
	planBefore := o.StepsJSON
	if planBefore == "" {
		t.Fatal("fixture has no step plan, so there is nothing for the redirect to destroy")
	}

	// Redirect it into a slot of a gate_choreography lane.
	d.HandleOrderRedirect(env, &protocol.OrderRedirect{
		OrderUUID:       o.EdgeUUID,
		NewDeliveryNode: s1.Name,
	})

	after, err := db.GetOrder(o.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.StepsJSON != planBefore {
		t.Fatalf("the redirect overwrote a coordinated order's plan.\n before: %s\n  after: %s\n"+
			"dispatchGated rewrites steps_json with [pickup, wait@gate, dropoff]; the Edge-authored "+
			"choreography — and every wait in it — is gone", planBefore, after.StepsJSON)
	}
	if after.DeliveryNode == s1.Name {
		t.Errorf("delivery_node was rewritten to %s; a refused redirect must not half-apply", s1.Name)
	}

	// Non-vacuity: the lane really is gated, so the arm above was reachable. A plain
	// order into the same slot must take the valve and ship UNSEALED.
	plain := testdb.CreateOrder(t, db, func(po *orders.Order) {
		po.DeliveryNode = s1.Name
		po.Status = "sourcing"
	})
	before := len(backend.CreateRequests())
	if _, err := d.DispatchDirect(plain, lineNode, s1); err != nil {
		t.Fatalf("plain dispatch into the gated lane: %v", err)
	}
	creates := backend.CreateRequests()
	if len(creates) != before+1 {
		t.Fatalf("plain dispatch made %d creates, want 1", len(creates)-before)
	}
	if creates[len(creates)-1].Complete {
		t.Error("the control order was created SEALED — the lane is not actually gated, so the " +
			"coordinated assertion above proves nothing")
	}
}
