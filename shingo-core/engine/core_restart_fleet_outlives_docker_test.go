//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

// A CORE RESTART WITH AN ORDER IN FLIGHT, AGAINST A FLEET THAT OUTLIVES IT.
//
// At a plant the fleet (RDS) is its own process, and a Core restart leaves its
// missions running. The sim rig cannot show that: its fleet lives inside Core
// and dies with it. Here one simulator outlives the first engine: an order is
// dispatched and running, that engine stops, a second engine starts on the same
// database and the same fleet, and the mission finishes against the new Core.
// The restarted Core reloads the mission, the fleet still holds it, and the
// order lands its bin and confirms as if nothing had happened.
func TestCoreRestart_AnOrderInFlightFinishesAgainstTheFleetThatOutlivedIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, "PART-A", storageNode.ID, "RESTART-BIN")
	sim := &attachCountingFleet{SimulatorBackend: simulator.New()}

	first := newUnstartedEngine(t, db, sim)
	first.Start()
	env := testEnvelope()
	first.Dispatcher().HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID: "restart-move", Quantity: 1,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: storageNode.Name},
			{Action: protocol.ActionDropoff, Node: lineNode.Name},
		},
	})
	before := testdb.RequireOrder(t, db, "restart-move")
	if before.VendorOrderID == "" || before.BinID == nil || *before.BinID != bin.ID {
		t.Fatalf("fixture: the order was not dispatched with its bin: vendor %q bin %v", before.VendorOrderID, before.BinID)
	}
	sim.DriveState(before.VendorOrderID, "RUNNING")
	if o := testdb.RequireOrder(t, db, "restart-move"); o.Status != protocol.StatusInTransit {
		t.Fatalf("fixture: the order is %q before the restart, want in_transit", o.Status)
	}

	// Core goes down with the robot mid-mission; the fleet does not. The new
	// Core is handed the fleet that outlived the first: every check and every
	// drive below is through the fleet the new Core holds.
	first.Stop()
	fleetAfter := sim
	second := newTestEngine(t, db, fleetAfter)
	if !fleetAfter.HasOrder(before.VendorOrderID) {
		t.Fatalf("the fleet the new Core holds does not know mission %s", before.VendorOrderID)
	}
	if fleetAfter.attached != 2 {
		t.Fatalf("the fleet was attached %d time(s), want twice: the new Core did not take up the fleet that outlived the first", fleetAfter.attached)
	}
	if o := testdb.RequireOrder(t, db, "restart-move"); o.Status != protocol.StatusInTransit || o.BinID == nil || *o.BinID != bin.ID {
		t.Fatalf("after the restart the order is %q holding %v, want in_transit holding bin %d", o.Status, o.BinID, bin.ID)
	}

	// The mission finishes against the new Core: the fleet's FINISHED alone
	// carries the order to delivered, before any receipt.
	fleetAfter.SimulatorBackend.DriveState(before.VendorOrderID, "FINISHED")
	if o := testdb.RequireOrder(t, db, "restart-move"); o.Status != protocol.StatusDelivered {
		t.Fatalf("after the fleet finished the mission the order is %q, want delivered: the new Core did not hear the fleet", o.Status)
	}
	second.Dispatcher().HandleOrderReceipt(env, &protocol.OrderReceipt{OrderUUID: "restart-move", ReceiptType: "confirmed", FinalCount: 1})
	after := testdb.RequireOrder(t, db, "restart-move")
	if after.Status != protocol.StatusConfirmed {
		t.Errorf("after the restart and the fleet's finish the order is %q, want confirmed", after.Status)
	}
	testdb.RequireBinAtNode(t, db, bin.ID, lineNode.ID)
	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "bin")
	if got.ClaimedBy != nil {
		t.Errorf("the bin is still claimed by %d after its order confirmed", *got.ClaimedBy)
	}
}

// A CORE RESTART WITH A COMPOUND PARENT MID-DIG, the same fleet outliving it.
//
// The parent is `reshuffling`, its first leg is out with the blocker on the
// robot, and Core goes down. The new Core takes up the fleet and the dig: the
// leg finishes against it, the next leg goes, the parent finishes with its
// bin at the line, the lane is unlocked and no bin is left claimed.
func TestCoreRestart_ADigInFlightFinishesAgainstTheFleetThatOutlivedIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix: "RESTARTDIG", NumSlots: 3, TargetSlot: 2, TargetAge: 2 * time.Hour,
	})
	sim := &attachCountingFleet{SimulatorBackend: simulator.New()}

	first := newUnstartedEngine(t, db, sim)
	first.Start()
	env := testEnvelope()
	first.Dispatcher().HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID: "restart-dig", OrderType: dispatch.OrderTypeRetrieve, PayloadCode: sc.Payload.Code,
		SourceNode: sc.Grp.Name, DeliveryNode: sc.LineNode.Name, Quantity: 1,
	})
	parent := testdb.RequireOrderStatus(t, db, "restart-dig", dispatch.StatusReshuffling)
	children, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "the dig's legs")
	if len(children) < 2 {
		t.Fatalf("fixture: %d leg(s) under the dig, want 2 (unbury, retrieve)", len(children))
	}
	leg := children[0]
	if leg.VendorOrderID == "" {
		t.Fatalf("fixture: the first leg was not dispatched (%s)", leg.Status)
	}
	sim.DriveState(leg.VendorOrderID, "RUNNING")

	// Core goes down with the dig's first robot moving; the fleet does not.
	first.Stop()
	second := newTestEngine(t, db, sim)
	if sim.attached != 2 {
		t.Fatalf("the fleet was attached %d time(s), want twice: the new Core did not take up the fleet", sim.attached)
	}
	if !sim.HasOrder(leg.VendorOrderID) {
		t.Fatalf("the fleet the new Core holds does not know the leg's mission %s", leg.VendorOrderID)
	}
	if p := testdb.RequireOrder(t, db, "restart-dig"); p.Status != dispatch.StatusReshuffling {
		t.Fatalf("after the restart the parent is %q, want reshuffling", p.Status)
	}

	// The dig finishes against the new Core, leg by leg.
	for _, c := range children {
		c, err := db.GetOrder(c.ID)
		testutil.MustNoErr(t, err, "leg")
		if c.VendorOrderID == "" {
			t.Fatalf("leg %d (seq %d) was never dispatched after the restart (%s, %q)", c.ID, c.Sequence, c.Status, c.QueueReason)
		}
		driveLegRunToFinish(t, db, sim.SimulatorBackend, c.ID, c.VendorOrderID)
		second.Dispatcher().HandleOrderReceipt(env, &protocol.OrderReceipt{OrderUUID: c.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1})
	}

	testdb.RequireBinAtNode(t, db, sc.TargetBin.ID, sc.LineNode.ID)
	for _, id := range []int64{sc.TargetBin.ID, sc.Blockers[0].ID} {
		b, err := db.GetBin(id)
		testutil.MustNoErr(t, err, "bin")
		if b.ClaimedBy != nil {
			t.Errorf("bin %d is still claimed by %d after the dig", id, *b.ClaimedBy)
		}
	}
	if second.Dispatcher().LaneLock().IsLocked(sc.Lane.ID) {
		t.Errorf("lane %d is still locked after the dig finished against the new Core", sc.Lane.ID)
	}
	if p := testdb.RequireOrder(t, db, "restart-dig"); p.Status != protocol.StatusConfirmed {
		t.Errorf("the parent is %q (%s), want confirmed: its last leg carried its bin to the line", p.Status, p.ErrorDetail)
	}
}

// attachCountingFleet is the simulator, counting the engines that attach to it
// (InitTracker): a Core takes up a fleet by attaching its tracker, and a
// restarted Core attaches to the fleet that outlived the first.
type attachCountingFleet struct {
	*simulator.SimulatorBackend
	attached int
}

func (f *attachCountingFleet) InitTracker(emitter fleet.TrackerEmitter, resolver fleet.OrderIDResolver) {
	f.attached++
	f.SimulatorBackend.InitTracker(emitter, resolver)
}
