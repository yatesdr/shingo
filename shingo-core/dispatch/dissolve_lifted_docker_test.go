//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// dissolve_lifted_docker_test.go — what a dissolve may not take back.
//
// The specimen (lane-stress 2026-09-25, dig-starvation SYNTH #34): leg 212 lifted
// bin 59 at 08:05:27.630 and dig 211 was dissolved in the same second. The
// dissolve cancelled every non-terminal child, 212 with them, so a robot stood
// holding a bin that no order owned, and the stranded sweep declined it for its
// whole two-hour window. The same second shows the other half: DISSOLVING dig 211
// was decided twice, and the second pass was stopped only by a child's CAS.

// planDigToDissolve plans a dig over `blockers` blockers in a 4-deep lane with
// parking beside it, returning the parent, the lane, its slots and the legs in
// sequence order. Leg 1 is dispatched by the plan itself.
func planDigToDissolve(t *testing.T, db *store.DB, d *Dispatcher, prefix string, blockers int) (*orders.Order, *nodes.Node, []*nodes.Node, []*orders.Order) {
	t.Helper()
	_, lane, slots, bp := staleDigLane(t, db, prefix)
	for i := 0; i < blockers; i++ {
		testdb.CreateBinAtNode(t, db, bp.Code, slots[i].ID, prefix+"-BLK-"+slots[i].Name)
	}
	target := testdb.CreateBinAtNode(t, db, bp.Code, slots[blockers].ID, prefix+"-TGT")
	lineNode(t, db, prefix+"-LINE")
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = prefix + "-parent"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = prefix + "-LINE"
		o.Status = protocol.StatusPending
	})
	if _, pe := d.planner.planBuriedReshuffle(parent, &BuriedError{Bin: target, Slot: slots[blockers], LaneID: lane.ID}); pe != nil {
		t.Fatalf("fixture: the dig must plan, got %s: %s", pe.Code, pe.Detail)
	}
	legs := legsOf(t, db, parent.ID)
	if len(legs) != blockers+1 {
		t.Fatalf("fixture: the dig planned %d leg(s), want %d", len(legs), blockers+1)
	}
	if legs[0].VendorOrderID == "" {
		t.Fatalf("fixture: leg %d was never dispatched (queue_cause %q)", legs[0].ID, legs[0].QueueCause)
	}
	return parent, lane, slots, legs
}

// TestDissolve_NeverCancelsALiftedLeg: a dissolve that finds a leg carrying its
// bin leaves that leg alone. The leg is still placed through the dwell, the
// chapter stays open until it lands, and only then does the lane go and the
// demand return to the acquiring set.
//
// MUTATION: drop the lifted skip in dissolveCompound. Leg 1 is cancelled under
// the dissolve marker with its bin at _TRANSIT, which is the specimen, and the
// first assertion fires.
func TestDissolve_NeverCancelsALiftedLeg(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, lane, slots, legs := planDigToDissolve(t, db, d, "DS-LIFT", 1)

	transit, err := db.GetNodeByDotName("_TRANSIT")
	testutil.MustNoErr(t, err, "resolve _TRANSIT")
	liftBin(t, db, d, legs[0], slots[0], transit)

	testutil.MustNoErr(t, d.dissolveCompound(parent.ID, "test: the plan went stale mid-lift"), "the dissolve")

	lifted, err := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "reload the lifted leg")
	if protocol.IsTerminal(lifted.Status) {
		t.Fatalf("the lifted leg %d is %q (%s) — a robot is holding its bin; cancelling the only order "+
			"that owns it leaves the bin on the robot with nowhere to go", lifted.ID, lifted.Status, lifted.ErrorDetail)
	}
	blk, err := db.GetBin(*legs[0].BinID)
	testutil.MustNoErr(t, err, "reload the lifted bin")
	if blk.ClaimedBy == nil || *blk.ClaimedBy != lifted.ID {
		t.Errorf("the lifted bin is claimed by %v, want leg %d", blk.ClaimedBy, lifted.ID)
	}
	rest, err := db.GetOrder(legs[1].ID)
	testutil.MustNoErr(t, err, "reload leg 2")
	if rest.Status != protocol.StatusCancelled || rest.ErrorDetail != reshuffleDissolveDetail {
		t.Errorf("leg 2 is %q (%q), want cancelled under the dissolve marker — the rest of the plan "+
			"is still stale", rest.Status, rest.ErrorDetail)
	}

	// SOAKSTAT'S MID-LIFT RULE, over the fixture: no child cancelled under the
	// dissolve marker is carrying a lifted bin.
	for _, c := range legsOf(t, db, parent.ID) {
		if c.Status != protocol.StatusCancelled || c.ErrorDetail != reshuffleDissolveDetail || c.BinID == nil {
			continue
		}
		b, err := db.GetBin(*c.BinID)
		testutil.MustNoErr(t, err, "reload a cancelled leg's bin")
		if b.NodeID != nil && *b.NodeID == transit.ID {
			t.Errorf("mid-lift dissolve: leg %d was cancelled with its bin %d at _TRANSIT", c.ID, b.ID)
		}
	}

	// THE CHAPTER IS STILL OPEN AND THE LANE STILL HELD: a robot is in it.
	if !d.hasOpenDigChapter(parent.ID) {
		t.Error("hasOpenDigChapter is false with a leg still carrying its bin")
	}
	if !d.laneLock.IsLocked(lane.ID) {
		t.Fatal("the dissolve released the lane with a robot still in it carrying a blocker — the " +
			"re-burial window")
	}
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "the cancel wiring's re-drive")
	if !d.laneLock.IsLocked(lane.ID) {
		t.Fatal("the chapter-end arm released the lane before the kept leg landed")
	}

	// THE KEPT LEG IS PLACED THROUGH THE DWELL, LANDS, AND THEN THE CHAPTER CLOSES.
	landLeg(t, d, db, lifted)
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "the landing's re-drive")
	if d.laneLock.IsLocked(lane.ID) {
		t.Error("the lane is still held after the kept leg landed and the chapter closed")
	}
	after, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload the parent")
	if !protocol.IsAcquiring(after.Status) {
		t.Errorf("parent is %q after the dissolved chapter closed, want an acquiring status", after.Status)
	}
}

// secondPassEmitter runs onCancel once, on the first cancel it sees — where the
// engine's EventOrderCancelled wiring starts `go AdvanceCompoundOrder(parent)`
// while the first dissolve is still between its cancels.
type secondPassEmitter struct {
	mockEmitter
	onCancel func(orderID int64)
}

func (e *secondPassEmitter) EmitOrderCancelled(orderID int64, edgeUUID, stationID, reason, previousStatus string, recovers *int64) {
	e.mockEmitter.EmitOrderCancelled(orderID, edgeUUID, stationID, reason, previousStatus, recovers)
	if f := e.onCancel; f != nil {
		e.onCancel = nil
		f(orderID)
	}
}

// TestDissolve_ASecondPassDuringADissolveDecidesNothing: one dissolve is one
// decision. A pass that arrives while another is dissolving the same dig cancels
// nothing; the first pass finishes the job.
//
// MUTATION: drop the per-parent guard in dissolveCompound. The second pass
// re-reads the children, finds legs 2 and 3 still pending, and cancels them
// itself; the first pass's own cancels then lose the CAS. That is S19.
func TestDissolve_ASecondPassDuringADissolveDecidesNothing(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	em := &secondPassEmitter{}
	d := NewDispatcher(db, testdb.NewSuccessBackend(), em, "core", "shingo.dispatch", nil)
	d.DebugLog = testDebugLog(t)
	parent, _, _, legs := planDigToDissolve(t, db, d, "DS-TWICE", 2)

	var during []protocol.Status
	em.onCancel = func(int64) {
		testutil.MustNoErr(t, d.dissolveCompound(parent.ID, "test: the second pass"), "the second pass")
		for _, l := range legs[1:] {
			o, err := db.GetOrder(l.ID)
			testutil.MustNoErr(t, err, "reload a leg during the first pass")
			during = append(during, o.Status)
		}
	}
	testutil.MustNoErr(t, d.dissolveCompound(parent.ID, "test: the first pass"), "the first pass")

	if len(during) == 0 {
		t.Fatal("the second pass never ran — the first pass cancelled nothing")
	}
	for i, st := range during {
		if protocol.IsTerminal(st) {
			t.Errorf("leg %d was %q when the second pass returned — it decided the dissolve again "+
				"while the first was still cancelling", legs[i+1].ID, st)
		}
	}
	for _, l := range legs[1:] {
		o, err := db.GetOrder(l.ID)
		testutil.MustNoErr(t, err, "reload a leg after the first pass")
		if o.Status != protocol.StatusCancelled || o.ErrorDetail != reshuffleDissolveDetail {
			t.Errorf("leg %d is %q (%q) after the first pass, want cancelled under the dissolve marker",
				o.ID, o.Status, o.ErrorDetail)
		}
	}
}
