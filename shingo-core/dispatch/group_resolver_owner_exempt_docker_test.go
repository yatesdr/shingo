//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// ── THE OWNER EXEMPTION, AGAINST A REAL DATABASE ──────────────────────────
//
// THE OWNER-AWARE CHANGE pointed resolveStoreLKND and resolveStoreDPTH at the
// owner-aware selector (FindStoreSlotInLaneExcluding, asker.OrderID) instead of the blind
// one. It shipped with no test of the case that matters, and the reason it had
// none is worth stating: every unit fixture in binresolver/ passes
// reservations.Anyone, which carries OrderID 0 and reproduces the blind form
// exactly — so the arm the commit changed was never entered. The only other
// route into it is rebindGatedDropoff, which needs a lane gate mark, and no
// fixture in the tree that runs these resolvers has one.
//
// So the behaviour change shipped uncertified on five live call sites, on every
// ungated plain store at both plants. This is the certification.
//
// WHAT GOES WRONG WITHOUT IT, in the selector's own words
// (store/nodes/lanes.go:120-128): the blind guards are owner-BLIND, so an order
// that already holds a slot "matches all three NOT EXISTS clauses against ITSELF
// and its own slot is invisible. Re-resolving with the blind version therefore
// never returns the slot the order already holds; it returns the next-best one,
// which is SHALLOWER. Re-binding to that would silently undo back-to-front
// packing ... while looking like it worked."
//
// That is a lane bubble being built one re-resolve at a time, and it is exactly
// the entombment shape the lane seam exists to prevent.

// ownerExemptFixture builds a group where exactly one slot is the right answer
// and the asking order is the only reason it is available.
//
// Lane A is empty and the order holds its DEEPEST slot — a pending slot
// reservation plus delivery_node, which is the shape a plain store carries
// between resolve and dispatch (simple stores set delivery_node and do not
// reserve; complex ones reserve; a gate-staged order has both, so holding both
// here covers every caller of these resolvers).
//
// Lane B is packed solid, so it can offer no slot at all. It exists so the
// answer has to come from lane A under BOTH storage algorithms: LKND ranks a
// matching-payload lane first and finds it full, DPTH walks it and finds it
// full, and both fall through to lane A. Without it, a shallower slot in a
// sibling lane would be a legitimate answer and the test would be asserting the
// resolver's lane RANKING rather than its slot exemption.
func ownerExemptFixture(t *testing.T, db *store.DB, algo string) (grp *nodes.Node, laneASlots []*nodes.Node, holder *orders.Order) {
	t.Helper()
	group, _, slots, bp := setupNodeGroup(t, db)
	if algo != "" {
		testutil.MustNoErr(t, db.SetNodeProperty(group.ID, "store_algorithm", algo), "set store_algorithm")
	}

	// Lane B full — every slot physically occupied, so it yields nothing.
	for i, s := range slots[1] {
		createTestBinAtNode(t, db, bp.Code, s.ID, fmt.Sprintf("OWNEX-FULL-%d", i))
	}

	deepest := slots[0][len(slots[0])-1]
	holder = testdbCreateStore(t, db, deepest.Name)
	testutil.MustNoErr(t, db.ReserveSlot(deepest.ID, holder.ID), "reserve the deepest slot for the holder")

	return group, slots[0], holder
}

// testdbCreateStore makes a live, non-terminal store order aimed at deliveryNode
// — the delivery_node string-proxy is one of the three guards the exemption has
// to see through, and it is the only one a simple store sets.
func testdbCreateStore(t *testing.T, db *store.DB, deliveryNode string) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID: "ownex-holder-" + deliveryNode, StationID: "test",
		OrderType: "move", Status: "in_transit", Quantity: 1,
		DeliveryNode: deliveryNode,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create holder order")
	return o
}

// TestGroupResolveStore_OwnerGetsItsOwnDeepSlotBack is the case that change made
// and did not pin. It runs both storage algorithms, because both arms moved and they reach the selector by different routes.
//
// Each subtest asserts the fix and the defect in one run:
//
//	OWNER-AWARE (AskerFor(holder))  -> the holder's OWN deepest slot
//	BLIND       (reservations.Anyone) -> a SHALLOWER slot
//
// The blind arm is not decoration. reservations.Anyone carries OrderID 0 and
// FindStoreSlotInLaneExcluding documents 0 as reproducing the blind behaviour
// exactly, so that arm IS the pre-change resolver, exercised through the same
// production code path. It fails the way the old code failed,
// which means this test cannot pass for the wrong reason if somebody quietly
// drops asker.OrderID again.
func TestGroupResolveStore_OwnerGetsItsOwnDeepSlotBack(t *testing.T) {
	t.Parallel()
	for _, algo := range []string{"LKND", "DPTH"} {
		t.Run(algo, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			grp, laneA, holder := ownerExemptFixture(t, db, algo)
			deepest := laneA[len(laneA)-1]

			gr := &GroupResolver{DB: db}

			// 1. OWNER-AWARE: the holder re-resolves its own group.
			own, err := gr.ResolveStore(grp, "", nil, reservations.AskerFor(holder.ID, 0))
			if err != nil {
				t.Fatalf("ResolveStore(owner-aware): %v — the holder cannot see the slot it is "+
					"already holding, which is the trap the owner-aware form exists to close", err)
			}
			if own.Node == nil {
				t.Fatal("owner-aware resolve returned no node")
			}
			if own.Node.ID != deepest.ID {
				t.Fatalf("owner-aware resolve returned %s, want the holder's OWN deepest slot %s.\n"+
					"Returning anything else walks the order forward out of the slot it holds and "+
					"toward the mouth — back-to-front packing silently undone, one re-resolve at a "+
					"time, while looking like it worked (store/nodes/lanes.go:120-128).",
					own.Node.Name, deepest.Name)
			}

			// 2. BLIND, through the same call path: OrderID 0 is the pre-change
			//    behaviour, and it must still be broken. If this arm starts agreeing
			//    with the one above, the fixture has stopped holding the slot and the
			//    assertion above is passing for free.
			blind, err := gr.ResolveStore(grp, "", nil, reservations.Anyone)
			if err != nil {
				t.Fatalf("ResolveStore(blind): %v — expected it to succeed and pick the WRONG slot; "+
					"an error here means lane A offered nothing at all and the fixture is degenerate", err)
			}
			if blind.Node == nil {
				t.Fatal("blind resolve returned no node")
			}
			if blind.Node.ID == deepest.ID {
				t.Fatalf("blind resolve ALSO returned %s. The holder's reservation and delivery_node "+
					"are supposed to make that slot invisible to a blind asker — if they do not, this "+
					"fixture is not exercising the exemption and the owner-aware assertion above "+
					"proves nothing.", deepest.Name)
			}
			if blind.Node.Depth == nil || deepest.Depth == nil {
				t.Fatalf("slot depths unreadable (blind=%v deepest=%v)", blind.Node.Depth, deepest.Depth)
			}
			if *blind.Node.Depth >= *deepest.Depth {
				t.Errorf("blind resolve returned %s at depth %d, want something SHALLOWER than the "+
					"holder's %s at depth %d — the documented failure is a walk toward the mouth",
					blind.Node.Name, *blind.Node.Depth, deepest.Name, *deepest.Depth)
			}
		})
	}
}
