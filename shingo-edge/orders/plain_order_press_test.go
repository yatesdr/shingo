package orders

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/orders"
)

// Every plain order the Edge makes for a cell names that cell's press on the
// wire (protocol.OrderRequest.ProcessNode), as every multi-step order does:
// the claim's Core node, read from the claim that governs the order's process
// node, which is the claim its payload is read from. An order with no process
// node, or a loader's, names none.

const ppPress = "PP-PRESS"

// wireOrderRequest is the order.request the outbox holds for uuid.
func wireOrderRequest(t *testing.T, db *store.DB, uuid string) protocol.OrderRequest {
	t.Helper()
	msgs, err := db.ListPendingOutbox(200)
	testutil.MustNoErr(t, err, "outbox")
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderRequest {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "envelope")
		var req protocol.OrderRequest
		testutil.MustNoErr(t, env.DecodePayload(&req), "payload")
		if req.OrderUUID == uuid {
			return req
		}
	}
	t.Fatalf("no order.request for %s in the outbox", uuid)
	return protocol.OrderRequest{}
}

// TestPlainOrdersNameThePressTheyServe: the retrieve and every move door, made
// for a claimed press node, carry the press on the wire.
func TestPlainOrdersNameThePressTheyServe(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")
	pid, sid, nid := seedProcessStyleNode(t, db, "PP-PROC", "PP-STYLE", ppPress)
	seedClaim(t, db, sid, ppPress, "PP-PART")
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &sid), "active style")

	for _, door := range []struct {
		name   string
		create func() (*orders.Order, error)
	}{
		{"retrieve", func() (*orders.Order, error) {
			return mgr.CreateRetrieveOrder(RetrieveSpec{Process: &nid, Delivery: ppPress, Quantity: 1, Origin: NoDemand()})
		}},
		{"empty retrieve", func() (*orders.Order, error) {
			return mgr.CreateRetrieveOrder(RetrieveSpec{Process: &nid, Delivery: ppPress, Quantity: 1, Empty: true, Origin: NoDemand()})
		}},
		{"move", func() (*orders.Order, error) {
			return mgr.CreateMoveOrder(&nid, 1, ppPress, "PP-FG", false, NoDemand())
		}},
		{"move with payload", func() (*orders.Order, error) {
			return mgr.CreateMoveOrderWithPayloadCode(&nid, 1, ppPress, "PP-FG", "PP-PART", false, NoDemand())
		}},
		{"move with a count", func() (*orders.Order, error) {
			return mgr.CreateMoveOrderWithUOP(&nid, 1, ppPress, "PP-FG", "PP-PART", nil, false, NoDemand())
		}},
		{"move carrying", func() (*orders.Order, error) {
			return mgr.CreateMoveOrderCarrying(&nid, ppPress, "PP-SUPER", "", NoDemand())
		}},
		{"move carrying to the line", func() (*orders.Order, error) {
			return mgr.CreateMoveOrderCarryingTo(&nid, "PP-SPOT", ppPress, "PP-PART", false, NoDemand())
		}},
		{"move for a bin", func() (*orders.Order, error) {
			return mgr.CreateMoveOrderForBin(&nid, ppPress, "PP-FG", "PP-PART", 7, NoDemand())
		}},
		{"move with payload for a bin", func() (*orders.Order, error) {
			return mgr.CreateMoveOrderWithPayloadCodeForBin(&nid, 1, ppPress, "PP-FG", "PP-PART", 7, false, NoDemand())
		}},
		{"hold move", func() (*orders.Order, error) {
			return mgr.CreateHoldMoveOrder(&nid, ppPress, "PP-QH", "PP-PART", 7, "operator", NoDemand())
		}},
	} {
		o, err := door.create()
		testutil.MustNoErr(t, err, door.name)
		if got := wireOrderRequest(t, db, o.UUID).ProcessNode; got != ppPress {
			t.Errorf("%s: process_node on the wire = %q, want the press %q", door.name, got, ppPress)
		}
	}
}

// TestPlainOrdersNameNoPressWithoutAClaimedCell: no process node (a person's
// move from the API), and a loader's claim, name no press.
func TestPlainOrdersNameNoPressWithoutAClaimedCell(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")
	pid, sid, nid := seedProcessStyleNode(t, db, "PP-LOADER-PROC", "PP-LOADER-STYLE", "PP-LOADER")
	// A stored manual_swap row, as one stands until the first node-list sync
	// quarantines it (the store refuses to write one now: Core owns loaders).
	_, err := db.Exec(`INSERT INTO style_node_claims (style_id, core_node_name, role, swap_mode, auto_confirm, outbound_destination)
		VALUES (?, 'PP-LOADER', 'produce', ?, 1, 'PP-SUPER')`, sid, string(protocol.SwapModeManualSwap))
	testutil.MustNoErr(t, err, "loader claim")
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &sid), "active style")

	person, err := mgr.CreateMoveOrder(nil, 1, "PP-A", "PP-B", false, NoDemand())
	testutil.MustNoErr(t, err, "a person's move")
	loader, err := mgr.CreateMoveOrderWithPayloadCode(&nid, 1, "PP-LOADER", "PP-SUPER", "PP-PART", true, NoDemand())
	testutil.MustNoErr(t, err, "a loader's move")
	for name, o := range map[string]*orders.Order{"a person's move": person, "a loader's move": loader} {
		if got := wireOrderRequest(t, db, o.UUID).ProcessNode; got != "" {
			t.Errorf("%s names press %q, want none", name, got)
		}
	}
	if got := mgr.PressOf(&nid); got != "" {
		t.Errorf("PressOf(a loader's node) = %q, want none", got)
	}
}
