package messaging

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// order_update_nudge_test.go — HandleOrderUpdate writes the ETA, the status,
// the queue reason and the fault clock, and only the status write emits (an
// order-update, when the status moved). A board that rebuilt on that event
// could read the row before the reason or the clock landed, and with no poll
// nothing would draw them. So the handler announces the order once, after
// its last write: naming the order and its process node, so only a board that
// draws that node or that order rebuilds (operator-state.js eventConcerns, the
// predicate every order-update is judged by). A global nudge here rebuilt every
// board on every order update: about 27 a minute per board on a running plant.

// orderEmissions records the order emitter's calls by name, in order.
type orderEmissions struct {
	noopEmitter
	calls *[]string
}

func (e orderEmissions) EmitOrderStatusChanged(int64, string, protocol.OrderType, string, string, string, *int64, *int64) {
	*e.calls = append(*e.calls, "status")
}
func (e orderEmissions) EmitOrderTouched(orderID int64, _ string, processNodeID *int64) {
	pn := "none"
	if processNodeID != nil {
		pn = fmt.Sprint(*processNodeID)
	}
	*e.calls = append(*e.calls, fmt.Sprintf("touched:%d:%s", orderID, pn))
}

func TestHandleOrderUpdate_AnnouncesTheOrderAfterItsLastWrite(t *testing.T) {
	t.Parallel()
	db := testHandlerDB(t)
	var calls []string
	h := NewEdgeHandler(orders.NewManager(db, orderEmissions{calls: &calls}, "edge"))
	proc, err := db.CreateProcess("NUDGE-LINE", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	node, err := db.CreateProcessNode(processes.NodeInput{ProcessID: proc, CoreNodeName: "LINE-1", Code: "LINE-1", Name: "LINE-1", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	oid, err := db.CreateOrder("uuid-nudge", protocol.OrderTypeRetrieve, &node, false, 1, "LINE-1", "", "", false, "", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	want := fmt.Sprintf("touched:%d:%d", oid, node)

	cases := []protocol.OrderUpdate{
		{OrderUUID: "uuid-nudge", Status: string(protocol.StatusQueued), QueueReason: "Waiting for material", QueueCode: "waiting_for_material"},
		// The same status again: no status event, and the reason and the clock
		// may still have moved.
		{OrderUUID: "uuid-nudge", Status: string(protocol.StatusQueued), QueueReason: "Waiting for a slot", QueueCode: "dropoff-occupied"},
	}
	for i, u := range cases {
		calls = nil
		h.HandleOrderUpdate(nil, &u)
		if len(calls) == 0 || calls[len(calls)-1] != want {
			t.Fatalf("update %d emitted %v: want %s (the order and its process node), last", i, calls, want)
		}
		n := 0
		for _, c := range calls {
			if strings.HasPrefix(c, "touched:") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("update %d emitted %v: want exactly one announcement", i, calls)
		}
	}
}
