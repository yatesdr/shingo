//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// table_turn_rule_docker_test.go — the turn rule at the create seams (PLAN §5
// phase 5): a lift with no wait in front at a node that turns the table gets a
// turn wait, goes out unsealed, and its lift is appended when the table reads
// the angle. Nobody presses anything.

// blockShape renders blocks as binTask@location.
func blockShape(bs []fleet.OrderBlock) string {
	out := ""
	for i, b := range bs {
		if i > 0 {
			out += " "
		}
		out += b.BinTask + "@" + b.Location
	}
	return out
}

// arrive is the fleet reporting the robot parked at its first wait: in transit
// with the robot named, then staged.
func (r turnRig) arrive(t *testing.T, id int64) *orders.Order {
	t.Helper()
	o := prReload(t, r.db, id)
	r.backend.SetOrderVehicle(o.VendorOrderID, ttVehicle)
	testutil.MustNoErr(t, r.d.lifecycle.MarkInTransit(o, ttVehicle, "fleet"), "in transit")
	o = prReload(t, r.db, id)
	testutil.MustNoErr(t, r.d.lifecycle.MarkStaged(o, "fleet"), "staged")
	return prReload(t, r.db, id)
}

// TestTableTurnRule_PlainMoveFromATurnNode: the create is one Wait at the
// node, unsealed, with the plan stored and no turn sent before the robot is
// there; the arrival sends the turn, and the lift and drop are appended,
// sealed, once the table reads the angle.
func TestTableTurnRule_PlainMoveFromATurnNode(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	x, y := prNode(t, r.db, "TR1-X"), prNode(t, r.db, "TR1-Y")
	r.turnAt(t, x.Name)
	order := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
		o.DeliveryNode, o.Status = y.Name, "pending"
	})
	if _, err := r.d.DispatchDirect(order, x, y); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}
	creates := r.backend.CreateRequests()
	if len(creates) != 1 || creates[0].Complete || blockShape(creates[0].Blocks) != "Wait@TR1-X" {
		t.Fatalf("creates %+v, want one unsealed [Wait@TR1-X]", creates)
	}
	if r.turns() != 0 || r.table.readCount() != 0 || r.appends() != 0 {
		t.Fatalf("before the robot is there: %d turns, %d reads, %d appends, want none", r.turns(), r.table.readCount(), r.appends())
	}
	stored := prReload(t, r.db, order.ID)
	steps, ok := decodeSteps(stored.StepsJSON)
	if !ok || shape(steps) != "wait@TR1-X:turn pickup@TR1-X dropoff@TR1-Y" {
		t.Fatalf("stored plan %q, want the turn wait in front of the lift", stored.StepsJSON)
	}

	o := r.arrive(t, order.ID)
	if at, kind, _ := stationPoint(steps, o.WaitIndex); at != nil || kind != WaitKindTurn {
		t.Errorf("the station is told %v/%q, want no ordinal and %q", at, kind, WaitKindTurn)
	}
	r.d.RedriveTableTurns(o.ID) // the staged hook
	if r.turns() != 1 {
		t.Fatalf("%d turns on arrival, want 1", r.turns())
	}
	if fresh := prReload(t, r.db, o.ID); QueueCause(fresh.QueueCause) != CauseTableTurning {
		t.Errorf("the row says %q, want %q", fresh.QueueCause, CauseTableTurning)
	}
	r.table.turn(1.569, false)
	if got := r.d.RedriveTableTurns(0); len(got) != 1 {
		t.Fatalf("on angle: appended %v", got)
	}
	a := r.backend.ReleaseCalls()[0]
	if blockShape(a.Blocks) != "JackLoad@TR1-X JackUnload@TR1-Y" || !a.Complete {
		t.Errorf("appended %s complete=%v, want [JackLoad@TR1-X JackUnload@TR1-Y] sealed", blockShape(a.Blocks), a.Complete)
	}
}

// TestTableTurnRule_IntoAGatedLane: a move from a turn node into a gated lane.
// The lane valve neither appends nor writes a lane cause, because the plan's
// first wait is the turn's; after the turn the robot is sent on to the lane
// wait, and the lane is asked.
func TestTableTurnRule_IntoAGatedLane(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	laneID, _, slot := gateChoreoLane(t, r.db, "TR2", "TR2-WAIT")
	x := prNode(t, r.db, "TR2-X")
	r.turnAt(t, x.Name)
	// The lane is contended (a dig holds it), so a valve that read the turn wait
	// as its lane's would refuse and write a lane cause on the row.
	digger := testdb.CreateOrder(t, r.db)
	if !r.d.laneLock.TryLock(laneID, digger.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}
	order := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
		o.DeliveryNode, o.Status = slot.Name, "pending"
	})
	if _, err := r.d.DispatchDirect(order, x, slot); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}
	creates := r.backend.CreateRequests()
	if len(creates) != 1 || creates[0].Complete || blockShape(creates[0].Blocks) != "Wait@TR2-X" {
		t.Fatalf("creates %+v, want one unsealed [Wait@TR2-X]", creates)
	}
	if r.appends() != 0 {
		t.Fatalf("the lane valve appended %d segment(s) behind the turn wait", r.appends())
	}
	if fresh := prReload(t, r.db, order.ID); fresh.QueueCause != "" {
		t.Errorf("the row says %q before the robot is anywhere, want nothing (no lane cause)", fresh.QueueCause)
	}
	for _, act := range auditActions(t, r, order.ID) {
		if act == "table_turn_refused" {
			t.Error("something tried to append behind the turn wait at create (the append door refused it): " +
				"the lane valve read the turn wait as its own")
		}
	}
	o := r.arrive(t, order.ID)
	r.d.RedriveTableTurns(o.ID)
	r.table.turn(1.569, false)
	if got := r.d.RedriveTableTurns(0); len(got) != 1 {
		t.Fatalf("on angle: appended %v", got)
	}
	a := r.backend.ReleaseCalls()[0]
	if blockShape(a.Blocks) != "JackLoad@TR2-X Wait@TR2-WAIT" || a.Complete {
		t.Errorf("after the turn: %s complete=%v, want the lift and the drive to the lane wait, unsealed",
			blockShape(a.Blocks), a.Complete)
	}
	if fresh := prReload(t, r.db, order.ID); fresh.WaitIndex < 1 {
		t.Errorf("wait_index %d after the turn's append, want past the turn wait", fresh.WaitIndex)
	}
}

// TestTableTurnRule_ComplexMultiBinKeepsItsCarts: an Edge-authored complex
// order that lifts at A and then at a turn node X. The create stops at X's
// turn wait, and the cart rows stand under the steps they lift and set down at
// in the stored plan, the turn wait counted.
func TestTableTurnRule_ComplexMultiBinKeepsItsCarts(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	sd := testdb.SetupStandardData(t, r.db)
	a, b, x, c := prNode(t, r.db, "TR3-A"), prNode(t, r.db, "TR3-B"), prNode(t, r.db, "TR3-X"), prNode(t, r.db, "TR3-C")
	binA := testdb.CreateBinAtNode(t, r.db, sd.Payload.Code, a.ID, "TR3-BIN-A")
	binX := testdb.CreateBinAtNode(t, r.db, sd.Payload.Code, x.ID, "TR3-BIN-X")
	r.turnAt(t, x.Name)
	o := submitComplexAndDispatch(t, r.d, r.db, testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "tr3", PayloadCode: sd.Payload.Code, Quantity: 1,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: a.Name}, {Action: protocol.ActionDropoff, Node: b.Name},
			{Action: protocol.ActionPickup, Node: x.Name}, {Action: protocol.ActionDropoff, Node: c.Name},
		},
	})
	creates := r.backend.CreateRequests()
	if len(creates) != 1 || creates[0].Complete || blockShape(creates[0].Blocks) != "JackLoad@TR3-A JackUnload@TR3-B Wait@TR3-X" {
		t.Fatalf("creates %+v, want one unsealed [JackLoad@A JackUnload@B Wait@X]", creates)
	}
	steps, ok := decodeSteps(o.StepsJSON)
	if !ok || shape(steps) != "pickup@TR3-A dropoff@TR3-B wait@TR3-X:turn pickup@TR3-X dropoff@TR3-C" {
		t.Fatalf("stored plan %q, want the turn wait in front of the second lift", o.StepsJSON)
	}
	want := map[int64][2]string{binA.ID: {a.Name, b.Name}, binX.ID: {x.Name, c.Name}}
	if len(o.Carts) != 2 {
		t.Fatalf("cart rows %+v, want two", o.Carts)
	}
	for _, cart := range o.Carts {
		w, ok := want[cart.BinID]
		if !ok || cart.PickStep == nil || cart.DropStep == nil {
			t.Fatalf("cart row %+v is not one of the order's bins with both steps", cart)
		}
		if got := [2]string{domain.StepNode(o, *cart.PickStep), domain.StepNode(o, *cart.DropStep)}; got != w {
			t.Errorf("bin %d's row reads lift@%s drop@%s, want lift@%s drop@%s", cart.BinID, got[0], got[1], w[0], w[1])
		}
	}
}

// TestTableTurnRule_NoAngleNodesAreUntouched: with an unrelated node carrying
// an angle, a plain move and a complex leg at nodes with none create exactly
// what they always have, read no robot and turn nothing.
func TestTableTurnRule_NoAngleNodesAreUntouched(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	prNode(t, r.db, "TR4-ELSEWHERE")
	r.turnAt(t, "TR4-ELSEWHERE")
	src, dst := prNode(t, r.db, "TR4-SRC"), prNode(t, r.db, "TR4-DST")
	plain := testdb.CreateOrder(t, r.db, func(o *orders.Order) { o.DeliveryNode, o.Status = dst.Name, "pending" })
	if _, err := r.d.DispatchDirect(plain, src, dst); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}
	sd := testdb.SetupStandardData(t, r.db)
	testdb.CreateBinAtNode(t, r.db, sd.Payload.Code, src.ID, "TR4-BIN")
	submitComplexAndDispatch(t, r.d, r.db, testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "tr4", PayloadCode: sd.Payload.Code, Quantity: 1,
		Steps: []protocol.ComplexOrderStep{{Action: protocol.ActionPickup, Node: src.Name}, {Action: protocol.ActionDropoff, Node: dst.Name}},
	})
	creates := r.backend.CreateRequests()
	if len(creates) != 2 {
		t.Fatalf("%d creates, want 2", len(creates))
	}
	if got := blockShape(creates[0].Blocks); got != "JackLoad@TR4-SRC JackUnload@TR4-DST" || !creates[0].Complete {
		t.Errorf("plain create %s complete=%v, want the sealed two-block move", got, creates[0].Complete)
	}
	if got := blockShape(creates[1].Blocks); got != "JackLoad@TR4-SRC JackUnload@TR4-DST" {
		t.Errorf("complex create %s, want its two blocks", got)
	}
	if r.turns() != 0 || r.table.readCount() != 0 {
		t.Errorf("%d turns and %d robot reads, want none", r.turns(), r.table.readCount())
	}
}

// TestTableTurnRule_LoadSequenceAtATurnNodeWaits: a payload whose load sequence
// (F4c) would be dropped at a turn node is held with the reason, not sent.
func TestTableTurnRule_LoadSequenceAtATurnNodeWaits(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	_, line, payload := testdb.SetupTestData(t, r.db)
	payload.AdvancedLoadSequence = "Child cart interlock"
	testutil.MustNoErr(t, r.db.UpdatePayload(payload), "configure payload sequence")
	x := prNode(t, r.db, "TR5-X")
	r.turnAt(t, x.Name)
	order := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
		o.DeliveryNode, o.Status, o.PayloadCode, o.OrderType = line.Name, "pending", payload.Code, OrderTypeRetrieve
	})
	if _, err := r.d.DispatchDirect(order, x, line); err == nil {
		t.Fatal("dispatched a sequenced lift at a turn node; the sequence would have been dropped")
	}
	if n := len(r.backend.CreateRequests()); n != 0 {
		t.Errorf("%d creates, want none", n)
	}
	if fresh := prReload(t, r.db, order.ID); QueueCause(fresh.QueueCause) != CauseTurnLoadSequence {
		t.Errorf("the row says %q, want %q", fresh.QueueCause, CauseTurnLoadSequence)
	}
}

// auditActions is the order's audit actions, in order.
func auditActions(t *testing.T, r turnRig, id int64) []string {
	t.Helper()
	rows, err := r.db.ListEntityAudit("order", id)
	testutil.MustNoErr(t, err, "audit")
	var out []string
	for _, a := range rows {
		out = append(out, a.Action)
	}
	return out
}

// ── The set-down side (PLAN §5 phase 6) ─────────────────────────────────────

// turnBeforeSetDown sets a node to turn its table before the set-down.
func (r turnRig) turnBeforeSetDown(t *testing.T, node string) {
	t.Helper()
	n, err := r.db.GetNodeByDotName(node)
	n = testutil.Must(t, n, err, "node "+node)
	testutil.MustNoErr(t, r.db.SetNodeProperty(n.ID, PropTableTurnSide, TableTurnSideSetDown), "side")
	testutil.MustNoErr(t, r.db.SetNodeProperty(n.ID, PropTableTurnRad, "1.5707963267948966"), "angle")
	testutil.MustNoErr(t, r.d.ReloadTableTurns(), "reload")
}

// loaded is the robot's jack up with a cart on it.
func (f *fakeTable) loaded(up bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.JackState, f.st.JackIsFull = 3, false
	if up {
		f.st.JackState, f.st.JackIsFull = 1, true
	}
}

// TestTableTurnSetDown_SequentialBackfill: a move onto a set-down node creates
// the lift and the drive onto the node, unsealed; the turn goes with the cart
// up; the set-down is appended, sealed, on angle.
func TestTableTurnSetDown_SequentialBackfill(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	src, l := prNode(t, r.db, "TS1-SRC"), prNode(t, r.db, "TS1-L")
	r.turnBeforeSetDown(t, l.Name)
	order := testdb.CreateOrder(t, r.db, func(o *orders.Order) { o.DeliveryNode, o.Status = l.Name, "pending" })
	if _, err := r.d.DispatchDirect(order, src, l); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}
	creates := r.backend.CreateRequests()
	if len(creates) != 1 || creates[0].Complete || blockShape(creates[0].Blocks) != "JackLoad@TS1-SRC Wait@TS1-L" {
		t.Fatalf("creates %+v, want one unsealed [JackLoad@TS1-SRC Wait@TS1-L]", creates)
	}
	r.table.loaded(true)
	o := r.arrive(t, order.ID)
	r.d.RedriveTableTurns(o.ID)
	if r.turns() != 1 {
		t.Fatalf("%d turns with the cart up, want 1", r.turns())
	}
	r.table.turn(1.569, false)
	if got := r.d.RedriveTableTurns(0); len(got) != 1 {
		t.Fatalf("on angle: appended %v", got)
	}
	if a := r.backend.ReleaseCalls()[0]; blockShape(a.Blocks) != "JackUnload@TS1-L" || !a.Complete {
		t.Errorf("appended %s complete=%v, want [JackUnload@TS1-L] sealed", blockShape(a.Blocks), a.Complete)
	}
}

// TestTableTurnSetDown_NoTurnWithTheJackDown: a set-down turn needs the cart
// up; with the jack down nothing is sent and the row says why.
func TestTableTurnSetDown_NoTurnWithTheJackDown(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	src, l := prNode(t, r.db, "TS2-SRC"), prNode(t, r.db, "TS2-L")
	r.turnBeforeSetDown(t, l.Name)
	order := testdb.CreateOrder(t, r.db, func(o *orders.Order) { o.DeliveryNode, o.Status = l.Name, "pending" })
	if _, err := r.d.DispatchDirect(order, src, l); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}
	o := r.arrive(t, order.ID) // the fake robot's jack is down
	r.d.RedriveTableTurns(o.ID)
	r.d.RedriveTableTurns(0)
	if r.turns() != 0 {
		t.Errorf("%d turns with the jack down, want none", r.turns())
	}
	if fresh := prReload(t, r.db, o.ID); QueueCause(fresh.QueueCause) != CauseTableTurnBlocked {
		t.Errorf("the row says %q %q, want %q", fresh.QueueCause, fresh.QueueReason, CauseTableTurnBlocked)
	}
}

// TestTableTurnSetDown_SupplyWaitsForTheEvac: at a line set to turn before the
// set-down, a two_robot supply's released segment drives onto the line and
// holds no drop. The two rules that keep a supply off an occupied line read it
// with its set-down: the Edge is told the supply awaits the evac's lift, and a
// press of the supply with its stamped bin still on the line is held until the
// lift, then goes, and the set-down follows the turn.
func TestTableTurnSetDown_SupplyWaitsForTheEvac(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	evac, cart := ttEvac(t, r, "TS3", "TS3-supply", StatusInTransit) // not yet at its lift
	r.turnBeforeSetDown(t, "TS3-LINE")
	stage := prNode(t, r.db, "TS3-STAGE")
	stamped := time.Now().UTC().Add(-time.Minute)
	supply := vsStagedLeg(t, r.db, "TS3-supply", evac.EdgeUUID, "TS3-LINE", StatusStaged, []resolvedStep{
		voWait(stage.Name, WaitKindStation), vsPick(stage.Name), voWait("TS3-LINE", WaitKindTurn),
		voDrop("TS3-LINE", &vacateStamp{Node: "TS3-LINE", Bin: cart.ID, Partner: evac.ID, StampedAt: &stamped})})
	r.backend.SetOrderVehicle(supply.VendorOrderID, ttVehicle)

	pts := r.d.ReleasePoints("line-1", []string{supply.EdgeUUID})
	if len(pts) != 1 || len(pts[0].AwaitsLift) != 1 || pts[0].AwaitsLift[0].Node != "TS3-LINE" || pts[0].AwaitsLift[0].CoRelease {
		t.Fatalf("release points %+v, want the supply to await the evac's lift at TS3-LINE, not co-released", pts)
	}
	if got := r.press(t, supply, 0); got != "appends=0 wait_index=0 status=staged errors=[]" {
		t.Fatalf("a press of the supply with the line's cart still on it: %s, want taken and held", got)
	}
	held := prReload(t, r.db, supply.ID)
	if !CoreOwnsWaitAt(held.StepsJSON, held.WaitIndex) {
		t.Fatal("the held supply is not Core's")
	}
	vhLift(t, r.db, cart.ID)
	if got := r.d.RedriveVacateWaits(cart.ID); len(got) != 1 {
		t.Fatalf("after the lift: released %v, want the supply", got)
	}
	if a := r.backend.ReleaseCalls()[0]; blockShape(a.Blocks) != "JackLoad@TS3-STAGE Wait@TS3-LINE" {
		t.Fatalf("appended %s, want the lift at staging and the drive onto the line", blockShape(a.Blocks))
	}
	r.table.loaded(true)
	o := prReload(t, r.db, supply.ID)
	testutil.MustNoErr(t, r.d.lifecycle.MarkStaged(o, "fleet"), "staged at the line")
	r.d.RedriveTableTurns(o.ID)
	r.table.turn(1.569, false)
	if got := r.d.RedriveTableTurns(0); len(got) != 1 {
		t.Fatalf("on angle: appended %v, want the set-down", got)
	}
	if a := r.backend.ReleaseCalls()[1]; blockShape(a.Blocks) != "JackUnload@TS3-LINE" {
		t.Errorf("appended %s, want the set-down", blockShape(a.Blocks))
	}
}
