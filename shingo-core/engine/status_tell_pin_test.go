//go:build docker

package engine

// status_tell_pin_test.go pins what Core tells a station today when an order's
// status is written: for each status writer, the exact outbox rows it adds for
// that order, as "msg_type status station" (status is the order.update's, blank
// on any other message).
//
// These are characterisation pins. They assert what the code does at this tree,
// not what it should do. Core is about to queue one order.update for the station
// inside every status write's own transaction, drop the per-call-site status
// sends, and give each order a version; each of those changes is expected to
// flip some of these on purpose, and each pin's comment says which change flips
// it.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// pinStation is the station every order in this file belongs to: testEnvelope's
// source, which the intake doors stamp on the order, and the station the
// hand-made orders here are given.
const pinStation = "line-1"

// outboxMark is the newest outbox row's id. The rows a step adds are the rows
// after it.
func outboxMark(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM outbox`).Scan(&id), "read outbox mark")
	return id
}

// tellsAfter lists, oldest first, every outbox row after mark whose payload
// names the order (order_uuid), sent or not, as "msg_type status station". The
// status is the one an order.update carries and is blank on every other message;
// the station is the row's own station_id, the one the drainer routes on.
func tellsAfter(t *testing.T, db *store.DB, edgeUUID string, mark int64) []string {
	t.Helper()
	rows, err := db.DB.Query(`SELECT msg_type, station_id, payload FROM outbox WHERE id > $1 ORDER BY id`, mark)
	testutil.MustNoErr(t, err, "read outbox")
	defer rows.Close()
	var out []string
	for rows.Next() {
		var msgType, station string
		var payload []byte
		testutil.MustNoErr(t, rows.Scan(&msgType, &station, &payload), "scan outbox row")
		var env struct {
			P json.RawMessage `json:"p"`
		}
		var body struct {
			OrderUUID string `json:"order_uuid"`
			Status    string `json:"status"`
		}
		if json.Unmarshal(payload, &env) != nil || json.Unmarshal(env.P, &body) != nil || body.OrderUUID != edgeUUID {
			continue
		}
		status := ""
		if msgType == protocol.TypeOrderUpdate {
			status = body.Status
		}
		out = append(out, fmt.Sprintf("%s %s %s", msgType, status, station))
	}
	testutil.MustNoErr(t, rows.Err(), "read outbox rows")
	return out
}

// updatesAfter is tellsAfter's order.update rows decoded, for the fields a row
// line does not show (robot, ETA, fault clock, queue sentence).
func updatesAfter(t *testing.T, db *store.DB, edgeUUID string, mark int64) []protocol.OrderUpdate {
	t.Helper()
	rows, err := db.DB.Query(`SELECT payload FROM outbox WHERE id > $1 AND msg_type = $2 ORDER BY id`,
		mark, protocol.TypeOrderUpdate)
	testutil.MustNoErr(t, err, "read outbox")
	defer rows.Close()
	var out []protocol.OrderUpdate
	for rows.Next() {
		var payload []byte
		testutil.MustNoErr(t, rows.Scan(&payload), "scan outbox row")
		var env struct {
			P protocol.OrderUpdate `json:"p"`
		}
		if json.Unmarshal(payload, &env) == nil && env.P.OrderUUID == edgeUUID {
			out = append(out, env.P)
		}
	}
	testutil.MustNoErr(t, rows.Err(), "read outbox rows")
	return out
}

// requireTells fails unless the rows after mark for the order are exactly want,
// in order. No want means the step told the station nothing.
func requireTells(t *testing.T, db *store.DB, edgeUUID string, mark int64, want ...string) {
	t.Helper()
	got := tellsAfter(t, db, edgeUUID, mark)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("outbox rows for %s:\n got  %q\n want %q", edgeUUID, got, want)
	}
}

// pinRetrieve is a plain retrieve, storage to line, dispatched at intake on a
// simulator. It returns the outbox mark from before the intake, so the dispatch's
// own rows can be read too.
func pinRetrieve(t *testing.T, uuid string) (*store.DB, *Engine, *simulator.SimulatorBackend, *orders.Order, int64) {
	t.Helper()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-"+uuid)
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	before := outboxMark(t, db)
	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    uuid,
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sd.Payload.Code,
		DeliveryNode: sd.LineNode.Name,
		Quantity:     1,
	})
	order := testdb.RequireOrderStatus(t, db, uuid, dispatch.StatusDispatched)
	if order.StationID != pinStation {
		t.Fatalf("the retrieve belongs to station %q, want %q — the station column below assumes it", order.StationID, pinStation)
	}
	return db, eng, sim, order, before
}

// pinOrder is a hand-made order on pinStation, for writers that need no robot.
func pinOrder(t *testing.T, db *store.DB, uuid string, status protocol.Status, opts ...func(*orders.Order)) *orders.Order {
	t.Helper()
	return testdb.CreateOrder(t, db, append([]func(*orders.Order){func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.StationID = pinStation
		o.Status = status
	}}, opts...)...)
}

// TestStatusTellPin_RowsPerTypedWriter drives each status writer once on an
// order that fits it and pins the outbox rows that step adds for the order.
//
// Today the tell is not the write's: it is sent by whoever called the writer
// (the vendor-status handler, an event subscriber, the scanner), so a writer
// called from somewhere that sends nothing tells the station nothing. Every row
// below flips when Core tells the station from the status write itself: the
// silent ones gain the one order.update the write queues, and the ones that send
// today change to whatever the build keeps (the per-call-site order.update
// retires; whether order.staged, order.delivered, order.cancelled, order.error,
// order.skipped and the waybill stay is the build's call, and these say what
// they are now). The per-order version flips every order.update row here when it
// rides the payload; these rows compare type, status and station only.
func TestStatusTellPin_RowsPerTypedWriter(t *testing.T) {
	t.Parallel()

	// A plain dispatch at intake: the scanner's ack and waybill, no
	// order.update (fulfillment/scanner.go notifyEdgeDispatched). The status
	// write itself (transitionTx on the hand's transaction) sends nothing.
	t.Run("plain dispatch at intake", func(t *testing.T) {
		t.Parallel()
		db, _, _, order, before := pinRetrieve(t, "tell-plain")
		requireTells(t, db, order.EdgeUUID, before,
			"order.ack  line-1",
			"order.waybill  line-1")
	})

	// Fleet RUNNING on a dispatched order (MarkInTransit through the
	// vendor-status handler): the first-robot waybill, then order.update
	// in_transit carrying the robot and an ETA.
	t.Run("fleet RUNNING", func(t *testing.T) {
		t.Parallel()
		db, _, sim, order, _ := pinRetrieve(t, "tell-running")
		mark := outboxMark(t, db)
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusInTransit)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.waybill  line-1",
			"order.update in_transit line-1")
		u := updatesAfter(t, db, order.EdgeUUID, mark)
		if len(u) != 1 || u[0].RobotID != "AMB-01" || u[0].Detail != "fleet state: RUNNING" {
			t.Fatalf("in_transit update = %+v, want robot AMB-01 and detail %q", u, "fleet state: RUNNING")
		}
		if _, err := time.Parse(time.RFC3339, u[0].ETA); err != nil {
			t.Errorf("in_transit update ETA = %q, want an RFC 3339 stamp (the empty medians cache falls back to its default): %v", u[0].ETA, err)
		}
		if u[0].FaultSince != nil || u[0].FaultRef != nil {
			t.Errorf("in_transit update carries fault fields: %+v", u[0])
		}
	})

	// Fleet WAITING on an in_transit order (MarkStaged): order.update staged,
	// then order.staged.
	t.Run("fleet WAITING", func(t *testing.T) {
		t.Parallel()
		db, _, sim, order, _ := pinRetrieve(t, "tell-waiting")
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		mark := outboxMark(t, db)
		sim.DriveState(order.VendorOrderID, "WAITING")
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusStaged)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.update staged line-1",
			"order.staged  line-1")
		if u := updatesAfter(t, db, order.EdgeUUID, mark); len(u) != 1 || u[0].RobotID != "AMB-01" || u[0].ETA != "" {
			t.Errorf("staged update = %+v, want robot AMB-01 and no ETA", u)
		}
	})

	// Fleet FINISHED on an in_transit order (MarkDelivered): order.delivered is
	// queued first (handleOrderDelivered runs before MarkDelivered), then
	// order.update delivered. No order.update confirmed: MarkDelivered's
	// completion event carries no station, and the completion notice is gated
	// on one. TestStatusTellPin_DeliveredIsNotToldConfirmed pins that gate on
	// its own.
	t.Run("fleet FINISHED", func(t *testing.T) {
		t.Parallel()
		db, _, sim, order, _ := pinRetrieve(t, "tell-finished")
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		mark := outboxMark(t, db)
		sim.DriveState(order.VendorOrderID, "FINISHED")
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusDelivered)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.delivered  line-1",
			"order.update delivered line-1")
	})

	// ConfirmReceipt on a delivered order: the completion notice, order.update
	// confirmed (engine/wiring.go, the EventOrderCompleted subscriber; the
	// receipt's event carries the station).
	t.Run("ConfirmReceipt", func(t *testing.T) {
		t.Parallel()
		db, eng, sim, order, _ := pinRetrieve(t, "tell-confirm")
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		sim.DriveState(order.VendorOrderID, "FINISHED")
		delivered := testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusDelivered)
		mark := outboxMark(t, db)
		ok, err := eng.Dispatcher().Lifecycle().ConfirmReceipt(delivered, delivered.StationID, "confirmed", 1)
		testutil.MustNoErr(t, err, "confirm receipt")
		if !ok {
			t.Fatal("ConfirmReceipt reported the order already completed")
		}
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusConfirmed)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.update confirmed line-1")
		if u := updatesAfter(t, db, order.EdgeUUID, mark); len(u) != 1 || u[0].Detail != "confirmed at Core" || u[0].RobotID != "AMB-01" {
			t.Errorf("confirmed update = %+v, want detail %q and robot AMB-01", u, "confirmed at Core")
		}
	})

	// CancelOrder on a dispatched order: order.cancelled from the cancellation
	// subscriber, no order.update.
	t.Run("CancelOrder", func(t *testing.T) {
		t.Parallel()
		db, eng, _, order, _ := pinRetrieve(t, "tell-cancel")
		mark := outboxMark(t, db)
		eng.Dispatcher().Lifecycle().CancelOrder(order, order.StationID, "pin: cancelled", dispatch.CancelCause{})
		eng.after.settle()
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusCancelled)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.cancelled  line-1")
	})

	// Fail on a dispatched order: order.error from the failure subscriber, no
	// order.update.
	t.Run("Fail", func(t *testing.T) {
		t.Parallel()
		db, eng, _, order, _ := pinRetrieve(t, "tell-fail")
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, eng.Dispatcher().Lifecycle().Fail(order, order.StationID, "pin_failed", "pin: failed"), "fail")
		eng.after.settle()
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusFailed)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.error  line-1")
	})

	// Skip on a queued order: order.skipped from the skip subscriber, no
	// order.update.
	t.Run("Skip", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		eng := newTestEngine(t, db, simulator.New())
		o := pinOrder(t, db, "tell-skip", protocol.StatusQueued)
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, eng.Dispatcher().Lifecycle().Skip(o, pinStation, "pin_skipped", "pin: never needed"), "skip")
		eng.after.settle()
		testdb.RequireOrderStatus(t, db, o.EdgeUUID, dispatch.StatusSkipped)
		requireTells(t, db, o.EdgeUUID, mark,
			"order.skipped  line-1")
	})

	// Fleet FAILED on an in_transit order (MarkFaulted): order.update faulted
	// with the fault clock, the threshold and the fault's reference.
	t.Run("fleet FAILED (MarkFaulted)", func(t *testing.T) {
		t.Parallel()
		db, _, sim, order, _ := pinRetrieve(t, "tell-faulted")
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		mark := outboxMark(t, db)
		sim.DriveState(order.VendorOrderID, "FAILED")
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusFaulted)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.update faulted line-1")
		u := updatesAfter(t, db, order.EdgeUUID, mark)
		if len(u) != 1 {
			t.Fatalf("faulted updates = %+v, want one", u)
		}
		f := u[0]
		if f.FaultSince == nil || f.FaultDeadline == nil || !f.FaultDeadline.After(*f.FaultSince) {
			t.Errorf("faulted update clock: since %v deadline %v, want both set and the deadline later", f.FaultSince, f.FaultDeadline)
		}
		if f.FaultNoticeAfterS <= 0 || f.FaultNotice {
			t.Errorf("faulted update threshold %d s, notice %v; want the configured threshold and no notice yet", f.FaultNoticeAfterS, f.FaultNotice)
		}
		if f.FaultRef == nil || f.FaultRef.Node != order.DeliveryNode {
			t.Errorf("faulted update ref = %+v, want the order's node %q", f.FaultRef, order.DeliveryNode)
		}
		if f.RobotID != "AMB-01" || f.Detail == "" || f.Detail == "fleet state: FAILED" || f.ETA != "" {
			t.Errorf("faulted update robot %q detail %q eta %q; want AMB-01, the fault sentence, no ETA", f.RobotID, f.Detail, f.ETA)
		}
	})

	// Fleet RUNNING on a faulted order (MarkFaultedRecovered): order.update
	// in_transit, no fault fields, an ETA, no second waybill.
	t.Run("fleet RUNNING after faulted (recovery)", func(t *testing.T) {
		t.Parallel()
		db, _, sim, order, _ := pinRetrieve(t, "tell-recovered")
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		sim.DriveState(order.VendorOrderID, "FAILED")
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusFaulted)
		mark := outboxMark(t, db)
		sim.DriveState(order.VendorOrderID, "RUNNING")
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusInTransit)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.update in_transit line-1")
		u := updatesAfter(t, db, order.EdgeUUID, mark)
		if len(u) != 1 || u[0].RobotID != "AMB-01" || u[0].ETA == "" || u[0].FaultSince != nil || u[0].FaultRef != nil {
			t.Errorf("recovery update = %+v, want robot AMB-01, an ETA and no fault fields", u)
		}
		h, err := db.LatestOrderHistoryForStatus(order.ID, dispatch.StatusInTransit)
		testutil.MustNoErr(t, err, "read in_transit history row")
		if h == nil || !strings.HasPrefix(h.Detail, "Recovered") {
			t.Errorf("latest in_transit history row = %+v, want the recovery sentence (this is MarkFaultedRecovered, not MarkInTransit)", h)
		}
	})

	// Release on a staged order: the write tells nothing, and neither does the
	// fleet's RUNNING that follows, which maps to the status Core already wrote
	// and takes the same-status return (engine/wiring_vendor_status.go). The
	// station hears of the release from nobody.
	t.Run("Release, then the fleet's RUNNING", func(t *testing.T) {
		t.Parallel()
		db, eng, sim, order, _ := pinRetrieve(t, "tell-release")
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		sim.DriveState(order.VendorOrderID, "WAITING")
		staged := testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusStaged)
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, eng.Dispatcher().Lifecycle().Release(staged, "dispatcher"), "release")
		eng.after.settle()
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusInTransit)
		requireTells(t, db, order.EdgeUUID, mark)
		sim.DriveState(order.VendorOrderID, "RUNNING")
		got := testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusInTransit)
		if got.VendorState != "RUNNING" {
			t.Errorf("vendor_state = %q after the fleet's RUNNING, want RUNNING", got.VendorState)
		}
		requireTells(t, db, order.EdgeUUID, mark)
	})

	// BeginReshuffle on a queued order: no action fires on the way into
	// reshuffling, and the write tells nothing.
	t.Run("BeginReshuffle", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		eng := newTestEngine(t, db, simulator.New())
		o := pinOrder(t, db, "tell-reshuffle", protocol.StatusQueued)
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, eng.Dispatcher().Lifecycle().BeginReshuffle(o, "pin: the dig begins"), "begin reshuffle")
		eng.after.settle()
		testdb.RequireOrderStatus(t, db, o.EdgeUUID, dispatch.StatusReshuffling)
		requireTells(t, db, o.EdgeUUID, mark)
	})

	// Restart on a parent in reshuffling (the fixture of
	// TestResume_TheParentGoesBackToPendingAndAcquiresOnce). The write
	// (reshuffling -> pending) tells nothing of its own; its one acquire runs on
	// the after-queue and parks the parent again, and that park tells the
	// station `queued` with the cause. The one row below is the acquire's, not
	// Restart's. The station is never told `pending`.
	t.Run("Restart", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		sd := testdb.SetupStandardData(t, db)
		grp, _, _ := closedGroup(t, db, "TELLRS", sd.Payload.Code, 1)
		testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "TELLRS-STORED")
		eng := newTestEngine(t, db, simulator.New())
		eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), closedGroupStore("tell-restart-shape", sd, grp))
		shape := testdb.RequireOrder(t, db, "tell-restart-shape")
		parent := &orders.Order{EdgeUUID: "tell-restart", StationID: shape.StationID, OrderType: shape.OrderType,
			Status: protocol.StatusReshuffling, Quantity: 1, PayloadCode: shape.PayloadCode,
			SourceNode: shape.SourceNode, DeliveryNode: shape.DeliveryNode, StepsJSON: shape.StepsJSON,
			Coordinated: true}
		testutil.MustNoErr(t, db.CreateOrder(parent), "the parent in reshuffling")
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, eng.Dispatcher().Lifecycle().Restart(parent, "pin: the dig is over"), "restart")
		eng.after.settle()
		after := testdb.RequireOrderStatus(t, db, parent.EdgeUUID, dispatch.StatusQueued)
		requireTells(t, db, parent.EdgeUUID, mark,
			"order.update queued line-1")
		if u := updatesAfter(t, db, parent.EdgeUUID, mark); len(u) != 1 || u[0].QueueReason != after.QueueReason ||
			u[0].QueueCode != after.QueueCode || u[0].QueueReason == "" {
			t.Errorf("queued update = %+v, want the cause the acquire parked it under (%s, %q)", u, after.QueueCode, after.QueueReason)
		}
	})

	// A coordinated dispatch: a complex order parked at intake for want of a bin,
	// dispatched by the next scan once the bin is there. The dispatch (the
	// hand's transitionTx, coordinated tail) tells the station nothing: no ack,
	// no waybill, no order.update. The station first hears of it at the fleet's
	// RUNNING.
	t.Run("coordinated dispatch by a scan", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		sd := testdb.SetupStandardData(t, db)
		eng := newTestEngine(t, db, simulator.New())
		eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
			OrderUUID:   "tell-coordinated",
			PayloadCode: sd.Payload.Code,
			Quantity:    1,
			Steps: []protocol.ComplexOrderStep{
				{Action: "pickup", Node: sd.StorageNode.Name},
				{Action: "dropoff", Node: sd.LineNode.Name},
			},
		})
		parked := testdb.RequireOrderStatus(t, db, "tell-coordinated", dispatch.StatusQueued)
		if !parked.Coordinated || parked.StationID != pinStation {
			t.Fatalf("the complex order is coordinated=%v on station %q, want a coordinated order on %q",
				parked.Coordinated, parked.StationID, pinStation)
		}
		mark := outboxMark(t, db)
		testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "TELLCO-BIN")
		eng.RunFulfillmentScan()
		eng.after.settle()
		testdb.RequireOrderStatus(t, db, "tell-coordinated", dispatch.StatusDispatched)
		requireTells(t, db, "tell-coordinated", mark)
	})

	// Force-recover (RecoveryService.RecoverFaultedOrder, which has no
	// production caller): MarkInTransit straight from faulted. Its recovery
	// event reaches the email notifier only; the station is told nothing.
	t.Run("RecoverFaultedOrder", func(t *testing.T) {
		t.Parallel()
		db, eng, sim, order, _ := pinRetrieve(t, "tell-force-recover")
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		sim.DriveState(order.VendorOrderID, "FAILED")
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusFaulted)
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, eng.RecoverFaultedOrder(order.ID, "pin-operator"), "recover faulted order")
		eng.after.settle()
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusInTransit)
		requireTells(t, db, order.EdgeUUID, mark)
	})

	// A compound child failed the way dispatch/compound.go fails one: a bare
	// db.FailOrderAtomic, past transition(), no event. Nobody is told, child or
	// parent.
	t.Run("compound child FailOrderAtomic", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		newTestEngine(t, db, simulator.New())
		parent := pinOrder(t, db, "tell-atomic-parent", protocol.StatusReshuffling, func(o *orders.Order) {
			o.OrderType = protocol.OrderTypeComplex
		})
		child := pinOrder(t, db, "tell-atomic-child", protocol.StatusPending, func(o *orders.Order) {
			o.ParentOrderID = &parent.ID
			o.OrderType = protocol.OrderTypeMove
		})
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, db.FailOrderAtomic(child.ID, "missing source node"), "fail the child")
		testdb.RequireOrderStatus(t, db, child.EdgeUUID, dispatch.StatusFailed)
		requireTells(t, db, child.EdgeUUID, mark)
		requireTells(t, db, parent.EdgeUUID, mark)
	})

	// The boot sweep of leftover reshuffle_restore orders cancels parent and
	// child through TerminalizeOrder, past transition(), no event. Nobody is
	// told.
	t.Run("RetireReshuffleRestoreOrders", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		newTestEngine(t, db, simulator.New())
		parent := pinOrder(t, db, "tell-retire-parent", protocol.StatusReshuffling, func(o *orders.Order) {
			o.OrderType = protocol.OrderTypeReshuffleRestore
		})
		child := pinOrder(t, db, "tell-retire-child", protocol.StatusQueued, func(o *orders.Order) {
			o.ParentOrderID = &parent.ID
		})
		mark := outboxMark(t, db)
		n, err := db.RetireReshuffleRestoreOrders()
		testutil.MustNoErr(t, err, "retire reshuffle_restore orders")
		if n != 2 {
			t.Fatalf("retired %d orders, want the parent and its child", n)
		}
		testdb.RequireOrderStatus(t, db, parent.EdgeUUID, dispatch.StatusCancelled)
		testdb.RequireOrderStatus(t, db, child.EdgeUUID, dispatch.StatusCancelled)
		requireTells(t, db, parent.EdgeUUID, mark)
		requireTells(t, db, child.EdgeUUID, mark)
	})

	// lifecycle.Park alone, on a pending order whose cause is already on the
	// row: no action fires on pending -> queued. Its callers tell (a door's one
	// queued push, the scanner's park tail), Park does not.
	t.Run("Park alone", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		eng := newTestEngine(t, db, simulator.New())
		o := pinOrder(t, db, "tell-park", protocol.StatusPending)
		eng.Dispatcher().SetQueueReason(o, protocol.QueueWaitingForMaterial, dispatch.CauseReserveHolding,
			dispatch.QueueParams{Payload: "TELL-P"})
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, eng.Dispatcher().Lifecycle().Park(o, "pin"), "park")
		eng.after.settle()
		testdb.RequireOrderStatus(t, db, o.EdgeUUID, dispatch.StatusQueued)
		requireTells(t, db, o.EdgeUUID, mark)
	})
}

// TestStatusTellPin_RobotSwapTellsNobody: the fleet moves an in_transit order to
// another robot without changing its state (EventOrderRobotChanged,
// handleVendorRobotChange). Core writes the new robot to orders.robot_id and
// tells nobody; the robot reaches the station on the order's next order.update.
// Flips if that robot write is made to tell the station (for example by
// bumping the order's version and queuing an order.update with it).
func TestStatusTellPin_RobotSwapTellsNobody(t *testing.T) {
	t.Parallel()
	db, eng, sim, order, _ := pinRetrieve(t, "tell-robot-swap")
	sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
	testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusInTransit)
	mark := outboxMark(t, db)

	(&pollerEmitter{bus: eng.Events}).EmitOrderRobotChanged(order.ID, order.VendorOrderID, "RUNNING", "AMB-01", "AMB-02")
	eng.after.settle()

	got := testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusInTransit)
	if got.RobotID != "AMB-02" {
		t.Errorf("robot_id = %q after the swap, want AMB-02", got.RobotID)
	}
	requireTells(t, db, order.EdgeUUID, mark)
}

// TestStatusTellPin_DeliveredIsNotToldConfirmed: after the fleet's FINISHED the
// order is delivered at Core, and over its whole life the station has been told
// delivered (order.update and order.delivered) and never confirmed. The fleet's
// MarkDelivered fires the order-completed event with no station on it, and the
// completion notice is gated on the event's station (engine/wiring.go, the
// EventOrderCompleted subscriber). Flips if Core tells the station `confirmed`
// for this delivery or if the gate starts reading the order's own station.
// Also pinned row-by-row in TestStatusTellPin_RowsPerTypedWriter/fleet_FINISHED.
func TestStatusTellPin_DeliveredIsNotToldConfirmed(t *testing.T) {
	t.Parallel()
	db, _, sim, order, before := pinRetrieve(t, "tell-delivered")
	sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	got := testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusDelivered)
	if got.CompletedAt != nil {
		t.Errorf("completed_at = %v on a delivered order nobody confirmed", got.CompletedAt)
	}
	requireTells(t, db, order.EdgeUUID, before,
		"order.ack  line-1",
		"order.waybill  line-1",
		"order.waybill  line-1",
		"order.update in_transit line-1",
		"order.delivered  line-1",
		"order.update delivered line-1")
	for _, u := range updatesAfter(t, db, order.EdgeUUID, before) {
		if u.Status == string(protocol.StatusConfirmed) {
			t.Errorf("the station was told confirmed (%+v) for an order Core holds as delivered", u)
		}
	}
}

// TestStatusTellPin_BootETABackfillCarriesTheRowStatus: at boot Core re-sends an
// ETA for every in_transit and staged order that has an in_transit history row,
// as an order.update whose Status is the row's own status (staged stays staged)
// and whose ETA is the last in_transit time plus the route's transit estimate.
// Flips when the status tell carries the per-order version (this push carries
// none) or when the backfill is folded into the status tell.
func TestStatusTellPin_BootETABackfillCarriesTheRowStatus(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	lc := eng.Dispatcher().Lifecycle()

	route := func(o *orders.Order) {
		o.SourceNode = "TELLETA-SRC"
		o.DeliveryNode = "TELLETA-DST"
	}
	// Each is born in_transit, so each has its in_transit history row; the
	// staged one is then moved on by the bare writer, which tells nobody.
	moving := pinOrder(t, db, "tell-eta-moving", protocol.StatusInTransit, route)
	staged := pinOrder(t, db, "tell-eta-staged", protocol.StatusInTransit, route)
	testutil.MustNoErr(t, lc.MarkStaged(staged, "fleet"), "stage the second order")
	testdb.RequireOrderStatus(t, db, staged.EdgeUUID, dispatch.StatusStaged)

	transit, ok := eng.etaCache.Lookup("TELLETA-SRC", "TELLETA-DST")
	if transit <= 0 {
		t.Fatalf("the ETA cache yields %v (route-specific %v) — the fixture needs an estimate", transit, ok)
	}
	mark := outboxMark(t, db)
	eng.backfillETAsForInTransitOrders()

	for _, o := range []*orders.Order{moving, staged} {
		var inTransitAt time.Time
		testutil.MustNoErr(t, db.DB.QueryRow(
			`SELECT MAX(created_at) FROM order_history WHERE order_id = $1 AND status = 'in_transit'`, o.ID).Scan(&inTransitAt),
			"read the in_transit history row")
		status := string(testdb.RequireOrder(t, db, o.EdgeUUID).Status)
		requireTells(t, db, o.EdgeUUID, mark, fmt.Sprintf("order.update %s line-1", status))
		u := updatesAfter(t, db, o.EdgeUUID, mark)
		if len(u) != 1 {
			continue
		}
		wantETA := inTransitAt.Add(transit).UTC().Format(time.RFC3339)
		if u[0].Status != status || u[0].ETA != wantETA || u[0].Detail != "eta restored on boot" {
			t.Errorf("%s backfill update = status %q eta %q detail %q; want status %q, eta %q, detail %q",
				o.EdgeUUID, u[0].Status, u[0].ETA, u[0].Detail, status, wantETA, "eta restored on boot")
		}
		if u[0].RobotID != "" || u[0].QueueReason != "" {
			t.Errorf("%s backfill update carries robot %q / queue reason %q; want neither", o.EdgeUUID, u[0].RobotID, u[0].QueueReason)
		}
	}
	if s := string(testdb.RequireOrder(t, db, staged.EdgeUUID).Status); s != string(protocol.StatusStaged) {
		t.Fatalf("the second order is %s, want staged", s)
	}
}
