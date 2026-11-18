package engine

// status_mirror_pin_test.go — how the Edge's order row follows Core's status
// today, pinned before the mirror moves to one versioned applier.
//
// Each pin drives the doors production drives: Core's messages through the
// wire handler (messaging.EdgeHandler), the reconcile's two halves
// (HandleOrderStatusSnapshots, HandleUnlistedOrders), the operator's confirm
// and abort, and the release act. It asserts the row's status, its
// order_history rows, the status-derived events the bus carried
// (EventOrderStatusChanged, EventOrderDelivered, EventOrderCompleted,
// EventOrderFailed) and, where it matters, Core's-cancel toast and the
// position's reorder hold. Every pin passes at this tree, and each says which
// change is expected to flip it, or which change already flipped it and what it
// pinned before. The RELEASE tap's own write is pinned beside
// it in orders/status_mirror_pin_test.go; the subscribers of those four events
// in status_event_census_test.go.

import (
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/messaging"
	"shingoedge/orders"
	"shingoedge/release"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
)

// mirrorRig is an engine whose order manager emits onto its bus, the wire
// handler over that manager, one process node with its runtime row, and the
// status-derived events the bus carried, in order. No subscriber is wired: the
// pins are about the row and the events, not what hangs on them.
type mirrorRig struct {
	t      *testing.T
	db     *store.DB
	eng    *Engine
	h      *messaging.EdgeHandler
	nodeID int64
	events []string
}

func newMirrorRig(t *testing.T) *mirrorRig {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.coreSync = newCoreSyncService(eng)
	_, nodeID := seedProcessNode(t, db)
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "the position's runtime row")
	r := &mirrorRig{t: t, db: db, eng: eng, h: newEdgeHandlerFor(eng), nodeID: nodeID}
	eng.Events.Subscribe(func(evt Event) {
		switch p := evt.Payload.(type) {
		case OrderStatusChangedEvent:
			r.events = append(r.events, "status "+p.OldStatus+"->"+p.NewStatus)
		case OrderDeliveredEvent:
			r.events = append(r.events, "delivered")
		case OrderCompletedEvent:
			r.events = append(r.events, "completed")
		case OrderFailedEvent:
			r.events = append(r.events, "failed")
		case OrderCancelledByCoreEvent:
			r.events = append(r.events, "cancelled by Core")
		}
	})
	return r
}

// leg is an order this Edge made at the rig's position, put at status with no
// event and no history row, the way a fixture puts the world where a pin
// starts.
func (r *mirrorRig) leg(uuid string, status protocol.Status) int64 {
	r.t.Helper()
	id, err := r.db.CreateOrder(uuid, orders.TypeRetrieve, &r.nodeID, false, 1, "TEST-NODE", "", "SRC",
		protocol.Height{}, false, "PART-M", "", "")
	testutil.MustNoErr(r.t, err, "create "+uuid)
	testutil.MustNoErr(r.t, r.db.UpdateOrderStatus(id, string(status)), "put "+uuid+" at "+string(status))
	return id
}

func (r *mirrorRig) row(id int64) *storeorders.Order {
	r.t.Helper()
	o, err := r.db.GetOrder(id)
	testutil.MustNoErr(r.t, err, "read the order")
	return o
}

// history is the order's history rows as "old->new: detail", in write order.
func (r *mirrorRig) history(id int64) []string {
	r.t.Helper()
	hs, err := r.db.ListOrderHistory(id)
	testutil.MustNoErr(r.t, err, "read the history")
	sort.Slice(hs, func(i, j int) bool { return hs[i].ID < hs[j].ID })
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, string(h.OldStatus)+"->"+string(h.NewStatus)+": "+h.Detail)
	}
	return out
}

func (r *mirrorRig) hold() domain.ReorderHold {
	r.t.Helper()
	rt, err := r.db.GetProcessNodeRuntime(r.nodeID)
	testutil.MustNoErr(r.t, err, "read the runtime row")
	return rt.ReorderHold
}

// expect fails unless the events since the last expect are want, then starts
// the next step's count.
func (r *mirrorRig) expect(step string, want ...string) {
	r.t.Helper()
	if !slices.Equal(r.events, want) {
		r.t.Errorf("%s: the bus carried %q, want %q", step, r.events, want)
	}
	r.events = nil
}

func expectRows(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n  got  %q\n  want %q", what, got, want)
	}
}

var wire = &protocol.Envelope{}

// TestStatusMirrorPin_TheWaybillWritesInTransit: Core's order.waybill is a
// status write today. The one Core sends at dispatch names no robot and moves
// an acknowledged row to in_transit; one naming a robot moves a staged row to
// in_transit (a legal edge). Each move is an ordinary transition: a history row
// in the waybill's words and an EventOrderStatusChanged.
//
// Expected to flip when the waybill stops writing the status of an order whose
// status Core versions: it then writes the waybill row and the robot only, and
// the row waits for Core's status.
func TestStatusMirrorPin_TheWaybillWritesInTransit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		from          protocol.Status
		waybill       protocol.OrderWaybill
		wantRobot     string
		wantHistory   string
		wantStatusEvt string
	}{
		{
			name:          "the dispatch waybill, no robot yet, on an acknowledged order",
			from:          protocol.StatusAcknowledged,
			waybill:       protocol.OrderWaybill{WaybillID: "V1"},
			wantHistory:   "acknowledged->in_transit: waybill V1, ETA ",
			wantStatusEvt: "status acknowledged->in_transit",
		},
		{
			name:          "the robot's waybill on a staged order",
			from:          protocol.StatusStaged,
			waybill:       protocol.OrderWaybill{WaybillID: "V2", RobotID: "AMR-1", ETA: "12:00"},
			wantRobot:     "AMR-1",
			wantHistory:   "staged->in_transit: waybill V2, ETA 12:00",
			wantStatusEvt: "status staged->in_transit",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newMirrorRig(t)
			id := r.leg("wb-leg", tc.from)

			wb := tc.waybill
			wb.OrderUUID = "wb-leg"
			r.h.HandleOrderWaybill(wire, &wb)

			o := r.row(id)
			if o.Status != protocol.StatusInTransit || o.RobotID != tc.wantRobot {
				t.Errorf("after the waybill the row reads %s, robot %q; want in_transit, robot %q",
					o.Status, o.RobotID, tc.wantRobot)
			}
			if o.WaybillID == nil || *o.WaybillID != wb.WaybillID {
				t.Errorf("waybill id %v, want %s", o.WaybillID, wb.WaybillID)
			}
			expectRows(t, "history", r.history(id), []string{tc.wantHistory})
			r.expect("the waybill", tc.wantStatusEvt)
		})
	}
}

// TestStatusMirrorPin_ACoreSideReleaseLeavesTheRowStaged: Core releases a wait
// it owns (here a lane wait) and sends the Edge nothing, so the row reads
// staged, at Core's kind, until the next message: order.delivered moves it
// staged->delivered, and the row's history never says in_transit after the
// staged.
//
// Expected to flip when the row follows Core's versioned status: Core's
// in_transit after its own release then reaches the row, and the history reads
// staged->in_transit->delivered.
func TestStatusMirrorPin_ACoreSideReleaseLeavesTheRowStaged(t *testing.T) {
	t.Parallel()
	r := newMirrorRig(t)
	id := r.leg("core-release-leg", protocol.StatusInTransit)

	r.h.HandleOrderStaged(wire, &protocol.OrderStaged{OrderUUID: "core-release-leg", Detail: "robot at the lane wait",
		WaitKind: protocol.WaitKindLane})
	r.expect("order.staged", "status in_transit->staged")

	// Core's lane gate releases the robot here and sends nothing.
	if o := r.row(id); o.Status != protocol.StatusStaged || o.WaitKind != protocol.WaitKindLane {
		t.Fatalf("after Core's own release the row reads %s at kind %q; want staged at %q",
			o.Status, o.WaitKind, protocol.WaitKindLane)
	}

	at := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	r.h.HandleOrderDelivered(wire, &protocol.OrderDelivered{OrderUUID: "core-release-leg", DeliveredAt: at})

	if o := r.row(id); o.Status != protocol.StatusDelivered {
		t.Fatalf("after order.delivered the row reads %s, want delivered", o.Status)
	}
	expectRows(t, "history", r.history(id), []string{
		"in_transit->staged: robot at the lane wait",
		"staged->delivered: " + at.Format(time.RFC3339),
	})
	r.expect("order.delivered", "status staged->delivered", "delivered")
}

// TestStatusMirrorPin_TheReconcileLeavesAConfirmedRowConfirmed: the operator
// confirms a delivered order (the receipt is queued, not yet at Core) and the
// completion chain's event fires. A reconcile snapshot taken before Core has
// the receipt says delivered, and the reconcile leaves the confirmed row as it
// ended: no status write, no history row, no event. Core's later word that it
// is confirmed, by snapshot or by push, finds the row ended and changes nothing
// either. EventOrderDelivered and EventOrderCompleted each fire once for the
// order, its history is the two rows it made, and Core's confirm is not
// answered with a second receipt.
//
// Flipped when the reconcile got the live path's terminal guard
// (ApplyCoreStatusSnapshot now returns on a terminal row before any write, as
// ApplyCoreStatus does). Before it, the delivered snapshot forced the row back
// to delivered and Core's confirmed confirmed it again, so both events fired
// twice.
func TestStatusMirrorPin_TheReconcileLeavesAConfirmedRowConfirmed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		confirm func(r *mirrorRig)
	}{
		{
			name: "a second snapshot says confirmed",
			confirm: func(r *mirrorRig) {
				r.eng.HandleOrderStatusSnapshots([]protocol.OrderStatusSnapshot{
					{OrderUUID: "confirm-leg", Found: true, Status: string(protocol.StatusConfirmed)}})
			},
		},
		{
			name: "Core's push says confirmed",
			confirm: func(r *mirrorRig) {
				r.h.HandleOrderUpdate(wire, &protocol.OrderUpdate{OrderUUID: "confirm-leg",
					Status: string(protocol.StatusConfirmed), Detail: "confirmed at Core"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newMirrorRig(t)
			id := r.leg("confirm-leg", protocol.StatusInTransit)
			at := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
			r.h.HandleOrderDelivered(wire, &protocol.OrderDelivered{OrderUUID: "confirm-leg", DeliveredAt: at})
			r.expect("order.delivered", "status in_transit->delivered", "delivered")

			testutil.MustNoErr(t, r.eng.OrderManager().ConfirmDelivery(id, 1), "the operator's confirm")
			r.expect("the operator's confirm", "status delivered->confirmed", "completed")

			r.eng.HandleOrderStatusSnapshots([]protocol.OrderStatusSnapshot{
				{OrderUUID: "confirm-leg", Found: true, Status: string(protocol.StatusDelivered)}})
			if st := r.row(id).Status; st != protocol.StatusConfirmed {
				t.Errorf("after the delivered snapshot the row reads %s, want confirmed: an ended row is not moved", st)
			}
			r.expect("the delivered snapshot")

			tc.confirm(r)
			if st := r.row(id).Status; st != protocol.StatusConfirmed {
				t.Errorf("after Core's confirmed the row reads %s, want confirmed", st)
			}
			r.expect("Core's confirmed")

			expectRows(t, "history", r.history(id), []string{
				"in_transit->delivered: " + at.Format(time.RFC3339),
				"delivered->confirmed: confirmed with count 1",
			})
			receipts, err := r.db.ListUnsentOutboxByType([]string{protocol.TypeOrderReceipt})
			testutil.MustNoErr(t, err, "read the receipts")
			if len(receipts) != 1 {
				t.Errorf("%d order.receipt rows queued, want the operator's one", len(receipts))
			}
		})
	}
}

// TestStatusMirrorPin_ALostCancelAndTheUnlistedProjectionLeaveACancelledRowCancelled:
// the operator aborts an in_transit order; the row ends cancelled and the
// order.cancel is queued, then lost before Core has it. The reconcile request
// does not name a cancelled row, so Core, still holding the order live, sends
// it back as Unlisted, and the projection finds the row ended and updates
// nothing: the row still reads cancelled, every column as the abort left it
// (the projection's quantity and blank payload do not land), with no status
// event and no history row beyond the abort's.
//
// Flipped when the projection upsert got the live path's terminal guard
// (UpsertProjection's conflict arm updates no row that has ended, as
// ApplyCoreStatus moves none). Before it, the projection wrote in_transit over
// the cancelled row and nothing an operator or a subscriber was told. It holds
// if the reconcile request comes to name a cancelled row too: the snapshot
// path carries the same guard.
func TestStatusMirrorPin_ALostCancelAndTheUnlistedProjectionLeaveACancelledRowCancelled(t *testing.T) {
	t.Parallel()
	r := newMirrorRig(t)
	id := r.leg("lost-cancel-leg", protocol.StatusInTransit)

	testutil.MustNoErr(t, r.eng.OrderManager().AbortOrderWithReason(id, "aborted by operator"), "the operator's abort")
	r.expect("the abort", "status in_transit->cancelled", "completed")
	cancels, err := r.db.ListUnsentOutboxByType([]string{protocol.TypeOrderCancel})
	testutil.MustNoErr(t, err, "read the cancels")
	if len(cancels) != 1 {
		t.Fatalf("fixture: %d order.cancel rows queued, want 1", len(cancels))
	}
	testutil.MustNoErr(t, r.db.AckOutbox(cancels[0].ID), "the cancel is lost: it never reaches Core")

	var named []string
	r.eng.SetSendFunc(func(env *protocol.Envelope) error {
		var d protocol.Data
		if err := env.DecodePayload(&d); err != nil {
			return err
		}
		var req protocol.OrderStatusRequest
		if err := json.Unmarshal(d.Body, &req); err != nil {
			return err
		}
		named = req.OrderUUIDs
		return nil
	})
	testutil.MustNoErr(t, r.eng.RequestOrderStatusSync(), "the reconcile request")
	if slices.Contains(named, "lost-cancel-leg") {
		t.Errorf("the reconcile request names the cancelled row (%v); pinned: it does not", named)
	}
	before := r.history(id)
	ended := r.row(id)

	r.eng.HandleUnlistedOrders([]protocol.OrderProjection{{OrderUUID: "lost-cancel-leg", OrderType: protocol.OrderTypeRetrieve,
		Status: string(protocol.StatusInTransit), StationID: "edge.test", SourceNode: "SRC", DeliveryNode: "TEST-NODE"}})

	after := r.row(id)
	if after.Status != protocol.StatusCancelled {
		t.Errorf("after the Unlisted projection the row reads %s, want cancelled: an ended row is not moved", after.Status)
	}
	if !reflect.DeepEqual(after, ended) {
		t.Errorf("the Unlisted projection changed the cancelled row: quantity %d, payload %q, updated %v; want %d, %q, %v",
			after.Quantity, after.PayloadCode, after.UpdatedAt, ended.Quantity, ended.PayloadCode, ended.UpdatedAt)
	}
	r.expect("the Unlisted projection")
	expectRows(t, "history after the projection", r.history(id), before)
	if want := []string{"in_transit->cancelled: aborted by operator"}; !slices.Equal(before, want) {
		t.Errorf("history %q, want %q", before, want)
	}
}

// TestStatusMirrorPin_TheCancelToastAndTheHoldNeedTheRowLive: Core's cancel
// toasts the station and holds the position's reorder only when the row was
// live as the message landed (HandleDispatchReply reads wasLive off the row
// before the mirror moves it), and Core's error holds on the same terms. The
// echo of the operator's own abort finds the row ended and is quiet; so is a
// reconcile snapshot that says cancelled, which ends a live row with no toast
// and no hold.
//
// Expected to flip when the toast and the hold stop reading the row's prior
// status: once the applier, not the cancelled or error envelope, ends the row,
// a live row can already read cancelled or failed when the envelope lands, and
// the toast and the hold have to hang on the message that carries the cause.
func TestStatusMirrorPin_TheCancelToastAndTheHoldNeedTheRowLive(t *testing.T) {
	t.Parallel()
	const fleet = "fleet order stopped: RDS error 60009"
	for _, tc := range []struct {
		name       string
		before     func(r *mirrorRig, id int64) // the row's own end, before Core's word
		core       func(r *mirrorRig)
		wantStatus protocol.Status
		wantEvents []string
		wantHold   domain.ReorderHold
	}{
		{
			name: "order.cancelled on a live row",
			core: func(r *mirrorRig) {
				r.h.HandleOrderCancelled(wire, &protocol.OrderCancelled{OrderUUID: "co-leg", Reason: fleet})
			},
			wantStatus: protocol.StatusCancelled,
			wantEvents: []string{"status in_transit->cancelled", "completed", "cancelled by Core"},
			wantHold:   domain.ReorderHoldPaused,
		},
		{
			name: "order.cancelled echoing the operator's abort",
			before: func(r *mirrorRig, id int64) {
				testutil.MustNoErr(r.t, r.eng.OrderManager().AbortOrder(id), "the operator's abort")
			},
			core: func(r *mirrorRig) {
				r.h.HandleOrderCancelled(wire, &protocol.OrderCancelled{OrderUUID: "co-leg", Reason: "aborted by operator"})
			},
			wantStatus: protocol.StatusCancelled,
			wantHold:   domain.ReorderHoldNone,
		},
		{
			name: "a reconcile snapshot that says cancelled, on a live row",
			core: func(r *mirrorRig) {
				r.eng.HandleOrderStatusSnapshots([]protocol.OrderStatusSnapshot{{OrderUUID: "co-leg", Found: true,
					Status: string(protocol.StatusCancelled), ErrorDetail: fleet}})
			},
			wantStatus: protocol.StatusCancelled,
			wantEvents: []string{"status in_transit->cancelled", "completed"},
			wantHold:   domain.ReorderHoldNone,
		},
		{
			name: "order.error on a live row",
			core: func(r *mirrorRig) {
				r.h.HandleOrderError(wire, &protocol.OrderError{OrderUUID: "co-leg", ErrorCode: "fleet_failed", Detail: fleet})
			},
			wantStatus: protocol.StatusFailed,
			wantEvents: []string{"status in_transit->failed", "completed", "failed"},
			wantHold:   domain.ReorderHoldPaused,
		},
		{
			name: "order.error on a row the operator aborted",
			before: func(r *mirrorRig, id int64) {
				testutil.MustNoErr(r.t, r.eng.OrderManager().AbortOrder(id), "the operator's abort")
			},
			core: func(r *mirrorRig) {
				r.h.HandleOrderError(wire, &protocol.OrderError{OrderUUID: "co-leg", ErrorCode: "fleet_failed", Detail: fleet})
			},
			wantStatus: protocol.StatusCancelled,
			wantHold:   domain.ReorderHoldNone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newMirrorRig(t)
			id := r.leg("co-leg", protocol.StatusInTransit)
			if tc.before != nil {
				tc.before(r, id)
				r.events = nil
			}

			tc.core(r)

			if st := r.row(id).Status; st != tc.wantStatus {
				t.Errorf("the row reads %s, want %s", st, tc.wantStatus)
			}
			r.expect("Core's word", tc.wantEvents...)
			if got := r.hold(); got != tc.wantHold {
				t.Errorf("the position holds %q, want %q", got, tc.wantHold)
			}
		})
	}
}

// TestStatusMirrorPin_ARejectionIsIgnoredUnlessTheRowIsInTransit: Core's
// order.error invalid_state is a refused release, and its rollback reads the
// row's status for whether a release went out: on an in_transit row (the
// RELEASE tap's write) it rolls back to staged, clears the intent and writes the
// sentence the release chip shows; on a staged row it changes nothing, intent
// and all.
//
// Expected to flip when the RELEASE tap stops writing in_transit for an order
// whose status Core versions: the released row then still reads staged when
// Core refuses, and the rollback has to read the intent's sent_at instead.
func TestStatusMirrorPin_ARejectionIsIgnoredUnlessTheRowIsInTransit(t *testing.T) {
	t.Parallel()
	const intent = `{"station_wait":0,"sent_at":"2026-11-17T09:00:00Z"}`
	const sentence = "Core rejected the release: the fleet did not take it. Click release to retry."
	for _, tc := range []struct {
		name        string
		from        protocol.Status
		wantStatus  protocol.Status
		wantIntent  string
		wantHeld    string
		wantEvents  []string
		wantHistory []string
	}{
		{
			name: "a staged row", from: protocol.StatusStaged,
			wantStatus: protocol.StatusStaged, wantIntent: intent,
		},
		{
			name: "an in_transit row", from: protocol.StatusInTransit,
			wantStatus: protocol.StatusStaged, wantHeld: sentence,
			wantEvents:  []string{"status in_transit->staged"},
			wantHistory: []string{"in_transit->staged: " + sentence},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newMirrorRig(t)
			id := r.leg("rej-leg", tc.from)
			testutil.MustNoErr(t, r.db.SetOrderReleaseIntent(id, intent), "the sent release's intent")

			r.h.HandleOrderError(wire, &protocol.OrderError{OrderUUID: "rej-leg", ErrorCode: "invalid_state",
				Detail: "the fleet did not take it"})

			o := r.row(id)
			if o.Status != tc.wantStatus || o.ReleaseIntent != tc.wantIntent || o.ReleaseHeld != tc.wantHeld {
				t.Errorf("the row reads %s, intent %q, release_held %q; want %s, %q, %q",
					o.Status, o.ReleaseIntent, o.ReleaseHeld, tc.wantStatus, tc.wantIntent, tc.wantHeld)
			}
			r.expect("order.error invalid_state", tc.wantEvents...)
			expectRows(t, "history", r.history(id), tc.wantHistory)
		})
	}
}

// TestStatusMirrorPin_OneBPerAAcrossTheReleaseDoors: a sequential position's
// Order A is released at the line through the intent worker (its press held at
// a live curtain, then re-planned once the curtain clears), and Order B is made
// once. Every other door that could release A again, or make B again, is
// pressed after it: the intent worker's next wake, the per-order RELEASE, the
// pair click, Core staging A at the same wait again (the fleet's no-op, which
// re-sends once), and a start. A has its one B throughout, linked to it.
//
// The doors this does not drive have their own pins: the per-order RELEASE as
// the first release, a re-press after Core's refusal, and a start between the
// release and B (sequential_backfill_release_test.go); a changeover's A, which
// brings its own carrier and gets no B (sequential_changeover_backfill_test.go,
// sequential_backfill_doors_test.go). The sim operator's release is the
// per-order door (release_census_test.go).
//
// Not expected to flip: when the RELEASE tap stops writing in_transit, a second
// press may go out where today it is refused (second_release_pin_test.go), and
// the guard in ensureSequentialBackfill (the position's B, and A's link to it)
// is what keeps this at one.
func TestStatusMirrorPin_OneBPerAAcrossTheReleaseDoors(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	h.sequentialAB(protocol.ClaimRoleConsume, true)
	coreWaybill(h, "removal", "AMR-1")
	coreStagedAt(h, "removal", 0)

	h.armCurtain(curtainLive)
	if err := orderClick("removal", dispEmpty)(h); err == nil {
		t.Fatal("fixture: the RELEASE at a live curtain went out; want it held")
	}
	if a := h.order("removal"); a.Status != protocol.StatusStaged || a.ReleaseIntent == "" {
		t.Fatalf("fixture: after the held press A reads %s, intent %q; want staged with the press remembered",
			a.Status, a.ReleaseIntent)
	}
	if b := backfills(h); len(b) != 0 {
		t.Fatalf("a held press made B %v; want none until A's release goes out", b)
	}

	h.wl.set(curtainSafe)
	h.eng.fireIntents(h.nodeID, "curtain cleared")
	first := backfills(h)
	if len(first) != 1 {
		t.Fatalf("after the intent worker released A: backfills %v, want exactly one", first)
	}
	// Read off the intent, not the status: the tap's in_transit is the write
	// the coming change takes away, and this pin does not hang on it.
	if in, err := release.DecodeIntent(h.order("removal").ReleaseIntent); err != nil || in == nil || !in.Sent() {
		t.Fatalf("fixture: A's intent after the intent worker's release is %+v (%v), want it sent", in, err)
	}

	for _, door := range []struct {
		name  string
		press func()
		// resends: the door sends A's release again today, so the act's
		// commit reaches ensureSequentialBackfill a second time and only its
		// guard stands between A and a second B. The other doors send
		// nothing today; what they send is not this pin's.
		resends bool
	}{
		{name: "the intent worker's next wake", press: func() { h.eng.fireIntents(h.nodeID, "floor") }},
		{name: "the per-order RELEASE", press: func() { _ = orderClick("removal", dispEmpty)(h) }},
		{name: "the pair click", press: func() { _ = h.eng.ReleaseStagedOrders(h.nodeID, dispEmpty) }},
		{name: "Core stages A at the same wait again", press: func() { coreStagedAt(h, "removal", 0) }, resends: true},
		{name: "a start", press: func() { h.eng.recoverSequentialBackfills() }},
	} {
		h.mark()
		door.press()
		if rel := h.releasedSinceMark(); door.resends && !slices.Equal(rel, []string{"removal"}) {
			t.Errorf("after %s: releases %v, want A's one re-send", door.name, rel)
		}
		if b := backfills(h); !slices.Equal(b, first) {
			t.Errorf("after %s: backfills %v, want only the first %v", door.name, b, first)
		}
	}
	a, b := h.order("removal"), backfillOf(h)
	if b == nil || b.ID != first[0] || b.SiblingOrderID == nil || *b.SiblingOrderID != a.ID ||
		a.SiblingOrderID == nil || *a.SiblingOrderID != b.ID {
		t.Errorf("B %v is not A %d's, linked both ways (A's link %v)", b, a.ID, a.SiblingOrderID)
	}
}
