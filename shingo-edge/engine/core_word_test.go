package engine

// core_word_test.go — how the Edge's order row follows a Core that versions
// its orders. Every word of Core's about an order's status (the live
// order.update, the reconcile's snapshot, the projection) goes through one
// applier (orders/core_word.go): newer only, a history row and the status
// events per move, an ended row kept as it ended. The dedicated envelopes then
// carry their facts and write no status.
//
// The tests drive the doors production drives, through the rig of
// status_mirror_pin_test.go: the wire handler, the reconcile's two halves, the
// operator's confirm and abort.

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
	"shingoedge/orders"
	storeorders "shingoedge/store/orders"
)

// wordRig is the mirror rig with four more recordings: "touched" in the
// event list for a board nudge that names an order (the applier's same-status
// word), every EventOrderDelivered's payload, every Core-cancel toast's
// reason, and the bins rows the wire handler applied off an order.error.
type wordRig struct {
	*mirrorRig
	delivered []OrderDeliveredEvent
	toasts    []string
	binsRows  []string
}

func newWordRig(t *testing.T) *wordRig {
	t.Helper()
	r := &wordRig{mirrorRig: newMirrorRig(t)}
	r.h.ApplyRows = func(rows []protocol.NodeBinsRow) {
		for _, row := range rows {
			r.binsRows = append(r.binsRows, row.Node)
		}
	}
	r.eng.Events.Subscribe(func(evt Event) {
		switch p := evt.Payload.(type) {
		case BoardNudgeEvent:
			// The projection's own nudge names no order id; only the
			// applier's touched does.
			if p.OrderID != 0 {
				r.events = append(r.events, "touched")
			}
		case OrderDeliveredEvent:
			r.delivered = append(r.delivered, p)
		case OrderCancelledByCoreEvent:
			r.toasts = append(r.toasts, p.Reason)
		}
	})
	return r
}

// autoLeg is leg for an order that confirms itself on delivery.
func (r *wordRig) autoLeg(uuid string, status protocol.Status) int64 {
	r.t.Helper()
	id, err := r.db.CreateOrder(uuid, orders.TypeRetrieve, &r.nodeID, false, 1, "TEST-NODE", "", "SRC",
		protocol.Height{}, true, "PART-M", "", "")
	testutil.MustNoErr(r.t, err, "create "+uuid)
	testutil.MustNoErr(r.t, r.db.UpdateOrderStatus(id, string(status)), "put "+uuid+" at "+string(status))
	return id
}

// update is Core's versioned order.update through the wire handler.
func (r *wordRig) update(u protocol.OrderUpdate) { r.h.HandleOrderUpdate(wire, &u) }

// snapshot is one reconcile snapshot Core found the order for.
func (r *wordRig) snapshot(s protocol.OrderStatusSnapshot) {
	s.Found = true
	r.eng.HandleOrderStatusSnapshots([]protocol.OrderStatusSnapshot{s})
}

// at fails unless the row reads status at Core's version, and returns it.
func (r *wordRig) at(step string, id int64, status protocol.Status, version int64) *storeorders.Order {
	r.t.Helper()
	o := r.row(id)
	if o.Status != status || o.CoreVersion != version {
		r.t.Errorf("%s: the row reads %s at version %d, want %s at %d", step, o.Status, o.CoreVersion, status, version)
	}
	return o
}

// live puts a leg at a versioned status the way Core's word does, then clears
// the events it fired, so a test starts on a row whose status is Core's.
func (r *wordRig) live(uuid string, status protocol.Status, version int64) {
	r.t.Helper()
	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(status), Version: version})
	r.events = nil
}

// cancels is the order.cancel payloads queued, in queue order.
func (r *wordRig) cancels() []protocol.OrderCancel {
	r.t.Helper()
	var out []protocol.OrderCancel
	for _, env := range r.queued(protocol.TypeOrderCancel) {
		var c protocol.OrderCancel
		testutil.MustNoErr(r.t, env.DecodePayload(&c), "decode an order.cancel")
		out = append(out, c)
	}
	return out
}

// receipts is the order.receipt payloads queued, in queue order.
func (r *wordRig) receipts() []protocol.OrderReceipt {
	r.t.Helper()
	var out []protocol.OrderReceipt
	for _, env := range r.queued(protocol.TypeOrderReceipt) {
		var c protocol.OrderReceipt
		testutil.MustNoErr(r.t, env.DecodePayload(&c), "decode an order.receipt")
		out = append(out, c)
	}
	return out
}

func (r *wordRig) queued(msgType string) []protocol.Envelope {
	r.t.Helper()
	msgs, err := r.db.ListUnsentOutboxByType([]string{msgType})
	testutil.MustNoErr(r.t, err, "read the outbox")
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
	out := make([]protocol.Envelope, 0, len(msgs))
	for _, m := range msgs {
		var env protocol.Envelope
		testutil.MustNoErr(r.t, json.Unmarshal(m.Payload, &env), "decode an outbox envelope")
		out = append(out, env)
	}
	return out
}

func expectCancels(t *testing.T, step string, got []protocol.OrderCancel, uuid, reason string, n int) {
	t.Helper()
	if len(got) != n {
		t.Errorf("%s: %d order.cancel rows queued %+v, want %d", step, len(got), got, n)
		return
	}
	for i, c := range got {
		if c.OrderUUID != uuid || c.Reason != reason {
			t.Errorf("%s: order.cancel %d names %q with reason %q, want %q with %q", step, i+1, c.OrderUUID, c.Reason, uuid, reason)
		}
	}
}

func textOf(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestCoreWord_APlainOrderFollowsCoresVersionsEndToEnd: an order this Edge
// made, from Core's echo of it to Core's confirm. Each status Core tells moves
// the row once, with its history row and EventOrderStatusChanged; the ack and
// both waybills write their facts and no status; order.delivered's carrier
// facts wait for Core's delivered, and the delivered event fires once with
// them; the operator's confirm completes the order once, and Core's confirmed
// after it changes nothing but the version.
func TestCoreWord_APlainOrderFollowsCoresVersionsEndToEnd(t *testing.T) {
	t.Parallel()
	r := newWordRig(t)
	const uuid = "plain-leg"
	id := r.leg(uuid, protocol.StatusSubmitted)

	_, err := r.eng.ApplyOrderProjection(protocol.OrderProjection{OrderUUID: uuid, OrderType: protocol.OrderTypeRetrieve,
		Status: string(protocol.StatusPending), Quantity: 1, SourceNode: "SRC",
		DeliveryNode: "TEST-NODE", PayloadCode: "PART-M", Version: 1})
	testutil.MustNoErr(t, err, "Core's echo of the order")
	r.at("the projection", id, protocol.StatusPending, 1)
	r.expect("the projection", "status submitted->pending")

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusDispatched), Version: 2})
	r.at("dispatched v2", id, protocol.StatusDispatched, 2)
	r.expect("dispatched v2", "status pending->dispatched")

	r.h.HandleOrderAck(wire, &protocol.OrderAck{OrderUUID: uuid, ShingoOrderID: 9, SourceNode: "SRC"})
	r.at("the ack", id, protocol.StatusDispatched, 2)
	r.expect("the ack")

	r.h.HandleOrderWaybill(wire, &protocol.OrderWaybill{OrderUUID: uuid, WaybillID: "V1"})
	if o := r.at("the dispatch waybill", id, protocol.StatusDispatched, 2); textOf(o.WaybillID) != "V1" || o.RobotID != "" {
		t.Errorf("after the dispatch waybill: waybill %s, robot %q; want V1, no robot", textOf(o.WaybillID), o.RobotID)
	}
	r.expect("the dispatch waybill")

	r.h.HandleOrderWaybill(wire, &protocol.OrderWaybill{OrderUUID: uuid, WaybillID: "V1", RobotID: "AMB-1", ETA: "12:05"})
	if o := r.at("the robot's waybill", id, protocol.StatusDispatched, 2); o.RobotID != "AMB-1" || textOf(o.ETA) != "12:05" {
		t.Errorf("after the robot's waybill: robot %q, ETA %s; want AMB-1, 12:05", o.RobotID, textOf(o.ETA))
	}
	r.expect("the robot's waybill")

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusInTransit), Version: 3,
		RobotID: "AMB-1", ETA: "12:07", Detail: "AMB-1 driving to TEST-NODE"})
	if o := r.at("in_transit v3", id, protocol.StatusInTransit, 3); o.RobotID != "AMB-1" || textOf(o.ETA) != "12:07" {
		t.Errorf("after in_transit v3: robot %q, ETA %s; want AMB-1, 12:07", o.RobotID, textOf(o.ETA))
	}
	r.expect("in_transit v3", "status dispatched->in_transit")

	// Core queues order.delivered before its status write, so it lands first:
	// the facts are written and wait for Core's delivered.
	bin, uop, code := int64(77), 30, "PART-M"
	at := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	r.h.HandleOrderDelivered(wire, &protocol.OrderDelivered{OrderUUID: uuid, DeliveredAt: at, BinID: &bin,
		UOPRemaining: &uop, BinPayloadCode: &code, DeltaEpoch: 4, DeliveryNode: "TEST-NODE", LandingSent: true})
	if o := r.at("order.delivered", id, protocol.StatusInTransit, 3); o.BinID == nil || *o.BinID != bin {
		t.Errorf("after order.delivered the row's bin is %v, want %d", o.BinID, bin)
	}
	r.expect("order.delivered")

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusDelivered), Version: 4})
	r.at("delivered v4", id, protocol.StatusDelivered, 4)
	r.expect("delivered v4", "status in_transit->delivered", "delivered")

	testutil.MustNoErr(t, r.eng.OrderManager().ConfirmDelivery(id, 1), "the operator's confirm")
	r.at("the operator's confirm", id, protocol.StatusConfirmed, 4)
	r.expect("the operator's confirm", "status delivered->confirmed", "completed")

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusConfirmed), Version: 5})
	r.at("confirmed v5", id, protocol.StatusConfirmed, 5)
	r.expect("confirmed v5")

	expectRows(t, "history", r.history(id), []string{
		"submitted->pending: projected by Core",
		"pending->dispatched: core: dispatched (version 2)",
		"dispatched->in_transit: AMB-1 driving to TEST-NODE",
		"in_transit->delivered: core: delivered (version 4)",
		"delivered->confirmed: confirmed with count 1",
	})
	if len(r.delivered) != 1 {
		t.Fatalf("%d delivered events, want 1", len(r.delivered))
	}
	d := r.delivered[0]
	if d.OrderID != id || d.BinID == nil || *d.BinID != bin || d.BinUOP == nil || *d.BinUOP != uop ||
		d.BinPayloadCode == nil || *d.BinPayloadCode != code || d.BinEpoch != 4 || !d.LandingSent {
		t.Errorf("the delivered event carries order %d, bin %v, uop %v, payload %v, epoch %d, landing %v; "+
			"want order %d and the envelope's bin %d, uop %d, %s, epoch 4, landing sent",
			d.OrderID, d.BinID, d.BinUOP, d.BinPayloadCode, d.BinEpoch, d.LandingSent, id, bin, uop, code)
	}
	if got := r.receipts(); len(got) != 1 || got[0].OrderUUID != uuid || got[0].FinalCount != 1 {
		t.Errorf("order.receipt rows %+v, want the operator's one, count 1", got)
	}
}

// TestCoreWord_AnOlderWordMovesNothing: a word older than the one applied
// writes nothing, the robot and ETA included, and fires nothing; the reconcile
// is held to the same.
func TestCoreWord_AnOlderWordMovesNothing(t *testing.T) {
	t.Parallel()
	r := newWordRig(t)
	id := r.leg("older-leg", protocol.StatusDispatched)

	r.update(protocol.OrderUpdate{OrderUUID: "older-leg", Status: string(protocol.StatusInTransit), Version: 4,
		RobotID: "AMB-1", ETA: "12:05"})
	r.expect("in_transit v4", "status dispatched->in_transit")

	r.update(protocol.OrderUpdate{OrderUUID: "older-leg", Status: string(protocol.StatusDispatched), Version: 3,
		RobotID: "AMB-9", ETA: "13:00"})
	if o := r.at("dispatched v3", id, protocol.StatusInTransit, 4); o.RobotID != "AMB-1" || textOf(o.ETA) != "12:05" {
		t.Errorf("after the older word: robot %q, ETA %s; want AMB-1, 12:05", o.RobotID, textOf(o.ETA))
	}
	r.expect("dispatched v3")

	r.snapshot(protocol.OrderStatusSnapshot{OrderUUID: "older-leg", Status: string(protocol.StatusDispatched), Version: 3})
	r.at("an older snapshot", id, protocol.StatusInTransit, 4)
	r.expect("an older snapshot")

	expectRows(t, "history", r.history(id), []string{"dispatched->in_transit: core: in_transit (version 4)"})
}

// TestCoreWord_AnEqualVersionWithAnotherStatusIsKeptAndRecorded: Core bumps
// the version with every status write, so another status at a version already
// applied is a defect to find. The row keeps what it has, no event fires, and
// a VERSION CLASH history row says what Core said. The same status at the same
// version is a repeated copy and writes nothing.
func TestCoreWord_AnEqualVersionWithAnotherStatusIsKeptAndRecorded(t *testing.T) {
	t.Parallel()
	r := newWordRig(t)
	id := r.leg("clash-leg", protocol.StatusDispatched)
	r.update(protocol.OrderUpdate{OrderUUID: "clash-leg", Status: string(protocol.StatusInTransit), Version: 4})
	r.expect("in_transit v4", "status dispatched->in_transit")

	r.update(protocol.OrderUpdate{OrderUUID: "clash-leg", Status: string(protocol.StatusInTransit), Version: 4})
	r.at("a repeated copy", id, protocol.StatusInTransit, 4)
	r.expect("a repeated copy")

	r.update(protocol.OrderUpdate{OrderUUID: "clash-leg", Status: string(protocol.StatusStaged), Version: 4})
	if o := r.at("staged v4", id, protocol.StatusInTransit, 4); o.StagedAt != nil {
		t.Errorf("the clash stamped staged_at %v; want none", o.StagedAt)
	}
	r.expect("staged v4")

	expectRows(t, "history", r.history(id), []string{
		"dispatched->in_transit: core: in_transit (version 4)",
		"in_transit->in_transit: VERSION CLASH: Core says staged at version 4, which this Edge applied as in_transit; the row is kept",
	})
}

// TestCoreWord_DeliveredFollowOnsRunOnceWhicheverHalfLandsFirst: a delivery is
// two messages, order.delivered (the carrier's facts) and Core's delivered
// (the status). The delivered event, with the envelope's bin, fires once when
// the second lands, in either order; an auto-confirmed order then confirms
// once, with one receipt. A repeat of either half adds nothing.
func TestCoreWord_DeliveredFollowOnsRunOnceWhicheverHalfLandsFirst(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		autoConfirm   bool
		envelopeFirst bool
	}{
		{name: "the envelope, then Core's delivered", envelopeFirst: true},
		{name: "Core's delivered, then the envelope"},
		{name: "an auto-confirmed order: the envelope, then Core's delivered", autoConfirm: true, envelopeFirst: true},
		{name: "an auto-confirmed order: Core's delivered, then the envelope", autoConfirm: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newWordRig(t)
			const uuid = "deliver-leg"
			var id int64
			if tc.autoConfirm {
				id = r.autoLeg(uuid, protocol.StatusDispatched)
			} else {
				id = r.leg(uuid, protocol.StatusDispatched)
			}
			r.live(uuid, protocol.StatusInTransit, 3)

			bin, uop := int64(41), 12
			envelope := func() {
				r.h.HandleOrderDelivered(wire, &protocol.OrderDelivered{OrderUUID: uuid,
					DeliveredAt: time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC), BinID: &bin, UOPRemaining: &uop, DeltaEpoch: 2})
			}
			tell := func() {
				r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusDelivered), Version: 4})
			}
			followOns := []string{"delivered"}
			if tc.autoConfirm {
				followOns = append(followOns, "status delivered->confirmed", "completed")
			}

			if tc.envelopeFirst {
				envelope()
				r.expect("the envelope first")
				tell()
				r.expect("Core's delivered second", append([]string{"status in_transit->delivered"}, followOns...)...)
			} else {
				tell()
				r.expect("Core's delivered first", "status in_transit->delivered")
				envelope()
				r.expect("the envelope second", followOns...)
			}

			envelope()
			tell()
			r.expect("both halves again")

			want, history := protocol.StatusDelivered, []string{
				"dispatched->in_transit: core: in_transit (version 3)",
				"in_transit->delivered: core: delivered (version 4)",
			}
			if tc.autoConfirm {
				want, history = protocol.StatusConfirmed, append(history, "delivered->confirmed: confirmed with count 1")
			}
			r.at("the end", id, want, 4)
			expectRows(t, "history", r.history(id), history)
			if len(r.delivered) != 1 || r.delivered[0].BinID == nil || *r.delivered[0].BinID != bin ||
				r.delivered[0].BinUOP == nil || *r.delivered[0].BinUOP != uop || r.delivered[0].BinEpoch != 2 {
				t.Errorf("delivered events %+v, want one carrying bin %d, uop %d, epoch 2", r.delivered, bin, uop)
			}
			receipts := r.receipts()
			switch {
			case tc.autoConfirm && (len(receipts) != 1 || receipts[0].OrderUUID != uuid || receipts[0].FinalCount != 1):
				t.Errorf("order.receipt rows %+v, want the auto-confirm's one, count 1", receipts)
			case !tc.autoConfirm && len(receipts) != 0:
				t.Errorf("order.receipt rows %+v, want none before the operator confirms", receipts)
			}
		})
	}
}

// TestCoreWord_CoresEndToastsAndHoldsOnceAtTheLiveToTerminalEdge: Core ending
// a live order is announced where it ends the row, the same for a live tell
// and a reconcile: a cancel toasts the station with Core's reason and holds the
// position's reorder (off when Core gave up waiting), a failure holds it
// paused, both complete the order and take its release intent. The dedicated
// envelope that follows adds nothing. A swap's partner, cancelled after its
// sibling, is held but not toasted again.
func TestCoreWord_CoresEndToastsAndHoldsOnceAtTheLiveToTerminalEdge(t *testing.T) {
	t.Parallel()
	const fleet = "fleet order stopped: RDS error 60009"
	abandoned := protocol.CancelReasonAbandonedPrefix + " waited 40m for a bin"
	for _, tc := range []struct {
		name       string
		end        func(r *wordRig)
		envelope   func(r *wordRig) // the dedicated envelope Core sends with it
		wantStatus protocol.Status
		wantEvents []string
		wantToasts []string
		wantHold   domain.ReorderHold
		wantDetail string
	}{
		{
			name: "order.update cancelled",
			end: func(r *wordRig) {
				r.update(protocol.OrderUpdate{OrderUUID: "end-leg", Status: string(protocol.StatusCancelled), Version: 5, Detail: fleet})
			},
			envelope: func(r *wordRig) {
				r.h.HandleOrderCancelled(wire, &protocol.OrderCancelled{OrderUUID: "end-leg", Reason: fleet})
			},
			wantStatus: protocol.StatusCancelled,
			wantEvents: []string{"status in_transit->cancelled", "completed", "cancelled by Core"},
			wantToasts: []string{fleet},
			wantHold:   domain.ReorderHoldPaused,
			wantDetail: fleet,
		},
		{
			name: "a reconcile snapshot that says cancelled",
			end: func(r *wordRig) {
				r.snapshot(protocol.OrderStatusSnapshot{OrderUUID: "end-leg", Status: string(protocol.StatusCancelled),
					Version: 5, ErrorDetail: fleet})
			},
			envelope: func(r *wordRig) {
				r.h.HandleOrderCancelled(wire, &protocol.OrderCancelled{OrderUUID: "end-leg", Reason: fleet})
			},
			wantStatus: protocol.StatusCancelled,
			wantEvents: []string{"status in_transit->cancelled", "completed", "cancelled by Core"},
			wantToasts: []string{fleet},
			wantHold:   domain.ReorderHoldPaused,
			wantDetail: fleet,
		},
		{
			name: "order.update cancelled because Core gave up waiting",
			end: func(r *wordRig) {
				r.update(protocol.OrderUpdate{OrderUUID: "end-leg", Status: string(protocol.StatusCancelled), Version: 5, Detail: abandoned})
			},
			envelope: func(r *wordRig) {
				r.h.HandleOrderCancelled(wire, &protocol.OrderCancelled{OrderUUID: "end-leg", Reason: abandoned})
			},
			wantStatus: protocol.StatusCancelled,
			wantEvents: []string{"status in_transit->cancelled", "completed", "cancelled by Core"},
			wantToasts: []string{abandoned},
			wantHold:   domain.ReorderHoldOff,
			wantDetail: abandoned,
		},
		{
			name: "order.update failed",
			end: func(r *wordRig) {
				r.update(protocol.OrderUpdate{OrderUUID: "end-leg", Status: string(protocol.StatusFailed), Version: 5, Detail: fleet})
			},
			envelope: func(r *wordRig) {
				r.h.HandleOrderError(wire, &protocol.OrderError{OrderUUID: "end-leg", ErrorCode: "fleet_failed", Detail: fleet})
			},
			wantStatus: protocol.StatusFailed,
			wantEvents: []string{"status in_transit->failed", "completed", "failed"},
			wantHold:   domain.ReorderHoldPaused,
			wantDetail: fleet,
		},
		{
			// The failed event carries the code the tell carries, and only that
			// one: order.error's code is the same failure's and writes nothing.
			name: "order.update failed with Core's code",
			end: func(r *wordRig) {
				r.update(protocol.OrderUpdate{OrderUUID: "end-leg", Status: string(protocol.StatusFailed), Version: 5,
					ErrorCode: string(protocol.TermNoBin), Detail: "complex order 9 has no source pickup"})
			},
			envelope: func(r *wordRig) {
				r.h.HandleOrderError(wire, &protocol.OrderError{OrderUUID: "end-leg", ErrorCode: string(protocol.TermNoBin),
					Detail: "complex order 9 has no source pickup"})
			},
			wantStatus: protocol.StatusFailed,
			wantEvents: []string{"status in_transit->failed", "completed", "failed no_bin"},
			wantHold:   domain.ReorderHoldPaused,
			wantDetail: "complex order 9 has no source pickup",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newWordRig(t)
			id := r.leg("end-leg", protocol.StatusDispatched)
			r.live("end-leg", protocol.StatusInTransit, 4)
			testutil.MustNoErr(t, r.db.SetOrderReleaseIntent(id, `{"station_wait":0}`), "a release intent on the leg")

			tc.end(r)
			if o := r.at("Core's end", id, tc.wantStatus, 5); o.ReleaseIntent != "" {
				t.Errorf("the ended row keeps its release intent %q", o.ReleaseIntent)
			}
			r.expect("Core's end", tc.wantEvents...)
			if got := r.hold(); got != tc.wantHold {
				t.Errorf("after Core's end the position holds %q, want %q", got, tc.wantHold)
			}

			tc.envelope(r)
			r.at("the envelope after", id, tc.wantStatus, 5)
			r.expect("the envelope after")
			if !slices.Equal(r.toasts, tc.wantToasts) {
				t.Errorf("toasts %q, want %q", r.toasts, tc.wantToasts)
			}
			if got := r.hold(); got != tc.wantHold {
				t.Errorf("after the envelope the position holds %q, want %q", got, tc.wantHold)
			}
			expectRows(t, "history", r.history(id), []string{
				"dispatched->in_transit: core: in_transit (version 4)",
				"in_transit->" + string(tc.wantStatus) + ": " + tc.wantDetail,
			})
		})
	}

	t.Run("a swap's partner after its sibling", func(t *testing.T) {
		t.Parallel()
		r := newWordRig(t)
		dead := r.leg("dead-leg", protocol.StatusDispatched)
		partner := r.leg("partner-leg", protocol.StatusDispatched)
		testutil.MustNoErr(t, r.db.LinkOrderSiblings(dead, partner), "link the swap")
		r.live("dead-leg", protocol.StatusInTransit, 3)
		r.live("partner-leg", protocol.StatusInTransit, 3)

		r.update(protocol.OrderUpdate{OrderUUID: "dead-leg", Status: string(protocol.StatusCancelled), Version: 4, Detail: fleet})
		r.expect("the dead leg", "status in_transit->cancelled", "completed", "cancelled by Core")
		const cascade = "coordinated swap: partner cancelled, cancelling this leg"
		r.update(protocol.OrderUpdate{OrderUUID: "partner-leg", Status: string(protocol.StatusCancelled), Version: 4, Detail: cascade})
		r.expect("the partner", "status in_transit->cancelled", "completed")

		if !slices.Equal(r.toasts, []string{fleet}) {
			t.Errorf("toasts %q, want the dead leg's one", r.toasts)
		}
		if got := r.hold(); got != domain.ReorderHoldPaused {
			t.Errorf("the position holds %q, want paused", got)
		}
	})
}

// TestCoreWord_AnOperatorsCancelCoreStillHoldsLiveIsSentAgain: the operator
// aborts and the row ends cancelled. A newer word of Core's that the order is
// live means Core has not acted on the cancel (it may be lost): the row stays
// cancelled and the cancel goes again, with the abort's reason. A reconcile
// says the same at the version already applied, so it sends again too; a live
// tell at that version does not. Core's cancelled ends it: nothing more, no
// toast, no hold.
func TestCoreWord_AnOperatorsCancelCoreStillHoldsLiveIsSentAgain(t *testing.T) {
	t.Parallel()
	r := newWordRig(t)
	const uuid = "abort-leg"
	reason := protocol.CancelReasonAcceptHalfSwap
	id := r.leg(uuid, protocol.StatusDispatched)
	r.live(uuid, protocol.StatusInTransit, 2)

	testutil.MustNoErr(t, r.eng.OrderManager().AbortOrderWithReason(id, reason), "the operator's abort")
	r.expect("the abort", "status in_transit->cancelled", "completed")
	expectCancels(t, "the abort", r.cancels(), uuid, reason, 1)

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusInTransit), Version: 3, RobotID: "AMB-2"})
	if o := r.at("in_transit v3", id, protocol.StatusCancelled, 3); o.RobotID != "" {
		t.Errorf("Core's word on the cancelled row wrote robot %q", o.RobotID)
	}
	r.expect("in_transit v3")
	expectCancels(t, "in_transit v3", r.cancels(), uuid, reason, 2)

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusInTransit), Version: 3})
	r.at("a repeated in_transit v3", id, protocol.StatusCancelled, 3)
	expectCancels(t, "a repeated in_transit v3", r.cancels(), uuid, reason, 2)

	r.snapshot(protocol.OrderStatusSnapshot{OrderUUID: uuid, Status: string(protocol.StatusInTransit), Version: 3})
	r.at("the reconcile at v3", id, protocol.StatusCancelled, 3)
	r.expect("the reconcile at v3")
	expectCancels(t, "the reconcile at v3", r.cancels(), uuid, reason, 3)

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusInTransit), Version: 2})
	expectCancels(t, "an older in_transit v2", r.cancels(), uuid, reason, 3)

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusCancelled), Version: 4, Detail: reason})
	r.h.HandleOrderCancelled(wire, &protocol.OrderCancelled{OrderUUID: uuid, Reason: reason})
	r.at("Core's cancelled v4", id, protocol.StatusCancelled, 4)
	r.expect("Core's cancelled v4")
	expectCancels(t, "Core's cancelled v4", r.cancels(), uuid, reason, 3)
	if len(r.toasts) != 0 {
		t.Errorf("toasts %q on the operator's own cancel, want none", r.toasts)
	}
	if got := r.hold(); got != domain.ReorderHoldNone {
		t.Errorf("the position holds %q, want none: the abort holds nothing and Core's echo is not an end", got)
	}
	expectRows(t, "history", r.history(id), []string{
		"dispatched->in_transit: core: in_transit (version 2)",
		"in_transit->cancelled: " + reason,
	})
}

// TestCoreWord_AnOperatorsConfirmHoldsCoresDelivered: the operator confirms a
// delivered order before Core has the receipt; Core's delivered, by reconcile
// or by a newer tell, finds the row ended and is held: the row stays confirmed,
// nothing fires, no second receipt, and the version still moves.
func TestCoreWord_AnOperatorsConfirmHoldsCoresDelivered(t *testing.T) {
	t.Parallel()
	r := newWordRig(t)
	const uuid = "confirm-leg"
	id := r.leg(uuid, protocol.StatusDispatched)
	r.live(uuid, protocol.StatusInTransit, 3)
	r.h.HandleOrderDelivered(wire, &protocol.OrderDelivered{OrderUUID: uuid, DeliveredAt: time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)})
	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusDelivered), Version: 4})
	r.expect("the delivery", "status in_transit->delivered", "delivered")
	testutil.MustNoErr(t, r.eng.OrderManager().ConfirmDelivery(id, 1), "the operator's confirm")
	r.expect("the operator's confirm", "status delivered->confirmed", "completed")
	before := r.history(id)

	r.snapshot(protocol.OrderStatusSnapshot{OrderUUID: uuid, Status: string(protocol.StatusDelivered), Version: 4})
	r.at("the reconcile at v4", id, protocol.StatusConfirmed, 4)
	r.expect("the reconcile at v4")

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusDelivered), Version: 5})
	r.at("delivered v5", id, protocol.StatusConfirmed, 5)
	r.expect("delivered v5")

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusConfirmed), Version: 6})
	r.at("confirmed v6", id, protocol.StatusConfirmed, 6)
	r.expect("confirmed v6")

	expectRows(t, "history", r.history(id), before)
	if got := r.receipts(); len(got) != 1 {
		t.Errorf("order.receipt rows %+v, want the operator's one", got)
	}
}

// TestCoreWord_ARobotSwapMovesTheRobotOnly: a newer word with the status the
// row has writes the robot and the ETA, with no history row and no
// EventOrderStatusChanged; the boards drawing the order are nudged. An empty
// robot or ETA on the wire is absent and keeps the stored one.
func TestCoreWord_ARobotSwapMovesTheRobotOnly(t *testing.T) {
	t.Parallel()
	r := newWordRig(t)
	const uuid = "swap-robot-leg"
	id := r.leg(uuid, protocol.StatusDispatched)
	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusInTransit), Version: 3, RobotID: "AMB-1", ETA: "12:05"})
	r.expect("in_transit v3", "status dispatched->in_transit")

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusInTransit), Version: 4, RobotID: "AMB-2", ETA: "12:09"})
	if o := r.at("the swap v4", id, protocol.StatusInTransit, 4); o.RobotID != "AMB-2" || textOf(o.ETA) != "12:09" {
		t.Errorf("after the swap: robot %q, ETA %s; want AMB-2, 12:09", o.RobotID, textOf(o.ETA))
	}
	r.expect("the swap v4", "touched")

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusInTransit), Version: 5})
	if o := r.at("in_transit v5, no robot named", id, protocol.StatusInTransit, 5); o.RobotID != "AMB-2" || textOf(o.ETA) != "12:09" {
		t.Errorf("after a word naming no robot: robot %q, ETA %s; want AMB-2, 12:09 kept", o.RobotID, textOf(o.ETA))
	}
	r.expect("in_transit v5, no robot named", "touched")

	expectRows(t, "history", r.history(id), []string{"dispatched->in_transit: core: in_transit (version 3)"})
}

// TestCoreWord_WithAVersionTheDedicatedEnvelopesWriteNoStatus: on a row whose
// status is Core's versioned word, the ack, order.staged, order.error,
// order.cancelled and order.skipped write no status and fire nothing (the
// toast and the holds are the applier's). Each keeps its own work: order.staged
// records where the robot parked, which Core's staged then keeps; order.error's
// bins rows are applied; order.skipped advances its changeover node task.
func TestCoreWord_WithAVersionTheDedicatedEnvelopesWriteNoStatus(t *testing.T) {
	t.Parallel()
	const uuid = "envelope-leg"
	for _, tc := range []struct {
		name     string
		from     protocol.Status
		envelope func(r *wordRig)
		wantRows []string // the bins rows the envelope carries, still applied
	}{
		{name: "the ack", from: protocol.StatusDispatched, envelope: func(r *wordRig) {
			r.h.HandleOrderAck(wire, &protocol.OrderAck{OrderUUID: uuid, ShingoOrderID: 9})
		}},
		{name: "order.error fleet_failed", from: protocol.StatusInTransit, envelope: func(r *wordRig) {
			r.h.HandleOrderError(wire, &protocol.OrderError{OrderUUID: uuid, ErrorCode: "fleet_failed", Detail: "RDS error 60009",
				Rows: []protocol.NodeBinsRow{{Node: "TEST-NODE", State: "occupied", Occupied: true}}})
		}, wantRows: []string{"TEST-NODE"}},
		{name: "order.cancelled", from: protocol.StatusInTransit, envelope: func(r *wordRig) {
			r.h.HandleOrderCancelled(wire, &protocol.OrderCancelled{OrderUUID: uuid, Reason: "RDS error 60009"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newWordRig(t)
			id := r.leg(uuid, protocol.StatusAcknowledged)
			r.live(uuid, tc.from, 2)

			tc.envelope(r)
			r.at(tc.name, id, tc.from, 2)
			r.expect(tc.name)
			if !slices.Equal(r.binsRows, tc.wantRows) {
				t.Errorf("bins rows applied for %q, want %q", r.binsRows, tc.wantRows)
			}
			if len(r.toasts) != 0 {
				t.Errorf("toasts %q, want none", r.toasts)
			}
			if got := r.hold(); got != domain.ReorderHoldNone {
				t.Errorf("the position holds %q, want none", got)
			}
			expectRows(t, "history", r.history(id), []string{
				"acknowledged->" + string(tc.from) + ": core: " + string(tc.from) + " (version 2)",
			})
		})
	}

	t.Run("order.staged, then Core's staged", func(t *testing.T) {
		t.Parallel()
		r := newWordRig(t)
		id := r.leg(uuid, protocol.StatusDispatched)
		r.live(uuid, protocol.StatusInTransit, 3)

		wait := 2
		r.h.HandleOrderStaged(wire, &protocol.OrderStaged{OrderUUID: uuid, Detail: "robot at the station's wait 2",
			StationWait: &wait, WaitKind: protocol.WaitKindStation})
		o := r.at("order.staged", id, protocol.StatusInTransit, 3)
		if o.StationWait == nil || *o.StationWait != wait || o.WaitKind != protocol.WaitKindStation || o.StagedAt != nil {
			t.Errorf("after order.staged: wait %v, kind %q, staged_at %v; want wait 2 at %q, no staged_at",
				o.StationWait, o.WaitKind, o.StagedAt, protocol.WaitKindStation)
		}
		r.expect("order.staged")

		r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusStaged), Version: 4})
		o = r.at("staged v4", id, protocol.StatusStaged, 4)
		if o.StationWait == nil || *o.StationWait != wait || o.WaitKind != protocol.WaitKindStation || o.StagedAt == nil {
			t.Errorf("after Core's staged: wait %v, kind %q, staged_at %v; want wait 2 at %q kept, staged_at stamped",
				o.StationWait, o.WaitKind, o.StagedAt, protocol.WaitKindStation)
		}
		r.expect("staged v4", "status in_transit->staged")
		expectRows(t, "history", r.history(id), []string{
			"dispatched->in_transit: core: in_transit (version 3)",
			"in_transit->staged: core: staged (version 4)",
		})
	})

	t.Run("order.skipped", func(t *testing.T) {
		t.Parallel()
		r := newWordRig(t)
		id := r.leg(uuid, protocol.StatusSubmitted)
		r.live(uuid, protocol.StatusAcknowledged, 2)
		changeoverID := evacTaskFor(t, r, id)

		r.h.HandleOrderSkipped(wire, &protocol.OrderSkipped{OrderUUID: uuid, ErrorCode: "no_source_bin", Detail: "every pickup empty"})
		r.at("order.skipped", id, protocol.StatusAcknowledged, 2)
		r.expect("order.skipped")
		task, err := r.db.GetChangeoverNodeTaskByNode(changeoverID, r.nodeID)
		testutil.MustNoErr(t, err, "read the node task")
		if task.State != domain.NodeTaskLineCleared || task.SkipNote != "evac skipped: bin missing at "+task.NodeName {
			t.Errorf("the evac's node task reads %s, note %q; want line_cleared, %q",
				task.State, task.SkipNote, "evac skipped: bin missing at "+task.NodeName)
		}
		expectRows(t, "history", r.history(id), []string{"submitted->acknowledged: core: acknowledged (version 2)"})
	})
}

// evacTaskFor makes a changeover at the rig's position whose node task
// evacuates with the order, and returns the changeover's id.
func evacTaskFor(t *testing.T, r *wordRig, orderID int64) int64 {
	t.Helper()
	var processID int64
	testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT process_id FROM process_nodes WHERE id=?`, r.nodeID).Scan(&processID), "read the process")
	styleID, err := r.db.CreateStyle("STYLE-B", "", processID)
	testutil.MustNoErr(t, err, "create the style")
	res, err := r.db.DB.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state) VALUES (?, ?, 'running')`, processID, styleID)
	testutil.MustNoErr(t, err, "insert the changeover")
	changeoverID, err := res.LastInsertId()
	testutil.MustNoErr(t, err, "the changeover's id")
	_, err = r.db.DB.Exec(`INSERT INTO changeover_node_tasks (process_changeover_id, process_node_id, situation, state,
		old_material_release_order_id) VALUES (?, ?, 'drop', 'empty_requested', ?)`, changeoverID, r.nodeID, orderID)
	testutil.MustNoErr(t, err, "insert the node task")
	return changeoverID
}

// TestCoreWord_AVersionedProjectionGoesThroughTheApplier: a projection of a row
// this Edge holds writes the order's fields and moves its status through the
// applier: newer only, its history row and EventOrderStatusChanged, the wait
// kept only while the order waits. A projection of an ended row writes nothing;
// one from the reconcile's Unlisted list of an operator-cancelled row sends the
// cancel again.
func TestCoreWord_AVersionedProjectionGoesThroughTheApplier(t *testing.T) {
	t.Parallel()
	const waitText = "no bin of PART-N in AMR Supermarket"
	projection := func(uuid string, status protocol.Status, version int64) protocol.OrderProjection {
		return protocol.OrderProjection{OrderUUID: uuid, OrderType: protocol.OrderTypeRetrieve, Status: string(status),
			Quantity: 3, SourceNode: "SRC-2", DeliveryNode: "TEST-NODE",
			PayloadCode: "PART-N", PayloadDesc: "part N", Version: version}
	}

	t.Run("a live row", func(t *testing.T) {
		t.Parallel()
		r := newWordRig(t)
		const uuid = "projected-leg"
		id := r.leg(uuid, protocol.StatusAcknowledged)
		r.live(uuid, protocol.StatusDispatched, 2)

		p := projection(uuid, protocol.StatusQueued, 3)
		p.QueueReason, p.QueueCode = waitText, string(protocol.QueueWaitingForMaterial)
		_, err := r.eng.ApplyOrderProjection(p)
		testutil.MustNoErr(t, err, "the projection")
		o := r.at("queued v3", id, protocol.StatusQueued, 3)
		if o.QueueReason != waitText || o.QueueCode != string(protocol.QueueWaitingForMaterial) ||
			o.Quantity != 3 || o.PayloadCode != "PART-N" || o.PayloadDesc != "part N" || o.SourceNode != "SRC-2" {
			t.Errorf("after the projection: wait %q (%s), quantity %d, payload %q %q, source %q; "+
				"want %q (%s), 3, PART-N, part N, SRC-2", o.QueueReason, o.QueueCode, o.Quantity, o.PayloadCode,
				o.PayloadDesc, o.SourceNode, waitText, protocol.QueueWaitingForMaterial)
		}
		r.expect("queued v3", "status dispatched->queued")

		_, err = r.eng.ApplyOrderProjection(projection(uuid, protocol.StatusDispatched, 2))
		testutil.MustNoErr(t, err, "an older projection")
		if o := r.at("an older projection", id, protocol.StatusQueued, 3); o.QueueReason != waitText {
			t.Errorf("the older projection cleared the wait to %q", o.QueueReason)
		}
		r.expect("an older projection")

		r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusDispatched), Version: 4, QueueReason: "stale"})
		if o := r.at("dispatched v4", id, protocol.StatusDispatched, 4); o.QueueReason != "" || o.QueueCode != "" {
			t.Errorf("a dispatched order keeps the wait %q (%s)", o.QueueReason, o.QueueCode)
		}
		r.expect("dispatched v4", "status queued->dispatched")

		expectRows(t, "history", r.history(id), []string{
			"acknowledged->dispatched: core: dispatched (version 2)",
			"dispatched->queued: projected by Core",
			"queued->dispatched: core: dispatched (version 4)",
		})
	})

	t.Run("an operator-confirmed row", func(t *testing.T) {
		t.Parallel()
		r := newWordRig(t)
		const uuid = "projected-confirmed-leg"
		id := r.leg(uuid, protocol.StatusDispatched)
		r.live(uuid, protocol.StatusInTransit, 3)
		r.h.HandleOrderDelivered(wire, &protocol.OrderDelivered{OrderUUID: uuid, DeliveredAt: time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)})
		r.live(uuid, protocol.StatusDelivered, 4)
		testutil.MustNoErr(t, r.eng.OrderManager().ConfirmDelivery(id, 1), "the operator's confirm")
		r.events = nil
		ended, before := r.row(id), r.history(id)

		_, err := r.eng.ApplyOrderProjection(projection(uuid, protocol.StatusDelivered, 5))
		testutil.MustNoErr(t, err, "the projection")
		after := r.row(id)
		if after.CoreVersion != 5 {
			t.Errorf("the projection left the version at %d, want 5 noted", after.CoreVersion)
		}
		after.CoreVersion = ended.CoreVersion
		if !reflect.DeepEqual(after, ended) {
			t.Errorf("the projection changed the confirmed row: status %s, quantity %d, payload %q; want %s, %d, %q",
				after.Status, after.Quantity, after.PayloadCode, ended.Status, ended.Quantity, ended.PayloadCode)
		}
		r.expect("the projection")
		expectRows(t, "history", r.history(id), before)
		if got := r.cancels(); len(got) != 0 {
			t.Errorf("order.cancel rows %+v, want none", got)
		}
	})

	t.Run("an operator-cancelled row from the Unlisted list", func(t *testing.T) {
		t.Parallel()
		r := newWordRig(t)
		const uuid = "projected-cancelled-leg"
		id := r.leg(uuid, protocol.StatusDispatched)
		r.live(uuid, protocol.StatusInTransit, 3)
		testutil.MustNoErr(t, r.eng.OrderManager().AbortOrderWithReason(id, "aborted by operator"), "the operator's abort")
		r.events = nil
		ended, before := r.row(id), r.history(id)

		_, err := r.eng.ApplyOrderProjection(projection(uuid, protocol.StatusInTransit, 3))
		testutil.MustNoErr(t, err, "a pushed projection at v3")
		expectCancels(t, "a pushed projection at v3", r.cancels(), uuid, "aborted by operator", 1)

		r.eng.HandleUnlistedOrders([]protocol.OrderProjection{projection(uuid, protocol.StatusInTransit, 3)})
		if after := r.row(id); !reflect.DeepEqual(after, ended) {
			t.Errorf("the Unlisted projection changed the cancelled row: status %s at %d, quantity %d, payload %q; want %s at %d, %d, %q",
				after.Status, after.CoreVersion, after.Quantity, after.PayloadCode, ended.Status, ended.CoreVersion, ended.Quantity, ended.PayloadCode)
		}
		r.expect("the Unlisted projection")
		expectRows(t, "history", r.history(id), before)
		expectCancels(t, "the Unlisted projection", r.cancels(), uuid, "aborted by operator", 2)
	})
}
