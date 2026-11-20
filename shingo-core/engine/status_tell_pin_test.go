//go:build docker

package engine

// status_tell_pin_test.go pins what Core tells a station at each write it tells
// the station about: for each writer, the exact outbox rows it adds for that
// order, as "msg_type status station" (status is the order.update's, blank on
// any other message).
//
// Core tells the station from the write: every status write, every change of an
// order's robot to a named one and every change of wait on a waiting order
// queues one order.update in that write's own transaction, built from the row
// the write read back and carrying orders.version, which the same write bumps.
// The dedicated messages (the ack, the waybill, order.staged, order.delivered,
// order.cancelled, order.error, order.skipped) ride beside it unchanged.
//
// These began as characterisation pins of the sends that stood beside the
// writes (the vendor-status handler's order.update, the completion notice, the
// resume notice, the queue-reason push), each sent by whoever called the writer.
// They flipped when Core began telling the station from the write, and each
// pin's comment says what it pinned before.

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

// requireVersionsGrow fails unless every order.update after mark for the order
// carries a version, each greater than the one before it: each is its own
// write's tell, and that write bumped orders.version. It returns the updates.
func requireVersionsGrow(t *testing.T, db *store.DB, edgeUUID string, mark int64) []protocol.OrderUpdate {
	t.Helper()
	u := updatesAfter(t, db, edgeUUID, mark)
	var prev int64
	for i, x := range u {
		if x.Version <= prev {
			t.Errorf("order.update %d for %s (%s) carries version %d, want more than %d: versions only grow",
				i, edgeUUID, x.Status, x.Version, prev)
		}
		prev = x.Version
	}
	return u
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

// tellPinOrder is a hand-made order on pinStation, for writers that need no robot.
func tellPinOrder(t *testing.T, db *store.DB, uuid string, status protocol.Status, opts ...func(*orders.Order)) *orders.Order {
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
// Every writer here tells the station itself, whoever called it: its status
// write queues one order.update in its own transaction, and the dedicated
// messages ride beside it. These rows flipped when Core began telling the
// station from the write. Before, the tell was sent by whoever called the
// writer (the vendor-status handler, an event subscriber, the scanner), so a
// writer called from somewhere that sent nothing told the station nothing: a
// release, a coordinated dispatch, a reshuffle start, a restart, a force-recover,
// a park, and the bare failing and cancelling writes were all silent, and a
// cancel, a fail, a skip and a plain dispatch sent only their dedicated
// messages. The rows compare type, status and station; most subtests also check
// that the order's versions only grow.
func TestStatusTellPin_RowsPerTypedWriter(t *testing.T) {
	t.Parallel()

	// A plain dispatch at intake: the hand's status write (transitionTx on the
	// hand's transaction) tells `dispatched`, then the scanner's ack and waybill
	// follow (fulfillment/scanner.go notifyEdgeDispatched). It told nothing of
	// its own before, and the ack and waybill were the whole of it.
	t.Run("plain dispatch at intake", func(t *testing.T) {
		t.Parallel()
		db, _, _, order, before := pinRetrieve(t, "tell-plain")
		requireTells(t, db, order.EdgeUUID, before,
			"order.update dispatched line-1",
			"order.ack  line-1",
			"order.waybill  line-1")
		requireVersionsGrow(t, db, order.EdgeUUID, before)
	})

	// Fleet RUNNING on a dispatched order (MarkInTransit through the
	// vendor-status handler): the first-robot waybill, then the status write's
	// order.update in_transit, carrying the robot and an ETA from the handler's
	// tell template. Unchanged in its rows when the handler's own send retired.
	t.Run("fleet RUNNING", func(t *testing.T) {
		t.Parallel()
		db, _, sim, order, before := pinRetrieve(t, "tell-running")
		mark := outboxMark(t, db)
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusInTransit)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.waybill  line-1",
			"order.update in_transit line-1")
		requireVersionsGrow(t, db, order.EdgeUUID, before)
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

	// Fleet WAITING on an in_transit order (MarkStaged): the status write's
	// order.update staged, then order.staged. Unchanged in its rows when the
	// handler's own send retired.
	t.Run("fleet WAITING", func(t *testing.T) {
		t.Parallel()
		db, _, sim, order, before := pinRetrieve(t, "tell-waiting")
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		mark := outboxMark(t, db)
		sim.DriveState(order.VendorOrderID, "WAITING")
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusStaged)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.update staged line-1",
			"order.staged  line-1")
		requireVersionsGrow(t, db, order.EdgeUUID, before)
		if u := updatesAfter(t, db, order.EdgeUUID, mark); len(u) != 1 || u[0].RobotID != "AMB-01" || u[0].ETA != "" {
			t.Errorf("staged update = %+v, want robot AMB-01 and no ETA", u)
		}
	})

	// Fleet FINISHED on an in_transit order (MarkDelivered): order.delivered is
	// queued first (handleOrderDelivered runs before MarkDelivered), then the
	// status write's order.update delivered. No order.update confirmed: the write
	// tells the status it wrote, and only a confirm writes `confirmed`.
	// TestStatusTellPin_DeliveredIsNotToldConfirmed pins that over the order's
	// whole life.
	t.Run("fleet FINISHED", func(t *testing.T) {
		t.Parallel()
		db, _, sim, order, before := pinRetrieve(t, "tell-finished")
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		mark := outboxMark(t, db)
		sim.DriveState(order.VendorOrderID, "FINISHED")
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusDelivered)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.delivered  line-1",
			"order.update delivered line-1")
		requireVersionsGrow(t, db, order.EdgeUUID, before)
	})

	// ConfirmReceipt on a delivered order: the confirm's status write tells
	// `confirmed`, its detail the write's own history sentence and its robot the
	// row's. It flipped from the completion notice (engine/wiring.go, the
	// EventOrderCompleted subscriber), which sent the same status with the
	// detail "confirmed at Core".
	t.Run("ConfirmReceipt", func(t *testing.T) {
		t.Parallel()
		db, eng, sim, order, before := pinRetrieve(t, "tell-confirm")
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
		requireVersionsGrow(t, db, order.EdgeUUID, before)
		const detail = "receipt: confirmed, count: 1"
		if u := updatesAfter(t, db, order.EdgeUUID, mark); len(u) != 1 || u[0].Detail != detail || u[0].RobotID != "AMB-01" {
			t.Errorf("confirmed update = %+v, want detail %q and robot AMB-01", u, detail)
		}
	})

	// CancelOrder on a dispatched order: the cancel's status write tells
	// `cancelled`, then order.cancelled from the cancellation subscriber. It
	// flipped from order.cancelled alone.
	t.Run("CancelOrder", func(t *testing.T) {
		t.Parallel()
		db, eng, _, order, before := pinRetrieve(t, "tell-cancel")
		mark := outboxMark(t, db)
		eng.Dispatcher().Lifecycle().CancelOrder(order, order.StationID, "pin: cancelled", dispatch.CancelCause{})
		eng.after.settle()
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusCancelled)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.update cancelled line-1",
			"order.cancelled  line-1")
		requireVersionsGrow(t, db, order.EdgeUUID, before)
	})

	// Fail on a dispatched order: the fail's status write tells `failed`, then
	// order.error from the failure subscriber. It flipped from order.error alone.
	t.Run("Fail", func(t *testing.T) {
		t.Parallel()
		db, eng, _, order, before := pinRetrieve(t, "tell-fail")
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, eng.Dispatcher().Lifecycle().Fail(order, order.StationID, "pin_failed", "pin: failed"), "fail")
		eng.after.settle()
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusFailed)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.update failed line-1",
			"order.error  line-1")
		requireVersionsGrow(t, db, order.EdgeUUID, before)
	})

	// Skip on a queued order: the skip's status write tells `skipped`, then
	// order.skipped from the skip subscriber. It flipped from order.skipped
	// alone.
	t.Run("Skip", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		eng := newTestEngine(t, db, simulator.New())
		o := tellPinOrder(t, db, "tell-skip", protocol.StatusQueued)
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, eng.Dispatcher().Lifecycle().Skip(o, pinStation, "pin_skipped", "pin: never needed"), "skip")
		eng.after.settle()
		testdb.RequireOrderStatus(t, db, o.EdgeUUID, dispatch.StatusSkipped)
		requireTells(t, db, o.EdgeUUID, mark,
			"order.update skipped line-1",
			"order.skipped  line-1")
		requireVersionsGrow(t, db, o.EdgeUUID, mark)
	})

	// Fleet FAILED on an in_transit order (MarkFaulted): the status write's
	// order.update faulted, with the fault clock, the threshold and the fault's
	// reference from the handler's tell template. Unchanged when the handler's
	// own send retired.
	t.Run("fleet FAILED (MarkFaulted)", func(t *testing.T) {
		t.Parallel()
		db, _, sim, order, before := pinRetrieve(t, "tell-faulted")
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		mark := outboxMark(t, db)
		sim.DriveState(order.VendorOrderID, "FAILED")
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusFaulted)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.update faulted line-1")
		requireVersionsGrow(t, db, order.EdgeUUID, before)
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

	// Fleet RUNNING on a faulted order (MarkFaultedRecovered): the status
	// write's order.update in_transit, no fault fields, an ETA, no second
	// waybill. Unchanged when the handler's own send retired.
	t.Run("fleet RUNNING after faulted (recovery)", func(t *testing.T) {
		t.Parallel()
		db, _, sim, order, before := pinRetrieve(t, "tell-recovered")
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		sim.DriveState(order.VendorOrderID, "FAILED")
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusFaulted)
		mark := outboxMark(t, db)
		sim.DriveState(order.VendorOrderID, "RUNNING")
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusInTransit)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.update in_transit line-1")
		requireVersionsGrow(t, db, order.EdgeUUID, before)
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

	// Release on a staged order: the release's status write tells `in_transit`
	// with the row's robot. The fleet's RUNNING that follows maps to the status
	// Core already wrote and takes the same-status return
	// (engine/wiring_vendor_status.go), so it adds nothing. It flipped from
	// silence: neither the release nor the RUNNING told the station, which
	// heard of the release from nobody.
	t.Run("Release, then the fleet's RUNNING", func(t *testing.T) {
		t.Parallel()
		db, eng, sim, order, before := pinRetrieve(t, "tell-release")
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		sim.DriveState(order.VendorOrderID, "WAITING")
		staged := testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusStaged)
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, eng.Dispatcher().Lifecycle().Release(staged, "dispatcher"), "release")
		eng.after.settle()
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusInTransit)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.update in_transit line-1")
		if u := updatesAfter(t, db, order.EdgeUUID, mark); len(u) != 1 || u[0].RobotID != "AMB-01" {
			t.Errorf("release update = %+v, want robot AMB-01", u)
		}
		sim.DriveState(order.VendorOrderID, "RUNNING")
		got := testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusInTransit)
		if got.VendorState != "RUNNING" {
			t.Errorf("vendor_state = %q after the fleet's RUNNING, want RUNNING", got.VendorState)
		}
		requireTells(t, db, order.EdgeUUID, mark,
			"order.update in_transit line-1")
		requireVersionsGrow(t, db, order.EdgeUUID, before)
	})

	// BeginReshuffle on a queued order: no action fires on the way into
	// reshuffling, and the status write tells `reshuffling`. It flipped from
	// silence.
	t.Run("BeginReshuffle", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		eng := newTestEngine(t, db, simulator.New())
		o := tellPinOrder(t, db, "tell-reshuffle", protocol.StatusQueued)
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, eng.Dispatcher().Lifecycle().BeginReshuffle(o, "pin: the dig begins"), "begin reshuffle")
		eng.after.settle()
		testdb.RequireOrderStatus(t, db, o.EdgeUUID, dispatch.StatusReshuffling)
		requireTells(t, db, o.EdgeUUID, mark,
			"order.update reshuffling line-1")
		requireVersionsGrow(t, db, o.EdgeUUID, mark)
	})

	// Restart on a parent in reshuffling (the fixture of
	// TestResume_TheParentGoesBackToPendingAndAcquiresOnce). The restart's own
	// write (reshuffling -> pending) tells `pending`, its detail the restart's
	// reason; its one acquire runs on the after-queue and parks the parent
	// again, and the park's write tells `queued` with the cause. It flipped from
	// the park's `queued` alone: the station was never told `pending`.
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
			"order.update pending line-1",
			"order.update queued line-1")
		u := requireVersionsGrow(t, db, parent.EdgeUUID, mark)
		if len(u) != 2 {
			t.Fatalf("restart updates = %+v, want pending then queued", u)
		}
		if u[0].Detail != "pin: the dig is over" || u[0].QueueReason != "" || u[0].QueueCode != "" {
			t.Errorf("pending update = %+v, want the restart's reason as its detail and no wait", u[0])
		}
		if u[1].QueueCode != string(protocol.QueueWaitingForSlot) || u[1].QueueCode != after.QueueCode ||
			u[1].QueueReason != after.QueueReason || u[1].QueueReason == "" {
			t.Errorf("queued update = %+v, want the cause the acquire parked it under (%s, %q)", u[1], after.QueueCode, after.QueueReason)
		}
	})

	// A coordinated dispatch: a complex order parked at intake for want of a bin,
	// dispatched by the next scan once the bin is there. The dispatch's status
	// write (the hand's transitionTx, coordinated tail) tells `dispatched`; no
	// ack, no waybill. It flipped from silence: the station first heard of it at
	// the fleet's RUNNING.
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
		requireTells(t, db, "tell-coordinated", mark,
			"order.update dispatched line-1")
		requireVersionsGrow(t, db, "tell-coordinated", mark)
	})

	// Force-recover (RecoveryService.RecoverFaultedOrder, which has no
	// production caller): MarkInTransit straight from faulted. Its recovery
	// event reaches the email notifier only; the status write tells the station
	// `in_transit`. It flipped from silence.
	t.Run("RecoverFaultedOrder", func(t *testing.T) {
		t.Parallel()
		db, eng, sim, order, before := pinRetrieve(t, "tell-force-recover")
		sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
		sim.DriveState(order.VendorOrderID, "FAILED")
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusFaulted)
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, eng.RecoverFaultedOrder(order.ID, "pin-operator"), "recover faulted order")
		eng.after.settle()
		testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusInTransit)
		requireTells(t, db, order.EdgeUUID, mark,
			"order.update in_transit line-1")
		requireVersionsGrow(t, db, order.EdgeUUID, before)
	})

	// A compound child failed the way dispatch/compound.go fails one: a bare
	// db.FailOrderAtomic, past transition(), no event. The terminal write tells
	// the child's station `failed` (this child has a station and an edge uuid);
	// the parent, not written, is told nothing. It flipped from nobody told,
	// child or parent.
	t.Run("compound child FailOrderAtomic", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		newTestEngine(t, db, simulator.New())
		parent := tellPinOrder(t, db, "tell-atomic-parent", protocol.StatusReshuffling, func(o *orders.Order) {
			o.OrderType = protocol.OrderTypeComplex
		})
		child := tellPinOrder(t, db, "tell-atomic-child", protocol.StatusPending, func(o *orders.Order) {
			o.ParentOrderID = &parent.ID
			o.OrderType = protocol.OrderTypeMove
		})
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, db.FailOrderAtomic(child.ID, "missing source node"), "fail the child")
		testdb.RequireOrderStatus(t, db, child.EdgeUUID, dispatch.StatusFailed)
		requireTells(t, db, child.EdgeUUID, mark,
			"order.update failed line-1")
		requireVersionsGrow(t, db, child.EdgeUUID, mark)
		requireTells(t, db, parent.EdgeUUID, mark)
	})

	// The boot sweep of leftover reshuffle_restore orders cancels parent and
	// child through TerminalizeOrder, past transition(), no event. Each terminal
	// write tells its order's station `cancelled`. It flipped from nobody told.
	t.Run("RetireReshuffleRestoreOrders", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		newTestEngine(t, db, simulator.New())
		parent := tellPinOrder(t, db, "tell-retire-parent", protocol.StatusReshuffling, func(o *orders.Order) {
			o.OrderType = protocol.OrderTypeReshuffleRestore
		})
		child := tellPinOrder(t, db, "tell-retire-child", protocol.StatusQueued, func(o *orders.Order) {
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
		requireTells(t, db, parent.EdgeUUID, mark,
			"order.update cancelled line-1")
		requireTells(t, db, child.EdgeUUID, mark,
			"order.update cancelled line-1")
		requireVersionsGrow(t, db, parent.EdgeUUID, mark)
		requireVersionsGrow(t, db, child.EdgeUUID, mark)
	})

	// lifecycle.Park alone, on a pending order whose cause is already on the
	// row: no action fires on pending -> queued, and the status write tells
	// `queued` with the cause the row holds. It flipped from silence, when only
	// Park's callers told (a door's one queued push, the scanner's park tail).
	t.Run("Park alone", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		eng := newTestEngine(t, db, simulator.New())
		o := tellPinOrder(t, db, "tell-park", protocol.StatusPending)
		eng.Dispatcher().SetQueueReason(o, protocol.QueueWaitingForMaterial, dispatch.CauseReserveHolding,
			dispatch.QueueParams{Payload: "TELL-P"})
		mark := outboxMark(t, db)
		testutil.MustNoErr(t, eng.Dispatcher().Lifecycle().Park(o, "pin"), "park")
		eng.after.settle()
		parked := testdb.RequireOrderStatus(t, db, o.EdgeUUID, dispatch.StatusQueued)
		requireTells(t, db, o.EdgeUUID, mark,
			"order.update queued line-1")
		u := requireVersionsGrow(t, db, o.EdgeUUID, mark)
		if len(u) != 1 || u[0].QueueCode != string(protocol.QueueWaitingForMaterial) ||
			u[0].QueueReason != parked.QueueReason || u[0].QueueReason == "" {
			t.Errorf("park update = %+v, want the row's cause (%s, %q)", u, protocol.QueueWaitingForMaterial, parked.QueueReason)
		}
	})
}

// TestStatusTellPin_ARobotSwapIsTold: the fleet moves an in_transit order to
// another robot without changing its state (EventOrderRobotChanged,
// handleVendorRobotChange). The robot write tells the station: one
// order.update naming the new robot, the status unchanged, and a version newer
// than the station held. It flipped when Core began telling the station from
// the write; it was written as the pin that a robot swap tells nobody: the
// robot write told nobody, and the robot reached the station only on the
// order's next order.update.
func TestStatusTellPin_ARobotSwapIsTold(t *testing.T) {
	t.Parallel()
	db, eng, sim, order, before := pinRetrieve(t, "tell-robot-swap")
	sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
	testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusInTransit)
	mark := outboxMark(t, db)
	held := updatesAfter(t, db, order.EdgeUUID, before)
	if len(held) == 0 || held[len(held)-1].RobotID != "AMB-01" {
		t.Fatalf("before the swap the station was told %+v, want an update naming AMB-01 last", held)
	}

	(&pollerEmitter{bus: eng.Events}).EmitOrderRobotChanged(order.ID, order.VendorOrderID, "RUNNING", "AMB-01", "AMB-02")
	eng.after.settle()

	got := testdb.RequireOrderStatus(t, db, order.EdgeUUID, dispatch.StatusInTransit)
	if got.RobotID != "AMB-02" {
		t.Errorf("robot_id = %q after the swap, want AMB-02", got.RobotID)
	}
	requireTells(t, db, order.EdgeUUID, mark,
		"order.update in_transit line-1")
	u := updatesAfter(t, db, order.EdgeUUID, mark)
	if len(u) != 1 {
		t.Fatalf("swap updates = %+v, want one", u)
	}
	if u[0].RobotID != "AMB-02" {
		t.Errorf("swap update names robot %q, want AMB-02", u[0].RobotID)
	}
	if was := held[len(held)-1].Version; u[0].Version <= was {
		t.Errorf("swap update carries version %d, want more than the %d the station held", u[0].Version, was)
	}
	requireVersionsGrow(t, db, order.EdgeUUID, before)
}

// TestStatusTellPin_DeliveredIsNotToldConfirmed: after the fleet's FINISHED the
// order is delivered at Core, and over its whole life the station has been told
// dispatched, in_transit and delivered (each by its own status write, beside
// the ack, the waybills and order.delivered) and never confirmed. Each write
// tells the status it wrote, so `delivered` for a delivery; `confirmed` comes
// only from a confirm's write. It flipped when Core began telling the station
// from the write: the list gained the dispatch's own `dispatched` first, and
// what kept `confirmed` away was a gate, the completion notice's on the
// event's station (engine/wiring.go), which MarkDelivered did not set. That
// gate is gone with the notice. Also pinned row-by-row in
// TestStatusTellPin_RowsPerTypedWriter/fleet_FINISHED.
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
		"order.update dispatched line-1",
		"order.ack  line-1",
		"order.waybill  line-1",
		"order.waybill  line-1",
		"order.update in_transit line-1",
		"order.delivered  line-1",
		"order.update delivered line-1")
	for _, u := range requireVersionsGrow(t, db, order.EdgeUUID, before) {
		if u.Status == string(protocol.StatusConfirmed) {
			t.Errorf("the station was told confirmed (%+v) for an order Core holds as delivered", u)
		}
	}
}

// TestStatusTellPin_BootETABackfillCarriesTheETAOnly: at boot Core re-sends an
// ETA for every in_transit and staged order that has an in_transit history row,
// as an order.update with no status, no version and no robot: the ETA, the last
// in_transit time plus the route's transit estimate, and nothing the status
// write already told. An empty status is no status claim; the station applies
// the ETA and keeps the status it holds. It flipped when Core began telling
// the station from the write; it was written as the pin that the backfill
// carries the row's status: the backfill carried the row's own status
// (staged stayed staged), which made it the only thing that ever told a station
// of a Core-side release.
func TestStatusTellPin_BootETABackfillCarriesTheETAOnly(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	lc := eng.Dispatcher().Lifecycle()

	route := func(o *orders.Order) {
		o.SourceNode = "TELLETA-SRC"
		o.DeliveryNode = "TELLETA-DST"
	}
	// Each is born in_transit, so each has its in_transit history row; the
	// staged one is then moved on by MarkStaged, whose own tell lands before the
	// mark below.
	moving := tellPinOrder(t, db, "tell-eta-moving", protocol.StatusInTransit, route)
	staged := tellPinOrder(t, db, "tell-eta-staged", protocol.StatusInTransit, route)
	testutil.MustNoErr(t, lc.MarkStaged(staged, "fleet", protocol.OrderUpdate{}), "stage the second order")
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
		requireTells(t, db, o.EdgeUUID, mark, "order.update  line-1")
		u := updatesAfter(t, db, o.EdgeUUID, mark)
		if len(u) != 1 {
			continue
		}
		wantETA := inTransitAt.Add(transit).UTC().Format(time.RFC3339)
		if u[0].Status != "" || u[0].ETA != wantETA || u[0].Detail != "eta restored on boot" {
			t.Errorf("%s backfill update = status %q eta %q detail %q; want no status, eta %q, detail %q",
				o.EdgeUUID, u[0].Status, u[0].ETA, u[0].Detail, wantETA, "eta restored on boot")
		}
		if u[0].RobotID != "" || u[0].QueueReason != "" || u[0].Version != 0 {
			t.Errorf("%s backfill update carries robot %q / queue reason %q / version %d; want none",
				o.EdgeUUID, u[0].RobotID, u[0].QueueReason, u[0].Version)
		}
	}
	if s := string(testdb.RequireOrder(t, db, staged.EdgeUUID).Status); s != string(protocol.StatusStaged) {
		t.Fatalf("the second order is %s, want staged", s)
	}
}
