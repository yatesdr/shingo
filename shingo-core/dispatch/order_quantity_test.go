//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// TestMoveOrder_CountsOneBin pins that a move says it is moving one bin.
//
// A robot carries one bin at a time. There is no multi-bin order anywhere in the
// system: the batch feature creates N separate orders precisely because one
// order cannot carry N, and order_bins binds one bin per STEP, not a count.
//
// Two ways the column was lying. The engineer's bin-move door built its order
// without the field at all, so those rows stored 0 — and because the INSERT
// names the column explicitly, the table's DEFAULT 1 never applied. The order
// screen rendered "qty 0". Separately, intake copies whatever the Edge sent, and
// the Edge will happily send 4 on a move: one order, one bin, stored as 4,
// displayed as 4, confirmed as 4.
//
// Nothing in Core reads the value, which is why neither showed up as a failure.
// The one reader in the product is the line of the order screen that prints it —
// so the only thing this column has ever done is tell a person a number, and the
// number was wrong.
func TestMoveOrder_CountsOneBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// The Edge asks for four of something that cannot come in fours.
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "uuid-qty-move",
		OrderType:    OrderTypeMove,
		PayloadCode:  bp.Code,
		Quantity:     4,
		SourceNode:   lineNode.Name,
		DeliveryNode: lineNode.Name,
	})

	got, err := db.GetOrderByUUID("uuid-qty-move")
	testutil.MustNoErr(t, err, "read back the move")
	if got.Quantity != 1 {
		t.Errorf("move stored quantity = %d, want 1. One robot carries one bin; the row is describing a delivery that cannot happen.", got.Quantity)
	}
}

// TestRetrieveOrder_KeepsTheCountItWasAskedFor is the other half: the floor is
// for moves, not for everything.
//
// A retrieve's count is meaningful on the Edge side — it is the number declared
// back on confirm — and the batch path reads it to decide how many separate
// orders to create. Flooring it everywhere would break both.
func TestRetrieveOrder_KeepsTheCountItWasAskedFor(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "uuid-qty-retrieve",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		Quantity:     3,
		DeliveryNode: lineNode.Name,
	})

	got, err := db.GetOrderByUUID("uuid-qty-retrieve")
	testutil.MustNoErr(t, err, "read back the retrieve")
	if got.Quantity != 3 {
		t.Errorf("retrieve stored quantity = %d, want 3 — the count is the Edge's to declare on this type", got.Quantity)
	}
}
