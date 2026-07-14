//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// mkSwapLeg creates a two-robot swap leg with the given uuid, sibling pointer,
// status, and node shape, in the classic two_robot shapes: the supply leg
// delivers TO the line (delivery == line); the evac leg delivers AWAY.
//
// The legs carry STEPS, because that is what decides a leg's role now
// (legTakesLineBin). They used to be step-less and encode role purely in
// delivery_node vs process_node, which is the inference that mis-read a
// press-index R2 — see TestSwapPeerTerminal_PressIndexR2Skipped_CancelsEvac.
func mkSwapLeg(t *testing.T, db *store.DB, uuid, sibUUID string, status protocol.Status, lineName, deliveryName, payload string) *orders.Order {
	t.Helper()
	steps := []resolvedStep{ // evac: lift the line's bin, carry it away
		{Action: protocol.ActionWait, Node: lineName},
		{Action: protocol.ActionPickup, Node: lineName},
		{Action: protocol.ActionDropoff, Node: deliveryName},
	}
	if deliveryName == lineName { // supply: fetch a fresh bin, set it on the line
		steps = []resolvedStep{
			{Action: protocol.ActionPickup, Node: "SWAP-SRC"},
			{Action: protocol.ActionDropoff, Node: lineName},
		}
	}
	return mkSwapLegWithSteps(t, db, uuid, sibUUID, status, lineName, deliveryName, payload, steps)
}

// mkSwapLegWithSteps is mkSwapLeg for shapes the supply/evac dichotomy can't
// express — notably a press-index R2, which sets a bin on the line and then
// carries on to re-index, so it is the SUPPLY while ending away from the line.
func mkSwapLegWithSteps(t *testing.T, db *store.DB, uuid, sibUUID string, status protocol.Status, lineName, deliveryName, payload string, steps []resolvedStep) *orders.Order {
	t.Helper()
	j, err := json.Marshal(steps)
	testutil.MustNoErr(t, err, "marshal steps "+uuid)
	o := &orders.Order{
		EdgeUUID: uuid, StationID: "ST", OrderType: OrderTypeComplex, Status: status,
		Quantity: 1, PayloadCode: payload,
		ProcessNode: lineName, DeliveryNode: deliveryName, SiblingOrderUUID: sibUUID,
		StepsJSON: string(j),
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create swap leg "+uuid)
	return o
}

func hasSwapHalfAudit(t *testing.T, db *store.DB, orderID int64) bool {
	t.Helper()
	entries, err := db.ListEntityAudit("order", orderID)
	testutil.MustNoErr(t, err, "list audit")
	for _, e := range entries {
		if e.Action == "swap_half_completed" {
			return true
		}
	}
	return false
}

// TestSwapPeerTerminal_SupplyFails_CancelsLiveEvac: the supply dies while the
// evac is still in flight → the evac is cancelled so it can't pull the line's
// resident with no replacement coming (ALN_003 post-dispatch strand).
func TestSwapPeerTerminal_SupplyFails_CancelsLiveEvac(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	store1 := &nodes.Node{Name: "SWAP-STORE-1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(store1), "create store node")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	mkSwapLeg(t, db, "sup1", "evac1", StatusFailed, lineNode.Name, lineNode.Name, bp.Code)
	supply, _ := db.GetOrderByUUID("sup1")
	mkSwapLeg(t, db, "evac1", "sup1", protocol.StatusInTransit, lineNode.Name, store1.Name, bp.Code)

	d.HandleSwapPeerTerminal(supply.ID, SwapTerminalFailed)

	got, _ := db.GetOrderByUUID("evac1")
	if got.Status != StatusCancelled {
		t.Fatalf("evac status = %q, want cancelled (line must keep its bin)", got.Status)
	}
}

// TestSwapPeerTerminal_EvacFails_CancelsLiveSupply: the evac dies while the
// supply is still in flight → the supply is cancelled so it can't drop a second
// bin onto the still-occupied line (collision).
func TestSwapPeerTerminal_EvacFails_CancelsLiveSupply(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	store1 := &nodes.Node{Name: "SWAP-STORE-2", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(store1), "create store node")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	mkSwapLeg(t, db, "sup2", "evac2", protocol.StatusInTransit, lineNode.Name, lineNode.Name, bp.Code)
	mkSwapLeg(t, db, "evac2", "sup2", StatusFailed, lineNode.Name, store1.Name, bp.Code)
	evac, _ := db.GetOrderByUUID("evac2")

	d.HandleSwapPeerTerminal(evac.ID, SwapTerminalFailed)

	got, _ := db.GetOrderByUUID("sup2")
	if got.Status != StatusCancelled {
		t.Fatalf("supply status = %q, want cancelled (no drop onto an un-cleared line)", got.Status)
	}
}

// TestSwapPeerTerminal_SupplyFails_EvacDelivered_Surfaces: the worst case — the
// evac already DELIVERED (line's resident removed) and the supply then dies. The
// evac is terminal-success so it can't be cancelled; the handler surfaces the
// half-swap (audit) instead of silently stranding the line, and does NOT
// re-create anything (re-issue is operator-driven).
func TestSwapPeerTerminal_SupplyFails_EvacDelivered_Surfaces(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	store1 := &nodes.Node{Name: "SWAP-STORE-3", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(store1), "create store node")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	mkSwapLeg(t, db, "sup3", "evac3", StatusFailed, lineNode.Name, lineNode.Name, bp.Code)
	supply, _ := db.GetOrderByUUID("sup3")
	mkSwapLeg(t, db, "evac3", "sup3", protocol.StatusConfirmed, lineNode.Name, store1.Name, bp.Code)

	d.HandleSwapPeerTerminal(supply.ID, SwapTerminalFailed)

	got, _ := db.GetOrderByUUID("evac3")
	if got.Status != protocol.StatusConfirmed {
		t.Fatalf("delivered evac status = %q, want confirmed (must not be mutated)", got.Status)
	}
	if !hasSwapHalfAudit(t, db, got.ID) {
		t.Fatal("expected a swap_half_completed audit surface (line not silently stranded)")
	}
}

// TestSwapPeerTerminal_DoubleTerminal_NoAction: both legs terminal (near-
// simultaneous double-death) → the IsTerminal guard means no cancel and no
// surface (nothing half-done to unwind).
func TestSwapPeerTerminal_DoubleTerminal_NoAction(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	store1 := &nodes.Node{Name: "SWAP-STORE-4", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(store1), "create store node")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	mkSwapLeg(t, db, "sup4", "evac4", StatusFailed, lineNode.Name, lineNode.Name, bp.Code)
	supply, _ := db.GetOrderByUUID("sup4")
	mkSwapLeg(t, db, "evac4", "sup4", StatusFailed, lineNode.Name, store1.Name, bp.Code)

	d.HandleSwapPeerTerminal(supply.ID, SwapTerminalFailed)

	got, _ := db.GetOrderByUUID("evac4")
	if got.Status != StatusFailed {
		t.Fatalf("evac status = %q, want failed (unchanged — no action on a dead peer)", got.Status)
	}
	if hasSwapHalfAudit(t, db, got.ID) {
		t.Fatal("must NOT surface a half-swap when the peer also failed (clean double-abort)")
	}
}

// TestSwapPeerTerminal_EvacSkipped_LeavesSupply: a SKIPPED (moot) evac means the
// line's resident was already gone, so the supply legitimately proceeds — the
// handler must NOT cancel it.
func TestSwapPeerTerminal_EvacSkipped_LeavesSupply(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	store1 := &nodes.Node{Name: "SWAP-STORE-5", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(store1), "create store node")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	mkSwapLeg(t, db, "sup5", "evac5", protocol.StatusInTransit, lineNode.Name, lineNode.Name, bp.Code)
	mkSwapLeg(t, db, "evac5", "sup5", StatusSkipped, lineNode.Name, store1.Name, bp.Code)
	evac, _ := db.GetOrderByUUID("evac5")

	d.HandleSwapPeerTerminal(evac.ID, SwapTerminalSkipped)

	got, _ := db.GetOrderByUUID("sup5")
	if got.Status != protocol.StatusInTransit {
		t.Fatalf("supply status = %q, want in_transit (a moot evac must not cancel the supply)", got.Status)
	}
}

// TestSwapPeerTerminal_PressIndexR2Skipped_CancelsEvac is the latent strand the
// old geometry discriminator hid. A 3-position press-index R2 is the SUPPLY — it
// sets the fresh bin on the press — but it then carries on to re-index C into B,
// so it ENDS at the index node, away from the line. `DeliveryNode != ProcessNode`
// therefore called it an evac, and a SKIPPED R2 took the moot-evac branch and
// returned as a silent no-op. The real evac (R1) then proceeded, pulled the
// press's bin, and no replacement was coming: the line strands.
//
// A skipped supply is a lost replacement. The evac must be cancelled.
func TestSwapPeerTerminal_PressIndexR2Skipped_CancelsEvac(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, press, bp := setupTestData(t, db)
	market := &nodes.Node{Name: "SWAP-STORE-6", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(market), "create market node")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// R1 — the real evac: lifts the press bin, then stages a fresh carrier at C.
	mkSwapLegWithSteps(t, db, "pi-r1", "pi-r2", protocol.StatusInTransit, press.Name, "INDEX-C", bp.Code,
		[]resolvedStep{
			{Action: protocol.ActionWait, Node: press.Name},
			{Action: protocol.ActionPickup, Node: press.Name},
			{Action: protocol.ActionDropoff, Node: market.Name},
			{Action: protocol.ActionPickup, Node: market.Name},
			{Action: protocol.ActionDropoff, Node: "INDEX-C"},
		})

	// R2 — the real supply: drops a bin ON the press, then re-indexes C into B.
	// Its final dropoff is INDEX-B, which is what made it look like an evac.
	r2 := mkSwapLegWithSteps(t, db, "pi-r2", "pi-r1", StatusSkipped, press.Name, "INDEX-B", bp.Code,
		[]resolvedStep{
			{Action: protocol.ActionWait, Node: "INDEX-B"},
			{Action: protocol.ActionPickup, Node: "INDEX-B"},
			{Action: protocol.ActionDropoff, Node: press.Name},
			{Action: protocol.ActionPickup, Node: "INDEX-C"},
			{Action: protocol.ActionDropoff, Node: "INDEX-B"},
		})

	d.HandleSwapPeerTerminal(r2.ID, SwapTerminalSkipped)

	got, _ := db.GetOrderByUUID("pi-r1")
	if got.Status != StatusCancelled {
		t.Fatalf("R1 (evac) status = %q, want cancelled — R2 IS the supply leg (it drops a bin on the press); "+
			"a skipped supply means no replacement is coming, so the evac must not pull the press's bin", got.Status)
	}
}
