//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// planTransport's synthetic-destination re-resolution, and who it covers.
//
// An order whose delivery node names a GROUP must leave planning with a
// CONCRETE child on it. Nothing downstream will do it: the fulfillment scanner
// reads order.DeliveryNode and looks it up with GetNodeByDotName, which FINDS a
// synthetic group — it is a real row — so there is no error to catch and no
// second resolver. A group name that survives planning reaches the fleet as a
// node no robot can drive to.

// ngrpWithFreeSlot builds a real NGRP with one enabled, empty child.
func ngrpWithFreeSlot(t *testing.T, db *store.DB, group, child string) (*nodes.Node, *nodes.Node) {
	t.Helper()
	ngrpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "get NGRP type")
	grp := &nodes.Node{Name: group, IsSynthetic: true, Enabled: true, NodeTypeID: &ngrpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")
	slot := &nodes.Node{Name: child, Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(slot), "create child")
	return grp, slot
}

// THE MG2-9 FIX. Before it, this branch asked isMove, so an empty carried its
// group name all the way to the fleet.
func TestPlanTransport_EmptyToAGroupLeavesWithAConcreteSlot(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	storageNode, _, _ := setupTestData(t, db)
	bt, err := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, err, "bin type")

	grp, slot := ngrpWithFreeSlot(t, db, "MG29-EMPTY-GRP", "MG29-EMPTY-SLOT")

	// An empty carrier to source, so resolution is the only thing under test.
	empty := &bins.Bin{BinTypeID: bt.ID, Label: "MG29-EMPTY-BIN", NodeID: &storageNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(empty), "create empty bin")

	// A REAL resolver. newTestDispatcher passes nil, which makes this whole
	// branch unreachable and the test vacuous.
	d := NewDispatcher(db, testdb.NewTrackingBackend(), &mockEmitter{}, "core",
		"shingo.dispatch", &DefaultResolver{DB: db})
	order := &orders.Order{
		EdgeUUID: "mg29-empty-to-group", StationID: "line-1", OrderType: OrderTypeRetrieveEmpty,
		SourceIntent: SourceIntentEmpty, Status: StatusPending, Quantity: 1,
		DeliveryNode: grp.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planTransport(order, testEnvelope(), "")
	if perr != nil {
		t.Fatalf("planTransport: %s", perr.Detail)
	}
	if !result.Queued {
		t.Fatal("the claim-move contract: planning queues and the scanner claims")
	}

	if order.DeliveryNode == grp.Name {
		t.Fatalf("delivery node is still the group %q. Nothing downstream resolves it — the "+
			"scanner's GetNodeByDotName FINDS a synthetic group and reports no error — so this "+
			"order reaches the fleet naming a node no robot can drive to", grp.Name)
	}
	if order.DeliveryNode != slot.Name {
		t.Errorf("delivery node = %q, want the group's one free child %q", order.DeliveryNode, slot.Name)
	}

	// AND IT IS PERSISTED. The in-memory field is not what the scanner reads.
	stored, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload order")
	if stored.DeliveryNode != slot.Name {
		t.Errorf("stored delivery_node = %q, want %q — the resolution has to reach the row the "+
			"scanner dispatches from, not just the struct planning was holding",
			stored.DeliveryNode, slot.Name)
	}
}

// THE REFUSAL WAS DELIBERATELY NOT WIDENED with the resolution. A move names its
// own source, so a same-node move is the operator asking for something
// impossible and failing it is right. An empty's source is chosen by the finder,
// so the same condition would be Core refusing an order over its own selection —
// a new behaviour, with no evidence behind it, riding along inside a fix.
func TestPlanTransport_SameNodeRefusalStaysMoveOnly(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, _, _ = setupTestData(t, db)
	bt, err := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, err, "bin type")

	grp, slot := ngrpWithFreeSlot(t, db, "MG29-SAME-GRP", "MG29-SAME-SLOT")

	// The only empty in the plant sits at the very slot the group will resolve
	// to, so source and destination come out identical.
	empty := &bins.Bin{BinTypeID: bt.ID, Label: "MG29-SAME-BIN", NodeID: &slot.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(empty), "create empty bin")

	// A REAL resolver. newTestDispatcher passes nil, which makes this whole
	// branch unreachable and the test vacuous.
	d := NewDispatcher(db, testdb.NewTrackingBackend(), &mockEmitter{}, "core",
		"shingo.dispatch", &DefaultResolver{DB: db})
	order := &orders.Order{
		EdgeUUID: "mg29-same-node", StationID: "line-1", OrderType: OrderTypeRetrieveEmpty,
		SourceIntent: SourceIntentEmpty, Status: StatusPending, Quantity: 1,
		DeliveryNode: grp.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	_, perr := d.planner.planTransport(order, testEnvelope(), "")
	if perr != nil && perr.Code == codeSameNode {
		t.Fatalf("an empty was refused with %s. That refusal is scoped to moves on purpose: "+
			"the finder chose this source, so failing the order blames the operator for Core's "+
			"own selection", codeSameNode)
	}
}
