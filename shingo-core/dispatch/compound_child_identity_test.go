//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestCompoundChild_IdentityIsMintedNotDerived is the v71 fix.
//
// A compound child's edge_uuid used to be BUILT from its parent's:
// "<parent-uuid>-step-<sequence>". That is a structural name, not an identity —
// and re-planning a parent's steps mints the same string again. The dev sim had
// one value on five rows.
//
// The column those strings live in is the one migration v71 makes UNIQUE, on the
// well-founded grounds that two orders sharing an edge_uuid is a shape this
// system has no story for (GetByUUID resolves the ambiguity with ORDER BY id DESC,
// so the ownership check behind cancel and release would act on an order nobody
// named). Both facts are right on their own and they cannot both hold: a plant
// that has run reshuffling cannot apply the index, and a plant that applied the
// index breaks the next time a compound re-plans.
//
// The resolution is that the column carries one kind of thing again. A child now
// mints a real UUID like every other order. Nothing is lost, because nothing
// read the name: the machinery finds children by parent_order_id and orders them
// by sequence (ListChildOrders / GetNextChildOrder), and the only Sscanf against
// an edge_uuid anywhere is the restore-parent's, which keeps its exemption.
//
// This test runs against a database with v71 applied — testdb migrates fully —
// so before the fix its second CreateCompoundOrder fails on the unique index,
// which is the "breaks reshuffling" direction reproduced.
func TestCompoundChild_IdentityIsMintedNotDerived(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// Two compounds under ONE parent uuid. This is the re-plan: the restore path
	// and the scanner both re-enter compound creation for a parent that already
	// has children, and the derived name collided every time.
	parent := &orders.Order{
		EdgeUUID:  "uuid-child-identity-parent",
		StationID: "line-1",
		OrderType: OrderTypeReshuffleRestore,
		Status:    StatusPending,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")
	testutil.MustNoErr(t, db.UpdateOrderStatus(parent.ID, string(StatusReshuffling), "fixture"), "set Reshuffling")
	parent, _ = db.GetOrder(parent.ID)

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-CID-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-CID-TGT")
	plan, _ := PlanReshuffleUnburyOnly(db, target, slots[1], lane, grp.ID)

	testutil.MustNoErr(t, d.CreateCompoundChildrenOnly(parent, plan), "first plan")
	if err := d.CreateCompoundChildrenOnly(parent, plan); err != nil {
		t.Fatalf("re-planning the same parent failed: %v\n"+
			"A compound that re-plans must not collide with its own earlier children. "+
			"On a database carrying v71's unique index this is where reshuffling stops.", err)
	}

	children, err := db.ListChildOrders(parent.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) < 2 {
		t.Fatalf("expected children from both plans, got %d", len(children))
	}

	seen := make(map[string]int, len(children))
	for _, c := range children {
		if c.EdgeUUID == "" {
			t.Errorf("child %d has no edge_uuid at all", c.ID)
			continue
		}
		seen[c.EdgeUUID]++
	}
	for uuid, n := range seen {
		if n > 1 {
			t.Errorf("edge_uuid %q is on %d children — this is the value that cannot be unique", uuid, n)
		}
	}

	// The name is gone, not merely made unique-ish by appending something. A
	// derived name that still looks derived invites the next reader to parse it.
	for _, c := range children {
		if len(c.EdgeUUID) != 36 {
			t.Errorf("child %d has edge_uuid %q — children mint a real UUID like every other order, "+
				"so nothing is tempted to read structure out of it", c.ID, c.EdgeUUID)
		}
	}

	// What actually locates a child, unchanged: the parent link and the order.
	for i, c := range children {
		if c.ParentOrderID == nil || *c.ParentOrderID != parent.ID {
			t.Errorf("child %d lost its parent link", c.ID)
		}
		if i > 0 && children[i-1].Sequence > c.Sequence {
			t.Errorf("children came back out of sequence order: %d before %d", children[i-1].Sequence, c.Sequence)
		}
	}
}
