package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// TestEcho_KeepsTheEdgesOwnProcessNode: Core projects an Edge-created order back
// to the station that sent it. The Edge already answered which process node the
// order belongs to when it created the row; the echo's delivery-node guess must
// not overwrite that answer. It fills process_node_id only on a row that has
// none — a Core-authored order's first insert.
//
// The type rename the echo carries (retrieve + flag → retrieve_empty) is KNOWN
// AND TOLERATED: the confirm lookup matches either spelling
// (ListDeliveredRetrieveByDeliveryNode), so it is recorded here, not refused.
func TestEcho_KeepsTheEdgesOwnProcessNode(t *testing.T) {
	t.Parallel()
	n := newEchoNodes(t, "EKP", protocol.ClaimRoleProduce, "BKP")

	l1, err := n.eng.orderMgr.CreateRetrieveOrder(&n.aID, true, 1, n.bCore, "EMPTY-MKT", "",
		"standard", "", false, true, orders.NoDemand())
	testutil.MustNoErr(t, err, "CreateRetrieveOrder")
	_, err = n.eng.ApplyOrderProjection(echoProjectionOf(l1))
	testutil.MustNoErr(t, err, "apply echo")

	row, err := n.db.GetOrder(l1.ID)
	testutil.MustNoErr(t, err, "reload")
	got := int64(0)
	if row.ProcessNodeID != nil {
		got = *row.ProcessNodeID
	}
	if got != n.aID {
		t.Errorf("process_node_id after the echo = %d, want the creator %d — the echo resolved it "+
			"from the delivery node (%s = %d) and overwrote the Edge's own answer", got, n.aID, n.bCore, n.bID)
	}
	if row.OrderType != protocol.OrderTypeRetrieveEmpty || !row.RetrieveEmpty {
		t.Errorf("order type after the echo = %q retrieve_empty=%v; the rename to retrieve_empty is "+
			"expected and tolerated (the confirm lookup matches either spelling)", row.OrderType, row.RetrieveEmpty)
	}
}
