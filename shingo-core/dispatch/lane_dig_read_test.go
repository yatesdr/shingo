//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// plainOrderIntoLane builds a PLAIN order (no parent) that picks from a lane
// slot and delivers to a line node, on a group left at the DEFAULT enforcement
// mode — `none`, which is what both plants run.
func plainOrderIntoLane(t *testing.T, db *store.DB, prefix string) (*orders.Order, int64, *nodes.Node, *nodes.Node) {
	t.Helper()
	sd := testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, prefix+"-LANE", 3)
	slots, err := db.ListLaneSlots(lane)
	if err != nil || len(slots) < 1 {
		t.Fatalf("list lane slots: %v (got %d)", err, len(slots))
	}
	dest, err := db.GetNodeByDotName(sd.LineNode.Name)
	if err != nil {
		t.Fatalf("resolve delivery node: %v", err)
	}

	o := &orders.Order{
		EdgeUUID: prefix + "-plain", StationID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusSourcing, Quantity: 1,
		SourceNode: slots[0].Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create plain order")
	return o, lane, slots[0], dest
}

func digHolder(t *testing.T, db *store.DB, uuid string) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID: uuid, StationID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusReshuffling, Quantity: 1,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create dig-holding order")
	return o
}

// TestAcquireLanesForOrder_ForeignDigRefuses is the floor defect.
//
// The dig row is written UNCONDITIONALLY — LaneLock.TryLock takes a ModeDig lane
// reservation with no enforcement-mode check (binresolver/lane_lock.go:60-69) —
// and on `none` it was read by nobody on this path. resolveOrderLaneHolds drops
// every lane with no gate mark (laneIsGated, lane_gate.go:139), so a `none`
// group yields zero holds, and AcquireLanesForOrder then returns admitted on
// len(holds)==0 (lane_gate.go:420-422) before anything consults the dig.
//
// The compound path already asks this question mode-independently
// (admission.go:230-261, whose comment says so in as many words). Plain orders do
// not go through admit at all — AcquireLanesForOrder is explicitly documented as
// not delegating to admission — so this is the path where a plain retrieve walked
// into a corridor another reshuffle owned.
//
// The question is PHYSICAL: a single-file lane being dug is being dug whether or
// not Core arbitrates its mouth.
func TestAcquireLanesForOrder_ForeignDigRefuses(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	o, lane, src, dest := plainOrderIntoLane(t, db, "FDIG")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	foreign := digHolder(t, db, "FDIG-foreign-dig")
	if !d.laneLock.TryLock(lane, foreign.ID) {
		t.Fatal("foreign dig could not take the lane")
	}

	admitted, cause, laneName, err := d.AcquireLanesForOrder(o, src, dest, EntryFreshBin)
	if err != nil {
		t.Fatalf("AcquireLanesForOrder: %v", err)
	}
	if admitted {
		t.Fatal("a plain order was admitted into a lane another reshuffle is digging. " +
			"The dig row is written regardless of enforcement mode; on `none` — what both " +
			"plants run — nothing on the plain-order path read it")
	}
	if cause != CauseLaneDigActive {
		t.Errorf("cause = %q, want %q", cause, CauseLaneDigActive)
	}
	if laneName == "" {
		t.Error("no lane name reported; the operator sentence needs the contended lane")
	}
}

// TestAcquireLanesForOrder_OwnDigAdmitsTheDigOwner is the exemption that must
// exist BEFORE the refusal, and it is the one a leg-shaped exemption misses.
//
// laneOwnerFor returns the order itself when it has no parent (lane_gate.go:457-
// 463), so a leg-shaped exemption — one that requires owner != order.ID — is
// FALSE for the dig owner. ownsDig answers this on its own first arm instead
// (digOwner == orderID); an exemption written only for legs refuses the dig's
// OWNER at its own dig row.
//
// That is not hypothetical on this path. In expose mode the lane lock is
// TRANSFERRED from the compound parent to the complex parent (compound.go:327-
// 331), and ResumeCompound then puts that parent back through the scanner to
// re-resolve its own pickup. The scanner calls AcquireLanesForOrder. So the order
// arriving here is routinely the dig owner, entering the lane its own dig holds,
// and the lock is released by that very pickup — refuse it and nothing ever
// clears it.
//
// MUTATION: drop ownsDig's owner arm — the `digOwner == orderID` return —
// leaving only the laneOwnerFor comparison, and this test wedges exactly there.
func TestAcquireLanesForOrder_OwnDigAdmitsTheDigOwner(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	o, lane, src, dest := plainOrderIntoLane(t, db, "ODIG")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	if !d.laneLock.TryLock(lane, o.ID) {
		t.Fatal("order could not take its own dig on the lane")
	}

	admitted, cause, _, err := d.AcquireLanesForOrder(o, src, dest, EntryFreshBin)
	if err != nil {
		t.Fatalf("AcquireLanesForOrder: %v", err)
	}
	if !admitted {
		t.Fatalf("the dig OWNER was refused at its own dig row (cause %q). Its own pickup is what "+
			"releases the lock, so this is a permanent wedge, not a wait", cause)
	}
}

// TestAcquireLanesForOrder_OwnDigAdmitsItsOwnLeg — a leg of the dig passes its
// parent's dig row. DESIGN §15 item 1 records this exact defect being fixed once
// already (9fc1f505); without it a leg parks behind a lock only its own
// completion clears.
func TestAcquireLanesForOrder_OwnDigAdmitsItsOwnLeg(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, lane, src, dest := plainOrderIntoLane(t, db, "LEGDIG")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	parent := digHolder(t, db, "LEGDIG-parent")
	if !d.laneLock.TryLock(lane, parent.ID) {
		t.Fatal("parent could not take the dig")
	}
	child := &orders.Order{
		EdgeUUID: "LEGDIG-child", StationID: "line-1",
		OrderType: OrderTypeMove, Status: StatusPending, Quantity: 1,
		ParentOrderID: &parent.ID, Sequence: 1,
		SourceNode: src.Name, DeliveryNode: dest.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(child), "create child leg")

	admitted, cause, _, err := d.AcquireLanesForOrder(child, src, dest, EntryFreshBin)
	if err != nil {
		t.Fatalf("AcquireLanesForOrder: %v", err)
	}
	if !admitted {
		t.Fatalf("a leg of the dig was refused by its OWN parent's dig row (cause %q)", cause)
	}
}

// TestAcquireLanesForOrder_NoDigUnaffected — the no-dig path stays byte-identical
// on the default mode. The wiring must add a refusal where a dig exists, not a
// new way for ordinary work to park.
func TestAcquireLanesForOrder_NoDigUnaffected(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	o, _, src, dest := plainOrderIntoLane(t, db, "NODIG")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	admitted, cause, laneName, err := d.AcquireLanesForOrder(o, src, dest, EntryFreshBin)
	if err != nil {
		t.Fatalf("AcquireLanesForOrder: %v", err)
	}
	if !admitted || cause != "" || laneName != "" {
		t.Fatalf("un-dug lane on the default mode: admitted=%v cause=%q lane=%q, want admitted with "+
			"no cause — nothing here is gated", admitted, cause, laneName)
	}
}
