//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// dig_dwell_reserved_slot_docker_test.go — a dweller and a slot somebody else has
// reserved.
//
// THE SPECIMEN (scripted sim, 2026-10-03, F6). Nine dig first-legs from nine
// different lanes stood staged, each robot holding a blocker, for between eight
// and twelve minutes. Every twelve seconds each one re-asked, each one was offered
// SMN_004, and each one lost the claim: "dig dwell: leg N lost SMN_004 between
// choosing it and claiming it (store slot contended: ... resource already
// reserved (race)) — re-asking". SMN_004 was held by a PENDING slot reservation
// of an unrelated complex swap that was reserve-holding (waiting for material, no
// claimed bin), and eighteen market slots stood free. It cleared only when a
// person terminated the swap.
//
// The walk tested availability with CheckDropoffCapacity, which counts bins and
// orders BRINGING a bin. A reserve-holding order brings nothing, so its slot read
// free to the walk and taken to the claim door, on every ask, for as long as the
// order held it.

// reservedSlotGroup builds:
//
//	<p>-GRP
//	├── <p>-DUGA  depth 2: S1 (blocker) · S2 (target)
//	├── <p>-DUGB  depth 2: S1 (blocker) · S2 (target)
//	├── <p>-FREE1 depth 1: S1
//	├── <p>-FREE2 depth 1: S1
//	├── <p>-FREE3 depth 1: S1
//	└── <p>-PARK  (a direct child: the walk's first candidate)
type reservedSlotGroup struct {
	grp, dugA, dugB, park *nodes.Node
	dugASlots, dugBSlots  []*nodes.Node
	free                  []*nodes.Node
	bp                    *payloads.Payload
}

func setupReservedSlotGroup(t *testing.T, db *store.DB, prefix string) *reservedSlotGroup {
	t.Helper()
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "get NGRP type")
	lanType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "get LANE type")

	g := &reservedSlotGroup{bp: &payloads.Payload{Code: prefix + "-P"}}
	testutil.MustNoErr(t, db.CreatePayload(g.bp), "create payload")
	g.grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(g.grp), "create group")

	mkLane := func(name string, depth int) (*nodes.Node, []*nodes.Node) {
		l := &nodes.Node{Name: name, NodeTypeID: &lanType.ID, ParentID: &g.grp.ID, Enabled: true, IsSynthetic: true}
		testutil.MustNoErr(t, db.CreateNode(l), "create "+name)
		var out []*nodes.Node
		for d := 1; d <= depth; d++ {
			at := d
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, d), ParentID: &l.ID, Enabled: true, Depth: &at}
			testutil.MustNoErr(t, db.CreateNode(s), "create slot")
			out = append(out, s)
		}
		reloaded, err := db.GetNode(l.ID)
		return testutil.Must(t, reloaded, err, "reload "+name), out
	}
	g.dugA, g.dugASlots = mkLane(prefix+"-DUGA", 2)
	g.dugB, g.dugBSlots = mkLane(prefix+"-DUGB", 2)
	for i := 1; i <= 3; i++ {
		_, s := mkLane(fmt.Sprintf("%s-FREE%d", prefix, i), 1)
		g.free = append(g.free, s[0])
	}
	g.park = &nodes.Node{Name: prefix + "-PARK", ParentID: &g.grp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(g.park), "create parking")

	for _, lane := range [][]*nodes.Node{g.dugASlots, g.dugBSlots} {
		createTestBinAtNode(t, db, g.bp.Code, lane[0].ID, lane[0].Name+"-BLK")
		createTestBinAtNode(t, db, g.bp.Code, lane[1].ID, lane[1].Name+"-TGT")
	}
	return g
}

// digFrom plans a dig for a target in lane slots and returns its first leg, the
// dweller holding the blocker.
func digFrom(t *testing.T, db *store.DB, d *Dispatcher, g *reservedSlotGroup, lane *nodes.Node, slots []*nodes.Node, uuid string) *orders.Order {
	t.Helper()
	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = g.bp.Code
		o.DeliveryNode = lineNode(t, db, uuid+"-LINE").Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, g.bp, lane, slots[1], demand)
	if len(legs) == 0 {
		t.Fatalf("the dig for %s planned no legs", uuid)
	}
	return legs[0]
}

// reserveHoldingSwap is the F6 holder: a complex order that has reserved a slot
// as its destination and is waiting for material, holding no bin.
func reserveHoldingSwap(t *testing.T, db *store.DB, uuid string, slot *nodes.Node) *orders.Order {
	t.Helper()
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.OrderType = OrderTypeComplex
		o.Status = protocol.StatusSourcing
		o.DeliveryNode = slot.Name
	})
	testutil.MustNoErr(t, db.ReserveSlot(slot.ID, o.ID), "the swap reserves its destination slot")
	return o
}

// askOnce runs ONE release pass for a dweller and returns it reloaded.
func askOnce(t *testing.T, d *Dispatcher, db *store.DB, leg *orders.Order) *orders.Order {
	t.Helper()
	d.EvaluateWaitLaneForStagedOrder(leg.ID)
	fresh, err := db.GetOrder(leg.ID)
	return testutil.Must(t, fresh, err, "reload the dweller")
}

// TestDwell_AStrangersPendingSlotReservationIsNotOffered is F6 in a fixture: a
// reserve-holding swap's pending slot reservation on the walk's first candidate,
// two dwellers from different lanes, and free slots behind it. Each dweller must
// bind a different free slot on its FIRST ask.
//
// Two things are pinned, because either alone frees these dwellers: the walk
// never OFFERS the reserved slot (Core does not pick a slot it cannot claim),
// and each dweller is bound on its first ask.
//
// MUTATION: drop the takeable test from shuffleSlotsFrom. Both dwellers are
// offered PARK first; the lost claim walks on, so they are still bound, and the
// "offered" assertion fires. Drop the walk-on as well and both stand with no
// destination, which was F6.
func TestDwell_AStrangersPendingSlotReservationIsNotOffered(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	g := setupReservedSlotGroup(t, db, "F6R")

	legA := digFrom(t, db, d, g, g.dugA, g.dugASlots, "f6r-a")
	legB := digFrom(t, db, d, g, g.dugB, g.dugBSlots, "f6r-b")
	swap := reserveHoldingSwap(t, db, "f6r-swap", g.park)

	var offered []string
	d.dwellChoiceHook = func(legID int64, dest *nodes.Node) { offered = append(offered, dest.Name) }

	a := askOnce(t, d, db, legA)
	b := askOnce(t, d, db, legB)

	for _, name := range offered {
		if name == g.park.Name {
			t.Errorf("the walk offered %s, which swap %d holds by a pending slot reservation; the claim "+
				"door refuses it on every ask for as long as the swap waits for material (offered: %v)",
				g.park.Name, swap.ID, offered)
			break
		}
	}

	for _, leg := range []*orders.Order{a, b} {
		if leg.DeliveryNode == "" {
			t.Errorf("dweller %d is still standing with no destination after its first ask (cause %q). "+
				"%s is reserved by swap %d, which is waiting for material and holds no bin; the walk "+
				"offered it anyway, the claim door refused it, and three free slots stood behind it",
				leg.ID, leg.QueueCause, g.park.Name, swap.ID)
			continue
		}
		if leg.DeliveryNode == g.park.Name {
			t.Errorf("dweller %d was bound to %s, which swap %d holds", leg.ID, g.park.Name, swap.ID)
		}
	}
	if a.DeliveryNode != "" && a.DeliveryNode == b.DeliveryNode {
		t.Errorf("both dwellers were bound to %s", a.DeliveryNode)
	}
	for _, leg := range []*orders.Order{a, b} {
		if leg.DeliveryNode == "" {
			continue
		}
		dest, err := db.GetNodeByDotName(leg.DeliveryNode)
		testutil.MustNoErr(t, err, "resolve the bound destination")
		if !holdsSlot(t, db, leg.ID, dest.ID) {
			t.Errorf("dweller %d was bound to %s without holding a slot reservation on it", leg.ID, dest.Name)
		}
	}
	if !holdsSlot(t, db, swap.ID, g.park.ID) {
		t.Errorf("the swap lost its reservation on %s; nothing here may take it", g.park.Name)
	}
}

// TestDwell_ALostClaimWalksOnInTheSamePass pins the walk past a claim lost in a
// true race: the slot read takeable, and another order reserved it between the
// walk's read and the claim. That slot is taken, not the pool, so the dweller
// binds the next free slot in the same pass instead of reporting no-shuffle-slot
// and waiting for the next firing.
//
// MUTATION: return the refusal from the lost-claim arm instead of excluding the
// slot and continuing. The dweller stands with no destination.
func TestDwell_ALostClaimWalksOnInTheSamePass(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	g := setupReservedSlotGroup(t, db, "LCW")
	leg := digFrom(t, db, d, g, g.dugA, g.dugASlots, "lcw-a")

	var thief *orders.Order
	d.dwellChoiceHook = func(legID int64, dest *nodes.Node) {
		if thief != nil {
			return
		}
		thief = reserveHoldingSwap(t, db, "lcw-thief", dest)
	}

	got := askOnce(t, d, db, leg)
	if thief == nil {
		t.Fatal("the dweller never reached a claim; the fixture is wrong")
	}
	stolen, err := db.GetNodeByDotName(thief.DeliveryNode)
	testutil.MustNoErr(t, err, "resolve the stolen slot")
	if got.DeliveryNode == "" {
		t.Fatalf("the dweller lost %s to order %d and stood with no destination (cause %q). The slot "+
			"was taken, not the pool; the next candidate was already known", stolen.Name, thief.ID, got.QueueCause)
	}
	if got.DeliveryNode == stolen.Name {
		t.Fatalf("the dweller was bound to %s, which order %d reserved before the claim", stolen.Name, thief.ID)
	}
	dest, err := db.GetNodeByDotName(got.DeliveryNode)
	testutil.MustNoErr(t, err, "resolve the bound destination")
	if !holdsSlot(t, db, got.ID, dest.ID) {
		t.Errorf("the dweller was bound to %s without holding a slot reservation on it", dest.Name)
	}
	if !holdsSlot(t, db, thief.ID, stolen.ID) {
		t.Errorf("order %d lost its reservation on %s", thief.ID, stolen.Name)
	}
}

// TestDwell_TwoDwellersRacingForOneSlotBindTwo is the same race with the other
// party a dweller: A chooses a slot, B is released in the window, chooses the
// same slot and claims it first. A's claim is refused and A walks on. Two
// dwellers never bind one slot, and neither stands down for it.
//
// MUTATION: delete the claimStoreSlot call from bindChosenDestination. Both
// dwellers are bound to the same slot.
func TestDwell_TwoDwellersRacingForOneSlotBindTwo(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	g := setupReservedSlotGroup(t, db, "TDR")
	legA := digFrom(t, db, d, g, g.dugA, g.dugASlots, "tdr-a")
	legB := digFrom(t, db, d, g, g.dugB, g.dugBSlots, "tdr-b")

	var b *orders.Order
	d.dwellChoiceHook = func(legID int64, dest *nodes.Node) {
		if legID != legA.ID || b != nil {
			return
		}
		b = askOnce(t, d, db, legB)
	}

	a := askOnce(t, d, db, legA)
	if b == nil {
		t.Fatal("dweller A never reached a claim; the fixture is wrong")
	}
	if b.DeliveryNode == "" {
		t.Fatalf("dweller B, released in A's window, was not bound (cause %q)", b.QueueCause)
	}
	if a.DeliveryNode == "" {
		t.Fatalf("dweller A lost its slot to B and stood with no destination (cause %q); free slots remained",
			a.QueueCause)
	}
	if a.DeliveryNode == b.DeliveryNode {
		t.Fatalf("both dwellers were bound to %s", a.DeliveryNode)
	}
	for _, leg := range []*orders.Order{a, b} {
		dest, err := db.GetNodeByDotName(leg.DeliveryNode)
		testutil.MustNoErr(t, err, "resolve the bound destination")
		if !holdsSlot(t, db, leg.ID, dest.ID) {
			t.Errorf("dweller %d was bound to %s without holding a slot reservation on it", leg.ID, dest.Name)
		}
	}
}
