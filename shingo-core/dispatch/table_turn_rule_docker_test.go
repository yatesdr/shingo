//go:build docker

package dispatch

import (
	"testing"

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
