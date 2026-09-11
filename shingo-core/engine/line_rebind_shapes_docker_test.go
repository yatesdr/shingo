//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// line_rebind_shapes_docker_test.go — census 25 and 32: the line rebind on the
// two OTHER shapes that set a carrier onto their own process node mid-order.
//
// single_robot_line_rebind_docker_test.go pins the rebind for BuildSingleSwapSteps'
// shape, where the order carries the fresh carrier from its own source and
// re-collects it from its own staging. Two more plans place at the line and then
// carry on, and neither had been driven:
//
//   - the swap leg of a single-robot CHANGEOVER pair (buildSingleRobotChangeoverSwap),
//     which collects a carrier its PARTNER — the stage leg — delivered;
//   - a 3-position press-index R2, which sets the on-deck carrier onto the press
//     and then re-indexes the rear one.

func rebindNode(t *testing.T, name string, create func(*nodes.Node) error) *nodes.Node {
	t.Helper()
	n := &nodes.Node{Name: name, Enabled: true}
	testutil.MustNoErr(t, create(n), "create node "+name)
	return n
}

// TestSingleRobotChangeover_SwapLegRebindsTheCarrierItsPartnerStaged is census 25.
//
// The stage leg has delivered the new style's carrier to inbound staging and
// finished. The swap leg goes to Core unpaired (census 24), so it sources only
// once that carrier is standing there, and it holds what a real one holds: the
// old carrier on the line AND the new one at staging. It lifts the old carrier,
// parks it at outbound staging, collects the staged one and sets it on the line.
// The line must end up recorded holding the NEW carrier, and the rebind the Edge
// binds its counter to must name it — not the old one parked a step earlier.
//
// COVERAGE PIN. Passes at bcbde0d2 once the swap leg holds its real claims:
// resolvePickupBin finds the new carrier through the order_bins row
// confirmComplexPlan wrote for the staging pickup. MUTATION: make
// resolvePickupBin answer from the order's bin_id first — the staging pickup then
// moves the old carrier, and the rebind names it.
//
// The first RED run had this as a defect pin, and the fixture was wrong, not the
// code: it seeded the swap leg holding only the old carrier, which no real swap
// leg does. The reserve walks every pickup, post-wait ones included, so a swap
// leg that dispatches has claimed the staged carrier. What kept a real swap leg
// from ever getting that far at bcbde0d2 was census 24.
func TestSingleRobotChangeover_SwapLegRebindsTheCarrierItsPartnerStaged(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())
	line := sd.LineNode
	inStage := rebindNode(t, "SRC25-IN-STAGE", db.CreateNode)
	outStage := rebindNode(t, "SRC25-OUT-STAGE", db.CreateNode)
	out := rebindNode(t, "SRC25-OUT", db.CreateNode)

	// The stage leg is done: its carrier stands at inbound staging, held by nobody.
	fresh := testdb.CreateBinAtNode(t, db, sd.Payload.Code, inStage.ID, "SRC25-NEW")
	spent := testdb.CreateBinAtNode(t, db, sd.Payload.Code, line.ID, "SRC25-OLD")

	// The swap leg, unpaired, sourced through the real reserve.
	d := eng.Dispatcher()
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "src25-swap", PayloadCode: sd.Payload.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionWait, Node: line.Name},
			{Action: protocol.ActionPickup, Node: line.Name},
			{Action: protocol.ActionDropoff, Node: outStage.Name, ExclusiveSlot: true},
			{Action: protocol.ActionPickup, Node: inStage.Name},
			{Action: protocol.ActionDropoff, Node: line.Name},
			{Action: protocol.ActionPickup, Node: outStage.Name},
			{Action: protocol.ActionDropoff, Node: out.Name},
		},
	})
	swap, err := db.GetOrderByUUID("src25-swap")
	testutil.MustNoErr(t, err, "load the swap leg")
	if swap == nil {
		t.Fatal("intake refused the swap leg")
	}
	_ = d.DispatchPreparedComplex(swap)
	claimed, err := db.ListBinsByClaim(swap.ID)
	testutil.MustNoErr(t, err, "list the swap leg's claims")
	if len(claimed) != 2 {
		t.Fatalf("fixture: the swap leg holds %d bin(s) after dispatch, want the old and the new carrier", len(claimed))
	}

	// Past its wait, the robot works the plan: lift the old carrier, park it,
	// collect the staged one, set it on the line.
	testutil.MustNoErr(t, db.UpdateOrderStatus(swap.ID, string(dispatch.StatusInTransit), "released"), "release")
	eng.handlePickupBlockCompleted(BlockCompletedEvent{
		OrderID: swap.ID, BlockID: "src25-b1", Location: line.Name, BinTask: "JackLoad",
	})
	eng.handleStoreBlockCompleted(BlockCompletedEvent{
		OrderID: swap.ID, BlockID: "src25-b2", Location: outStage.Name, BinTask: "JackUnload",
	})
	eng.handlePickupBlockCompleted(BlockCompletedEvent{
		OrderID: swap.ID, BlockID: "src25-b3", Location: inStage.Name, BinTask: "JackLoad",
	})
	eng.handleStoreBlockCompleted(BlockCompletedEvent{
		OrderID: swap.ID, BlockID: "src25-b4", Location: line.Name, BinTask: "JackUnload",
	})

	got, err := db.GetBin(fresh.ID)
	testutil.MustNoErr(t, err, "reload the new carrier")
	if got.NodeID == nil || *got.NodeID != line.ID {
		t.Errorf("the NEW carrier is recorded at node %v, want the line (%d) — the robot set it there", got.NodeID, line.ID)
	}
	old, err := db.GetBin(spent.ID)
	testutil.MustNoErr(t, err, "reload the old carrier")
	if old.NodeID == nil || *old.NodeID != outStage.ID {
		t.Errorf("the OLD carrier is recorded at node %v, want outbound staging (%d) — it was parked there "+
			"at step 3 and nothing has moved it since", old.NodeID, outStage.ID)
	}
	ann := boundAnnouncementsFor(t, db, line.Name)
	if len(ann) == 0 {
		t.Fatalf("no Bound UOPAdjustment for %s after the swap leg set the new carrier on it", line.Name)
	}
	if ann[len(ann)-1].BinID != fresh.ID {
		t.Errorf("the rebind names bin %d, want the NEW carrier %d. The Edge binds the line's counter to "+
			"this bin; naming the old one charges the cell's ticks to a carrier on its way to the market",
			ann[len(ann)-1].BinID, fresh.ID)
	}
}

// TestPressIndex3_R2RebindsTheCarrierItSetOnThePress is census 32. A 3-position
// R2 lifts the middle (on-deck) carrier, sets it on the press, then lifts the
// rear carrier and puts it in the middle. The press dropoff is INTERMEDIATE, so
// the rebind rides handleStoreBlockCompleted, and it must name the carrier R2
// set on the press.
//
// COVERAGE PIN. Expected to pass at bcbde0d2: both carriers are R2's own claims,
// the middle one is the only claimed bin at _TRANSIT at the press dropoff, and
// resolveDropoffBin answers it. MUTATION: make resolvePickupBin return the
// order's bin_id first — the middle pickup then lifts the rear carrier, which
// bin_id names, and the press rebind names the wrong bin.
func TestPressIndex3_R2RebindsTheCarrierItSetOnThePress(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())
	press := sd.LineNode
	mid := rebindNode(t, "PI3-MID", db.CreateNode)
	rear := rebindNode(t, "PI3-REAR", db.CreateNode)

	onDeck := testdb.CreateBinAtNode(t, db, sd.Payload.Code, mid.ID, "PI3-ONDECK")
	rearBin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, rear.ID, "PI3-REAR-BIN")
	r2Steps := `[{"action":"wait","node":"` + mid.Name + `"},` +
		`{"action":"pickup","node":"` + mid.Name + `"},` +
		`{"action":"dropoff","node":"` + press.Name + `"},` +
		`{"action":"pickup","node":"` + rear.Name + `"},` +
		`{"action":"dropoff","node":"` + mid.Name + `"}]`
	r2 := &orders.Order{
		EdgeUUID: "pi3-r2", StationID: "line-1", OrderType: dispatch.OrderTypeComplex,
		Status: dispatch.StatusInTransit, Quantity: 1, PayloadCode: sd.Payload.Code,
		ProcessNode: press.Name, DeliveryNode: mid.Name, Coordinated: true, StepsJSON: r2Steps,
		// bin_id names the REAR carrier on purpose. R2 holds two carriers and has
		// one bin pointer, so its middle pickup must resolve by where it is, not by
		// the pointer — which is what the MUTATION above breaks.
		BinID: &rearBin.ID,
	}
	testutil.MustNoErr(t, db.CreateOrder(r2), "create R2")
	testdb.ClaimBinForTest(t, db, onDeck.ID, r2.ID)
	testdb.ClaimBinForTest(t, db, rearBin.ID, r2.ID)

	eng.handlePickupBlockCompleted(BlockCompletedEvent{
		OrderID: r2.ID, BlockID: "pi3-b2", Location: mid.Name, BinTask: "JackLoad",
	})
	eng.handleStoreBlockCompleted(BlockCompletedEvent{
		OrderID: r2.ID, BlockID: "pi3-b3", Location: press.Name, BinTask: "JackUnload",
	})

	testdb.RequireBinAtNode(t, db, onDeck.ID, press.ID)
	ann := boundAnnouncementsFor(t, db, press.Name)
	if len(ann) == 0 {
		t.Fatalf("no Bound UOPAdjustment for %s after R2 set the on-deck carrier on it", press.Name)
	}
	if ann[len(ann)-1].BinID != onDeck.ID {
		t.Errorf("the rebind names bin %d, want the carrier R2 set on the press (%d)", ann[len(ann)-1].BinID, onDeck.ID)
	}
}
