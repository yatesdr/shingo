//go:build docker

package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// unloader_empty_out_named_bin_docker_test.go — an unloader's empty-out that
// names its bin (OrderRequest.BinID, kept as orders.named_bin_id) is typed by
// that bin before it is sourced.
//
// Core types an order's carrier (store.BinTypeForOrder) and counts what is
// coming into a levelled group (orders.CountTypedInboundToGroup). Neither read
// named_bin_id, so an empty-out was untyped until dispatch claimed its bin: the
// keeper did not count it as coming and asked for one more, and the group's
// capacity gate judged it against the group's total, so another type over its
// own level parked it.

// levelledEmptiesGroup builds a maintained group with `slots` positions and one
// level per entry of wants, with held[i] empties of that type already in it.
func levelledEmptiesGroup(t *testing.T, db *store.DB, name string, slots int,
	codes []string, wants, held []int) (*nodes.Node, []int64) {
	t.Helper()
	grpID, err := nodes.CreateGroup(db.DB, name)
	testutil.MustNoErr(t, err, "CreateGroup "+name)
	testdb.MarkGroup(t, db, grpID)
	var positions []*nodes.Node
	for i := 0; i < slots; i++ {
		n := &nodes.Node{Name: fmt.Sprintf("%s-P%02d", name, i+1), Enabled: true, ParentID: &grpID}
		testutil.MustNoErr(t, db.CreateNode(n), "position")
		positions = append(positions, n)
	}
	testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropMaintainEnabled, "on"), "enable")
	testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropMaintenanceStation, "test-core"), "station")
	next := 0
	var typeIDs []int64
	for i, code := range codes {
		var btID int64
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`, code).Scan(&btID),
			"bin type "+code)
		typeIDs = append(typeIDs, btID)
		testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{
			GroupNodeID: grpID, BinTypeID: btID, Want: wants[i]}), "level "+code)
		for j := 0; j < held[i]; j++ {
			landAt(t, db, btID, positions[next].ID, fmt.Sprintf("%s-HELD-%d", code, j))
			next++
		}
	}
	grp, err := db.GetNode(grpID)
	testutil.MustNoErr(t, err, "read the group")
	return grp, typeIDs
}

// Coming: an empty-out bound for the group, naming its carrier, not yet
// sourced (no bin_id, no hold), is counted as coming for its carrier's type,
// and its type is that carrier's.
func TestUnloaderEmptyOut_NamedBinCountsAsComingBeforeItIsSourced(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, types := levelledEmptiesGroup(t, db, "UEO-C-GRP", 4, []string{"UEO-C-TOTE-1"}, []int{2}, []int{0})
	window := &nodes.Node{Name: "UEO-C-WINDOW", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(window), "the unloader window")
	carrier := landAt(t, db, types[0], window.ID, "UEO-C-CART")

	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = protocol.OrderTypeMove
		o.SourceNode = window.Name
		o.DeliveryNode = grp.Name
		o.NamedBinID = &carrier
	})
	if o.BinID != nil {
		t.Fatalf("fixture: the empty-out already holds bin %d; this is about the window before it is sourced", *o.BinID)
	}

	coming, err := db.CountTypedInboundToGroup(grp.ID, grp.Name, "UEO-C-TOTE-1")
	testutil.MustNoErr(t, err, "count coming")
	if coming != 1 {
		t.Errorf("coming = %d, want 1: the empty-out names carrier %d, a UEO-C-TOTE-1, bound for %s",
			coming, carrier, grp.Name)
	}
	typeID, err := db.BinTypeForOrder(o.ID)
	testutil.MustNoErr(t, err, "the order's carrier type")
	if typeID == nil || *typeID != types[0] {
		t.Errorf("BinTypeForOrder = %v, want %d: the order names its carrier", typeID, types[0])
	}
}

// Not parked: the same empty-out, sent as the Edge sends it with its bin
// named, into a group that was at its level for the carrier's type when it
// arrived (it parks, correctly) and has since dropped below it, while another
// type stands over its own level. On the next passes it goes. Untyped, the
// replay judges it against the group's total (4 held of 3 wanted) and it stays
// parked "at level" for a level its carrier is short of. With the other type
// at its level instead of over it, the same order goes on the same passes
// (checked by hand at the base): the over-level type is what holds it.
func TestUnloaderEmptyOut_NamedBinIsNotParkedByAnotherTypeOverLevel(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())
	grp, types := levelledEmptiesGroup(t, db, "UEO-P-GRP", 7,
		[]string{"UEO-P-TOTE-1", "UEO-P-KD-1"}, []int{2, 1}, []int{2, 3})
	window := &nodes.Node{Name: "UEO-P-WINDOW", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(window), "the unloader window")
	carrier := landAt(t, db, types[0], window.ID, "UEO-P-CART")

	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "ueo-p-move", OrderType: protocol.OrderTypeMove,
		SourceNode: window.Name, DeliveryNode: grp.Name, Quantity: 1, BinID: carrier,
	})
	o := testdb.RequireOrder(t, db, "ueo-p-move")
	if o.Status != protocol.StatusQueued || o.DeliveryNode != grp.Name {
		t.Fatalf("fixture: the empty-out is %s to %s at intake; want it parked at %s, whose "+
			"UEO-P-TOTE-1 level (2) is met", o.Status, o.DeliveryNode, grp.Name)
	}

	// One UEO-P-TOTE-1 leaves the group: it now holds 1 of the 2 it wants.
	_, err := db.Exec(`DELETE FROM bins WHERE label='UEO-P-TOTE-1-HELD-0'`)
	testutil.MustNoErr(t, err, "one empty leaves the group")
	for range 3 {
		eng.RunFulfillmentScan()
	}

	o = testdb.RequireOrder(t, db, "ueo-p-move")
	if o.DeliveryNode == grp.Name {
		t.Errorf("the empty-out is still parked at %s (status %s, cause %q, reason %q): it carries a "+
			"UEO-P-TOTE-1 and the group holds 1 of the 2 it wants; the UEO-P-KD-1 over its own "+
			"level is not this carrier's level", grp.Name, o.Status, o.QueueCause, o.QueueReason)
	}
}
