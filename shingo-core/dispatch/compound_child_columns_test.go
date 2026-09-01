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

// TestCompoundChild_PersistsEveryFieldSetOnIt pins a property that sounds too
// obvious to test: a field you set on an order is a field that comes back when
// you read it.
//
// It did not hold for compound children. They were written by a second, separate
// INSERT statement carrying a shorter column list than the shared one, so five
// fields — payload_code, skip_auto_confirm, sibling_order_uuid, source_intent,
// coordinated — were silently dropped on the floor. No error, no warning; the
// assignment simply did nothing.
//
// Nothing was losing data in practice, because the one place that builds compound
// children happens to set none of those five. That is luck, not design: the next
// person to set one would have found out the hard way, and the file carried a
// comment saying as much.
//
// NOTE ON THE FIXTURE: this child is a persistence fixture, not a valid dispatch
// shape. A plain Move order marked coordinated would trip
// AssertSimpleNotCoordinated if it ever reached the scanner. It never does here —
// the test writes a row and reads it back. Do not copy this shape into a test
// that dispatches.
func TestCompoundChild_PersistsEveryFieldSetOnIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	src := &nodes.Node{Name: "CCC-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "create src")
	childBin := testdb.CreateBinAtNode(t, db, bp.Code, src.ID, "CCC-BIN")

	parent := &orders.Order{
		EdgeUUID: "ccc-parent", StationID: "edge.ccc", OrderType: OrderTypeComplex,
		Status: StatusReshuffling, Quantity: 1, PayloadCode: bp.Code,
		SourceNode: src.Name, DeliveryNode: lineNode.Name, Coordinated: true,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")

	child := &orders.Order{
		EdgeUUID: "ccc-child", StationID: "edge.ccc", OrderType: OrderTypeMove,
		Status: StatusPending, Quantity: 1,
		SourceNode: src.Name, DeliveryNode: lineNode.Name,
		ParentOrderID: &parent.ID, Sequence: 1, BinID: &childBin.ID,

		// The five the short INSERT dropped. Each is set to a value that is
		// distinguishable from the column default, so a dropped write reads back
		// as the zero value and fails loudly rather than looking plausible.
		PayloadCode:      bp.Code,
		SkipAutoConfirm:  true,
		SiblingOrderUUID: "ccc-sibling-uuid",
		SourceIntent:     SourceIntentLocal,
		Coordinated:      true,
	}
	_, ccErr := db.CreateCompoundChildren([]store.CompoundChild{{Order: child, BinID: childBin.ID}})
	testutil.MustNoErr(t, ccErr, "create child")

	kids, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list children")
	if len(kids) != 1 {
		t.Fatalf("expected exactly 1 child, got %d", len(kids))
	}
	got := kids[0]

	for _, c := range []struct {
		column string
		want   any
		got    any
	}{
		{"payload_code", bp.Code, got.PayloadCode},
		{"skip_auto_confirm", true, got.SkipAutoConfirm},
		{"sibling_order_uuid", "ccc-sibling-uuid", got.SiblingOrderUUID},
		{"source_intent", SourceIntentLocal, got.SourceIntent},
		{"coordinated", true, got.Coordinated},
	} {
		if c.got != c.want {
			t.Errorf("child column %s = %v, want %v — the value was set on the struct and did not survive the write",
				c.column, c.got, c.want)
		}
	}
}
