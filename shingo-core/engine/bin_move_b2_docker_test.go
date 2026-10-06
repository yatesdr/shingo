//go:build docker

package engine

import (
	"slices"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// bin_move_b2_docker_test.go — the bin move through planner, builder and
// acquiring (B2), and the orc's Stop 1 and Stop 2 rulings (2026-09-26).

// historyStatuses is an order's history as its statuses, oldest first.
func historyStatuses(t *testing.T, db *store.DB, orderID int64) []protocol.Status {
	t.Helper()
	rows, err := db.ListOrderHistory(orderID)
	testutil.MustNoErr(t, err, "list history")
	out := make([]protocol.Status, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Status)
	}
	return out
}

// outboxTo counts the messages queued for a station.
func outboxTo(t *testing.T, db *store.DB, station string) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM outbox WHERE station_id=$1`, station).Scan(&n),
		"count outbox rows")
	return n
}

// TestBinMove_ToAGroupWithRoomDispatches is B2's group pin, green: the move is
// born at the child the planner chose and dispatched there. It was born at the
// group and the fleet was sent the synthetic node (base code, the same at
// 6a590181 and 0b6a7522).
func TestBinMove_ToAGroupWithRoomDispatches(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP type")
	grp := &nodes.Node{Name: "B2P-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")
	slot := &nodes.Node{Name: "B2P-SLOT-1", ParentID: &grp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")

	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "B2P-BIN")
	res, err := eng.CreateBinMove(BinMoveRequest{
		Selection: BinSelectionByLabel, BinLabel: bin.Label, DestNodeName: grp.Name, StationID: "test",
	})
	testutil.MustNoErr(t, err, "move a bin to a group with room")
	if res.Queued || res.VendorOrderID == "" || res.ToNode != slot.Name {
		t.Fatalf("result %+v, want dispatched to %s", res, slot.Name)
	}
	o, err := db.GetOrder(res.OrderID)
	testutil.MustNoErr(t, err, "reload the move")
	if o.DeliveryNode != slot.Name || o.Status != protocol.StatusDispatched {
		t.Errorf("move is %s to %s, want dispatched to %s", o.Status, o.DeliveryNode, slot.Name)
	}
}

// TestBinMove_GoesFromPendingStraightToDispatched: a move no station knows takes
// SYNTH F8's edge (the orc's Stop 2 ruling), so its history says it was born
// and dispatched, and never that it waited.
func TestBinMove_GoesFromPendingStraightToDispatched(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	createTestBinAtNode(t, db, "PART-A", storageNode.ID, "B2-EDGE")
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	res, err := eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionAuto,
		SourceNodeID: storageNode.ID, DestNodeID: lineNode.ID, StationID: "b2-edge"})
	testutil.MustNoErr(t, err, "move")
	got := historyStatuses(t, db, res.OrderID)
	want := []protocol.Status{protocol.StatusPending, protocol.StatusDispatched}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("history %v, want %v: a move that did not wait has no queued or sourcing row", got, want)
	}
}

// TestBinMove_ACoreBornMoveSendsItsStationNothing: a person's bin move projects
// to nobody and its station did not send it, so its station has no row for it
// and is sent nothing about the order: no ack or waybill at dispatch, no notice
// when it is cancelled (the orc's Stop 1 ruling, v4 §2.5). Its pickup and
// delivery are TestBinMove_ProjectionDecidesWhoHearsTheDelivery's.
func TestBinMove_ACoreBornMoveSendsItsStationNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	createTestBinAtNode(t, db, "PART-A", storageNode.ID, "B2-QUIET")
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	res, err := eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionAuto,
		SourceNodeID: storageNode.ID, DestNodeID: lineNode.ID, StationID: "b2-quiet"})
	testutil.MustNoErr(t, err, "move")
	o, err := db.GetOrder(res.OrderID)
	testutil.MustNoErr(t, err, "reload")
	if !o.StationUnaware || o.ProjectsTo != "" {
		t.Fatalf("the bin move was born unaware=%t projecting to %q; Build writes every Core-born row unaware, and a person's move projects to nobody",
			o.StationUnaware, o.ProjectsTo)
	}
	testutil.MustNoErr(t, eng.TerminateOrder(res.OrderID, "test"), "cancel the move")
	if n := outboxTo(t, db, "b2-quiet"); n != 0 {
		t.Errorf("%d message(s) queued for the move's station, want 0: it has no row to apply them to", n)
	}
}

// moveThroughDelivery births a bin move, dispatches it, and drives the fleet's
// pickup block and its delivery through the engine's own handlers. It returns
// the order as delivered.
func moveThroughDelivery(t *testing.T, eng *Engine, db *store.DB, from, to *nodes.Node, station, projects string) *orders.Order {
	t.Helper()
	res, err := eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionAuto,
		SourceNodeID: from.ID, DestNodeID: to.ID, StationID: station, Projects: projects})
	testutil.MustNoErr(t, err, "move")
	if res.Queued {
		t.Fatalf("the move parked (%s); this test needs it dispatched", res.QueueReason)
	}
	eng.handlePickupBlockCompleted(BlockCompletedEvent{OrderID: res.OrderID, BlockID: "b1",
		Location: from.Name, BinTask: "load"})
	testutil.MustNoErr(t, db.UpdateOrderStatus(res.OrderID, string(protocol.StatusDelivered), "test: delivered"), "set delivered")
	o, err := db.GetOrder(res.OrderID)
	testutil.MustNoErr(t, err, "reload")
	eng.handleOrderDelivered(o)
	bin, err := db.GetBin(*o.BinID)
	testutil.MustNoErr(t, err, "reload the bin")
	if bin.NodeID == nil || *bin.NodeID != to.ID {
		t.Fatalf("the bin is not at %s after delivery; the handlers did not run the move", to.Name)
	}
	return o
}

// messagesAbout lists, as "station msg_type", every outbox row whose payload
// names the order's reference number.
func messagesAbout(t *testing.T, db *store.DB, o *orders.Order) []string {
	t.Helper()
	rows, err := db.DB.Query(`SELECT station_id, msg_type FROM outbox
		WHERE position(convert_to($1, 'UTF8') in payload) > 0 ORDER BY id`, o.EdgeUUID)
	testutil.MustNoErr(t, err, "list messages")
	defer rows.Close()
	var out []string
	for rows.Next() {
		var st, mt string
		testutil.MustNoErr(t, rows.Scan(&st, &mt), "scan message")
		out = append(out, st+" "+mt)
	}
	testutil.MustNoErr(t, rows.Err(), "messages")
	return out
}

// TestBinMove_ProjectionDecidesWhoHearsTheDelivery: the orc's ruling on Stop
// 1's delivery line, pinned in both shapes. A move that projects to a station
// (a stage-2 pull, whose window's Edge binds the cart on arrival) sends that
// station its bin_picked_up and its order.delivered and nothing else; a Core-born
// move that projects to nobody sends no station anything, through dispatch,
// pickup and delivery.
func TestBinMove_ProjectionDecidesWhoHearsTheDelivery(t *testing.T) {
	t.Parallel()
	pickedUp := "data." + protocol.SubjectBinPickedUp

	t.Run("projects to its window's Edge", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		storageNode, lineNode, _ := setupTestData(t, db)
		createTestBinAtNode(t, db, "PART-A", storageNode.ID, "B2-WINDOW")
		eng := newTestEngine(t, db, testdb.NewTrackingBackend())

		o := moveThroughDelivery(t, eng, db, storageNode, lineNode, "b2-window", "b2-window")
		got := messagesAbout(t, db, o)
		want := []string{"b2-window " + pickedUp, "b2-window " + protocol.TypeOrderDelivered}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("messages about the move %v, want exactly %v: no ack, no waybill, no status", got, want)
		}
	})

	t.Run("projects to nobody", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		storageNode, lineNode, _ := setupTestData(t, db)
		createTestBinAtNode(t, db, "PART-A", storageNode.ID, "B2-NOBODY")
		eng := newTestEngine(t, db, testdb.NewTrackingBackend())

		o := moveThroughDelivery(t, eng, db, storageNode, lineNode, "core-operator", "")
		if got := messagesAbout(t, db, o); len(got) != 0 {
			t.Errorf("messages about the move %v, want none: it projects to nobody", got)
		}
	})
}

// TestAcquire_AStationKnownOrderKeepsItsAckAndWalksPendingToDispatched: an
// order its station knows (every door but Build's so far) is acked and
// waybilled at dispatch, and walks pending -> dispatched with no sourcing step
// between (C1: the Edge mirror accepts the edge).
func TestAcquire_AStationKnownOrderKeepsItsAckAndWalksPendingToDispatched(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, "PART-A", storageNode.ID, "B2-KNOWN")
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	// Born and acquired under the scan lock, as a door does it (Scanner.Door). A
	// row inserted outside it is the started engine's to find, and its pass can
	// dispatch the order before AcquireOne reaches it: the test then fails on a
	// history it never made (C2a's source report, finding 5).
	var known *orders.Order
	var dispatched bool
	eng.fulfillment.Door(func() {
		known = testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.StationID = "b2-known"
			o.OrderType = protocol.OrderTypeMove
			o.Status = protocol.StatusPending
			o.SourceNode = storageNode.Name
			o.DeliveryNode = lineNode.Name
			o.BinID = &bin.ID
			o.SourceIntent = "local"
		})
		testdb.ReserveBin(t, db, known.ID, bin.ID)
		dispatched = eng.fulfillment.AcquireLocked(known.ID, nil)
	})

	if !dispatched {
		t.Fatalf("the known order did not dispatch; history %v", historyStatuses(t, db, known.ID))
	}
	got := historyStatuses(t, db, known.ID)
	if want := []protocol.Status{protocol.StatusPending, protocol.StatusDispatched}; !slices.Equal(got, want) {
		t.Errorf("history %v, want %v: a station-known order takes pending -> dispatched", got, want)
	}
	var acks int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM outbox WHERE station_id='b2-known'
		AND msg_type IN ('order.ack','order.waybill')`).Scan(&acks), "count acks")
	if acks != 2 {
		t.Errorf("%d ack/waybill message(s) for the known order's station, want 2", acks)
	}
}

// TestAcquireOne_TriesOneOrderAndNotTheWaitingSet: a door's acquire is one act
// on its own order (the orc's Stop 2 ruling). A waiting order a pass would
// re-ask is left exactly as it was.
func TestAcquireOne_TriesOneOrderAndNotTheWaitingSet(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	createTestBinAtNode(t, db, "PART-A", storageNode.ID, "B2-ONE")
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	created := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.StationID = "b2-waiting"
		o.PayloadCode = "NO-SUCH-PART"
		o.DeliveryNode = lineNode.Name
		o.SourceIntent = "full"
	})
	waiting, err := db.GetOrder(created.ID)
	testutil.MustNoErr(t, err, "read the waiting order as stored")
	res, err := eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionAuto,
		SourceNodeID: storageNode.ID, DestNodeID: lineNode.ID, StationID: "b2-one"})
	testutil.MustNoErr(t, err, "move")
	if res.Queued {
		t.Fatalf("the move parked (%s); this test needs it dispatched", res.QueueReason)
	}
	after, err := db.GetOrder(waiting.ID)
	testutil.MustNoErr(t, err, "reload the waiting order")
	if after.QueueCause != "" || !after.UpdatedAt.Equal(waiting.UpdatedAt) {
		t.Errorf("the waiting order was re-asked (cause %q, updated %v -> %v): the move's acquire walked the waiting set",
			after.QueueCause, waiting.UpdatedAt, after.UpdatedAt)
	}
}
