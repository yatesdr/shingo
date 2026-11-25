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
	"fmt"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
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

// TestCoreWord_AnOperatorsCancelCoreStillHoldsLiveIsSentAgain: the operator's
// cancel of a row whose status is Core's word asks Core (final brief 5f): the
// row stays live with its cancel sent. A newer live word of Core's is the
// cancel's ordinary crossing (the cancel is still ahead on the outbox, which
// never expires or dead-letters it): it moves the row and sends nothing. A
// reconcile that finds Core holding the order live once the cancel is older
// than an ask's life sends it again, with its reason, asking. Core's
// cancelled ends it: no toast, no hold. Until 2026-11-25 the row ended at the
// tap, and S3t kept the ended row and sent the cancel again on any newer live
// word (resendCancel, now only for a row ended at the tap).
func TestCoreWord_AnOperatorsCancelCoreStillHoldsLiveIsSentAgain(t *testing.T) {
	t.Parallel()
	r := newWordRig(t)
	const uuid = "abort-leg"
	reason := protocol.CancelReasonAcceptHalfSwap
	id := r.leg(uuid, protocol.StatusDispatched)
	r.live(uuid, protocol.StatusInTransit, 2)

	testutil.MustNoErr(t, r.eng.OrderManager().CancelOrderWithReason(id, reason), "the operator's cancel")
	if o := r.at("the cancel", id, protocol.StatusInTransit, 2); !o.CancelSent {
		t.Errorf("after the cancel the row reads cancel sent %v, want sent", o.CancelSent)
	}
	r.expect("the cancel", "touched")
	expectCancels(t, "the cancel", r.cancels(), uuid, reason, 1)
	if c := r.cancels()[0]; !c.Intent {
		t.Errorf("the cancel %+v does not ask", c)
	}

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusInTransit), Version: 3, RobotID: "AMB-2"})
	if o := r.at("in_transit v3", id, protocol.StatusInTransit, 3); o.RobotID != "AMB-2" || !o.CancelSent {
		t.Errorf("Core's live word left robot %q, cancel sent %v; want AMB-2 and the cancel still out", o.RobotID, o.CancelSent)
	}
	r.expect("in_transit v3", "touched")
	expectCancels(t, "in_transit v3", r.cancels(), uuid, reason, 1)

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusInTransit), Version: 3})
	r.at("a repeated in_transit v3", id, protocol.StatusInTransit, 3)
	expectCancels(t, "a repeated in_transit v3", r.cancels(), uuid, reason, 1)

	r.snapshot(protocol.OrderStatusSnapshot{OrderUUID: uuid, Status: string(protocol.StatusInTransit), Version: 3})
	expectCancels(t, "a reconcile while the cancel is young", r.cancels(), uuid, reason, 1)
	// The cancel went out before an outage: older than an ask's whole life.
	_, err := r.db.DB.Exec(`UPDATE orders SET cancel_intent = json_set(cancel_intent, '$.sent_at', '2026-11-25T12:00:00Z') WHERE id = ?`, id)
	testutil.MustNoErr(t, err, "age the cancel")
	r.snapshot(protocol.OrderStatusSnapshot{OrderUUID: uuid, Status: string(protocol.StatusInTransit), Version: 3})
	r.at("the reconcile at v3", id, protocol.StatusInTransit, 3)
	r.expect("the reconcile at v3")
	expectCancels(t, "the reconcile at v3", r.cancels(), uuid, reason, 2)
	if c := r.cancels()[1]; !c.Intent {
		t.Errorf("the copy %+v does not ask", c)
	}

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusInTransit), Version: 2})
	expectCancels(t, "an older in_transit v2", r.cancels(), uuid, reason, 2)

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusCancelled), Version: 4, Detail: reason})
	r.h.HandleOrderCancelled(wire, &protocol.OrderCancelled{OrderUUID: uuid, Reason: reason})
	r.at("Core's cancelled v4", id, protocol.StatusCancelled, 4)
	r.expect("Core's cancelled v4", "status in_transit->cancelled", "completed")
	expectCancels(t, "Core's cancelled v4", r.cancels(), uuid, reason, 2)
	if len(r.toasts) != 0 {
		t.Errorf("toasts %q on the operator's own cancel, want none", r.toasts)
	}
	if got := r.hold(); got != domain.ReorderHoldNone {
		t.Errorf("the position holds %q, want none: the door holds nothing for a cancel no person asked", got)
	}
	expectRows(t, "history", r.history(id), []string{
		"dispatched->in_transit: core: in_transit (version 2)",
		"in_transit->cancelled: " + reason,
	})
}

// TestCoreWord_ACancelEndedAtTheTapIsSentAgainOnALiveWord: a cancel of an
// order no versioned word of Core's had reached ends the row at the tap (the
// one door's old body). Core's first versioned word that the order is live
// finds the row ended: the row stays cancelled and the cancel goes again
// with the reason the row ended on (S3t's arm, resendCancel, kept one release
// for rows ended at the tap).
func TestCoreWord_ACancelEndedAtTheTapIsSentAgainOnALiveWord(t *testing.T) {
	t.Parallel()
	r := newWordRig(t)
	const uuid = "tap-ended-leg"
	reason := protocol.CancelReasonAcceptHalfSwap
	id := r.leg(uuid, protocol.StatusDispatched)

	testutil.MustNoErr(t, r.eng.OrderManager().CancelOrderWithReason(id, reason), "the cancel of an unversioned row")
	r.at("the cancel", id, protocol.StatusCancelled, 0)
	expectCancels(t, "the cancel", r.cancels(), uuid, reason, 1)

	r.update(protocol.OrderUpdate{OrderUUID: uuid, Status: string(protocol.StatusInTransit), Version: 1})
	r.at("Core's first word", id, protocol.StatusCancelled, 1)
	expectCancels(t, "Core's first word", r.cancels(), uuid, reason, 2)
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
		// The cancel asks Core (final brief 5f) and the row stays live; it went
		// out before an outage, older than an ask's whole life.
		testutil.MustNoErr(t, r.eng.OrderManager().CancelOrderWithReason(id, "cancelled by operator"), "the operator's cancel")
		_, err := r.db.DB.Exec(`UPDATE orders SET cancel_intent = json_set(cancel_intent, '$.sent_at', '2026-11-25T12:00:00Z') WHERE id = ?`, id)
		testutil.MustNoErr(t, err, "age the cancel")
		r.events = nil
		asked, before := r.row(id), r.history(id)

		_, err = r.eng.ApplyOrderProjection(projection(uuid, protocol.StatusInTransit, 3))
		testutil.MustNoErr(t, err, "a pushed projection at v3")
		expectCancels(t, "a pushed projection at v3", r.cancels(), uuid, "cancelled by operator", 1)

		r.eng.HandleUnlistedOrders([]protocol.OrderProjection{projection(uuid, protocol.StatusInTransit, 3)})
		if after := r.row(id); after.Status != asked.Status || after.CoreVersion != asked.CoreVersion ||
			after.Quantity != asked.Quantity || after.PayloadCode != asked.PayloadCode || !after.CancelSent {
			t.Errorf("the Unlisted projection changed the asked row: status %s at %d, quantity %d, payload %q, cancel sent %v; want %s at %d, %d, %q, sent",
				after.Status, after.CoreVersion, after.Quantity, after.PayloadCode, after.CancelSent,
				asked.Status, asked.CoreVersion, asked.Quantity, asked.PayloadCode)
		}
		r.expect("the Unlisted projection")
		expectRows(t, "history", r.history(id), before)
		expectCancels(t, "the Unlisted projection", r.cancels(), uuid, "cancelled by operator", 2)
	})
}

// askCancelByHand writes a cancel this station asked onto the row, as the
// cancel door writes it (orders.cancel_intent), for the readers' pins.
func (r *wordRig) askCancelByHand(id int64, by, env string) {
	r.t.Helper()
	raw, err := domain.CancelIntent{Reason: "cancelled by operator", By: by, Env: env,
		SentAt: "2026-11-25T12:00:00Z"}.Encode()
	testutil.MustNoErr(r.t, err, "encode the cancel")
	_, err = r.db.DB.Exec(`UPDATE orders SET cancel_intent = ? WHERE id = ?`, raw, id)
	testutil.MustNoErr(r.t, err, "write the cancel")
}

// TestCoreWord_TheStationsOwnCancelIsNotCoresToast: Core's word that cancels a
// row carrying the cancel this station asked (unanswered) is the station's
// cancel ended, not Core's: no "Core cancelled" toast, and the hold is the
// asker's (orders coreEnded). A person's (the Orders page) pauses the position
// there; the Edge's own doors hold nothing. The completion event names who
// asked, for the keep-staged kick. A cancel Core refused, then cancelled on its
// own, is Core's again; and a row with no cancel asked is today's.
func TestCoreWord_TheStationsOwnCancelIsNotCoresToast(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, by   string
		refused    bool
		asked      bool
		wantToasts int
		wantHold   domain.ReorderHold
		wantNote   string
		wantBy     string
	}{
		{name: "a person's cancel", asked: true, by: domain.CancelByPerson, wantHold: domain.ReorderHoldPaused,
			wantNote: "a person cancelled order %d", wantBy: domain.CancelByPerson},
		{name: "a deciding flow's cancel", asked: true, by: domain.CancelByDecided, wantBy: domain.CancelByDecided},
		{name: "another door's cancel", asked: true},
		{name: "a cancel Core refused, then Core's own", asked: true, refused: true, wantToasts: 1,
			wantHold: domain.ReorderHoldPaused, wantNote: "Core cancelled order %d (fleet stop)"},
		{name: "no cancel asked", wantToasts: 1, wantHold: domain.ReorderHoldPaused,
			wantNote: "Core cancelled order %d (fleet stop)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newWordRig(t)
			var by []string
			r.eng.Events.Subscribe(func(evt Event) {
				if c, ok := evt.Payload.(OrderCompletedEvent); ok {
					by = append(by, c.CancelBy)
				}
			})
			id := r.leg("asked-leg", protocol.StatusInTransit)
			r.live("asked-leg", protocol.StatusInTransit, 2)
			if tc.asked {
				r.askCancelByHand(id, tc.by, "env-1")
			}
			if tc.refused {
				_, err := r.db.DB.Exec(`UPDATE orders SET cancel_intent = json_set(cancel_intent, '$.refused', 'set down') WHERE id = ?`, id)
				testutil.MustNoErr(t, err, "refuse the cancel")
			}

			r.update(protocol.OrderUpdate{OrderUUID: "asked-leg", Status: string(protocol.StatusCancelled), Version: 3,
				Detail: "fleet stop"})

			o := r.at("Core's cancelled", id, protocol.StatusCancelled, 3)
			if o.CancelIntent != "" {
				t.Errorf("the ended row keeps cancel_intent %q, want it cleared with the terminal", o.CancelIntent)
			}
			if len(r.toasts) != tc.wantToasts {
				t.Errorf("toasts %q, want %d", r.toasts, tc.wantToasts)
			}
			rt, err := r.db.GetProcessNodeRuntime(r.nodeID)
			testutil.MustNoErr(t, err, "the runtime row")
			note := ""
			if tc.wantNote != "" {
				note = fmt.Sprintf(tc.wantNote, id)
			}
			if rt.ReorderHold != tc.wantHold || rt.ReorderHoldNote != note {
				t.Errorf("the position holds %q %q, want %q %q", rt.ReorderHold, rt.ReorderHoldNote, tc.wantHold, note)
			}
			if !slices.Equal(by, []string{tc.wantBy}) {
				t.Errorf("the completion named who asked %q, want [%q]", by, tc.wantBy)
			}
		})
	}
}

// TestCoreWord_ANotFoundToTheCancelEndsTheRow: Core's not_found answer to the
// cancel this station asked (its CorID the cancel's envelope) ends the row
// here, cancelled "Core holds no such order", since no word of Core's will; a
// not_found answering another envelope changes nothing on the versioned row.
func TestCoreWord_ANotFoundToTheCancelEndsTheRow(t *testing.T) {
	t.Parallel()
	r := newWordRig(t)
	id := r.leg("nf-leg", protocol.StatusInTransit)
	r.live("nf-leg", protocol.StatusInTransit, 2)
	r.askCancelByHand(id, domain.CancelByPerson, "env-1")

	r.h.HandleOrderError(&protocol.Envelope{CorID: "env-0"}, &protocol.OrderError{OrderUUID: "nf-leg",
		ErrorCode: "not_found", Detail: "order not found or access denied"})
	r.at("a not_found of another envelope", id, protocol.StatusInTransit, 2)

	r.h.HandleOrderError(&protocol.Envelope{CorID: "env-1"}, &protocol.OrderError{OrderUUID: "nf-leg",
		ErrorCode: "not_found", Detail: "order not found or access denied"})
	o := r.row(id)
	if o.Status != protocol.StatusCancelled || o.CancelIntent != "" {
		t.Errorf("after the not_found of its cancel the row reads %s with %q, want cancelled and no intent", o.Status, o.CancelIntent)
	}
	h := r.history(id)
	if len(h) == 0 || h[len(h)-1] != "in_transit->cancelled: "+orders.CoreHoldsNoSuchOrderDetail {
		t.Errorf("history %q, want it to end in_transit->cancelled: %s", h, orders.CoreHoldsNoSuchOrderDetail)
	}
	rt, err := r.db.GetProcessNodeRuntime(r.nodeID)
	testutil.MustNoErr(t, err, "the runtime row")
	if rt.ReorderHold != domain.ReorderHoldPaused || rt.ReorderHoldNote != fmt.Sprintf("a person cancelled order %d", id) {
		t.Errorf("the position holds %q %q, want the person's pause", rt.ReorderHold, rt.ReorderHoldNote)
	}
}

// TestCoreWord_ARefusalWritesOnlyTheSentence: Core's "cannot" to the cancel
// this station asked (order.error cancel_refused_set_down or
// cancel_refused_ended, its CorID the cancel's envelope) writes Core's sentence
// onto the cancel and nothing else: the status stays Core's, no history row,
// no toast, no hold; Core's own word then moves the row. A refusal of another
// envelope, or the identical one again (Core resends it for a redelivered
// cancel), changes nothing and announces nothing; neither fails the order, which
// an order.error with another code does on a row with no versioned word.
func TestCoreWord_ARefusalWritesOnlyTheSentence(t *testing.T) {
	t.Parallel()
	for _, code := range []string{protocol.CancelRefusedSetDown, protocol.CancelRefusedEnded} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			r := newWordRig(t)
			id := r.leg("refused-leg", protocol.StatusInTransit)
			r.live("refused-leg", protocol.StatusInTransit, 2)
			r.askCancelByHand(id, domain.CancelByPerson, "env-1")
			before := len(r.history(id))
			const sentence = "Not cancelled: its cart was set down at TEST-NODE on 2026-11-25 at 12:00. Confirm it at the station."

			r.h.HandleOrderError(&protocol.Envelope{CorID: "env-0"}, &protocol.OrderError{OrderUUID: "refused-leg",
				ErrorCode: code, Detail: "stale"})
			if o := r.row(id); o.CancelRefused != "" || !o.CancelSent {
				t.Errorf("a refusal of another envelope: the row reads sent=%v refused=%q, want it still sent", o.CancelSent, o.CancelRefused)
			}
			refusal := &protocol.OrderError{OrderUUID: "refused-leg", ErrorCode: code, Detail: sentence}
			r.h.HandleOrderError(&protocol.Envelope{CorID: "env-1"}, refusal)
			// Core sends its refusal again when the same envelope is delivered
			// twice (it keeps no record of one): the identical second changes
			// nothing and announces nothing.
			events, updated := len(r.events), r.row(id).UpdatedAt
			r.h.HandleOrderError(&protocol.Envelope{CorID: "env-1"}, refusal)
			if len(r.events) != events || !r.row(id).UpdatedAt.Equal(updated) {
				t.Errorf("the identical second refusal fired %q and wrote the row, want a no-op", r.events[events:])
			}

			o := r.at("the refusal", id, protocol.StatusInTransit, 2)
			if o.CancelSent || o.CancelRefused != sentence {
				t.Errorf("after the refusal the row reads sent=%v refused=%q, want refused %q", o.CancelSent, o.CancelRefused, sentence)
			}
			if h := r.history(id); len(h) != before {
				t.Errorf("the refusal wrote history %q, want none", h[before:])
			}
			r.update(protocol.OrderUpdate{OrderUUID: "refused-leg", Status: string(protocol.StatusDelivered), Version: 3})
			if o := r.at("Core's delivered", id, protocol.StatusDelivered, 3); o.CancelRefused != sentence {
				t.Errorf("after Core's delivered the row reads refused %q, want Core's sentence still", o.CancelRefused)
			}
			if len(r.toasts) != 0 {
				t.Errorf("toasts %q, want none", r.toasts)
			}
			rt, err := r.db.GetProcessNodeRuntime(r.nodeID)
			testutil.MustNoErr(t, err, "the runtime row")
			if rt.ReorderHold != "" {
				t.Errorf("the position holds %q after a refused cancel, want nothing: no pause names a cancel that did not happen", rt.ReorderHold)
			}
		})
	}
}

// TestCoreWord_AReconcileSendsAnUnansweredCancelAgain: a reconcile's word (the
// boot or gap snapshot) that finds Core holding live an order whose asked
// cancel is unanswered and older than an ask's life sends the cancel again,
// with its reason and its actor and asking as the first did; the cancel's
// envelope moves to the copy. A live tell, a young cancel, a refused one and a
// row with none send nothing.
func TestCoreWord_AReconcileSendsAnUnansweredCancelAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		young     bool
		refused   bool
		asked     bool
		reconcile bool
		want      int
	}{
		{name: "a reconcile, an old unanswered cancel", asked: true, reconcile: true, want: 1},
		{name: "a live tell", asked: true},
		{name: "a young cancel", asked: true, young: true, reconcile: true},
		{name: "a refused cancel", asked: true, refused: true, reconcile: true},
		{name: "no cancel asked", reconcile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newWordRig(t)
			id := r.leg("resend-leg", protocol.StatusStaged)
			r.live("resend-leg", protocol.StatusStaged, 2)
			if tc.asked {
				in := domain.CancelIntent{Reason: protocol.CancelReasonLineIdle, Actor: protocol.CancelActorEdge, Env: "env-1",
					SentAt: "2026-11-25T12:00:00Z"}
				// Young is stamped here, on the clock the resend reads, and not
				// when the parent test starts: a parallel subtest can wait longer
				// than the resend's 35 s for a slot under load, and a stamp taken
				// before that wait reads old (the push gate of 2026-11-25).
				if tc.young {
					in.SentAt = clock.Now().UTC().Format(time.RFC3339Nano)
				}
				if tc.refused {
					in.Refused = "set down"
				}
				raw, err := in.Encode()
				testutil.MustNoErr(t, err, "encode")
				_, err = r.db.DB.Exec(`UPDATE orders SET cancel_intent = ? WHERE id = ?`, raw, id)
				testutil.MustNoErr(t, err, "write the cancel")
			}

			if tc.reconcile {
				r.snapshot(protocol.OrderStatusSnapshot{OrderUUID: "resend-leg", Status: string(protocol.StatusStaged), Version: 2})
			} else {
				r.update(protocol.OrderUpdate{OrderUUID: "resend-leg", Status: string(protocol.StatusStaged), Version: 3})
			}

			envs := r.queued(protocol.TypeOrderCancel)
			if len(envs) != tc.want {
				t.Fatalf("queued %d order.cancel, want %d", len(envs), tc.want)
			}
			if tc.want == 0 {
				return
			}
			var c protocol.OrderCancel
			testutil.MustNoErr(t, envs[0].DecodePayload(&c), "decode the copy")
			if c.Reason != protocol.CancelReasonLineIdle || c.Actor != protocol.CancelActorEdge || !c.Intent {
				t.Errorf("the copy is %+v, want the line idle reason, the Edge's actor, asking", c)
			}
			in, err := domain.DecodeCancelIntent(r.row(id).CancelIntent)
			testutil.MustNoErr(t, err, "decode the cancel")
			if in == nil || in.Env != envs[0].ID || in.SentAt != "2026-11-25T12:00:00Z" {
				t.Errorf("the cancel after the copy is %+v, want its envelope the copy's %s and its clock the tap's", in, envs[0].ID)
			}
		})
	}
}
