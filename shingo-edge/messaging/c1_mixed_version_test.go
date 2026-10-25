package messaging

import (
	"testing"

	"shingo/protocol"
)

// c1_mixed_version_test.go — the ladder cutover across the Core/Edge version
// boundary, both ways (BRIEF C1.2; RULINGS-C1-P0 F2, F3, F4). The Edge ships
// first: for one release it may face either Core.
//
//   - NEW EDGE + OLD CORE. An old Core still says `sourcing`, on a live push and
//     on the boot snapshot. The Edge translates it at the door to `queued`, with
//     the code the push carries or else waiting_for_material (what `sourcing`
//     meant), and never stores the word (F3). RED at 647b65af, where the Edge
//     stores `sourcing`.
//
//   - OLD EDGE + NEW CORE. A new Core never says `sourcing` and never says
//     `pending`; an order that dispatches on its first hand is never told
//     `queued`; a park is one `queued` push carrying its cause; a fleet refusal
//     queues; a resumed parent is told where it landed; the skip door ends a move
//     from `pending` or `queued`. Every sequence below is what a new Core sends,
//     fed to this Edge. At 647b65af this IS the old Edge, so these are green there
//     by construction, and they must stay green on the new one.

type c1Msg struct {
	kind   string // ack | waybill | update | skipped
	status protocol.Status
	reason string
	code   protocol.QueueCode
}

// c1Feed creates an Edge order at `at` and feeds it msgs; returns the row.
func c1Feed(t *testing.T, uuid string, at protocol.Status, msgs ...c1Msg) (status protocol.Status, code string) {
	t.Helper()
	db := testHandlerDB(t)
	h := testHandler(t, db)
	id, err := db.CreateOrder(uuid, protocol.OrderTypeRetrieve, nil, false, 1, "LINE-1", "", "", false, "PART", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if at != protocol.StatusPending {
		if err := db.UpdateOrderStatus(id, string(at)); err != nil {
			t.Fatalf("seed status %s: %v", at, err)
		}
	}
	for _, m := range msgs {
		switch m.kind {
		case "ack":
			h.HandleOrderAck(nil, &protocol.OrderAck{OrderUUID: uuid, ShingoOrderID: 1, SourceNode: "SRC"})
		case "waybill":
			h.HandleOrderWaybill(nil, &protocol.OrderWaybill{OrderUUID: uuid, WaybillID: "W1"})
		case "update":
			h.HandleOrderUpdate(nil, &protocol.OrderUpdate{OrderUUID: uuid, Status: string(m.status),
				QueueReason: m.reason, QueueCode: string(m.code)})
		case "skipped":
			h.HandleOrderSkipped(nil, &protocol.OrderSkipped{OrderUUID: uuid, ErrorCode: string(protocol.TermNotNeeded),
				Detail: "the named bin left its source"})
		}
	}
	o, err := db.GetOrder(id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return o.Status, o.QueueCode
}

func TestMixedVersion_NewEdgeOldCore_SourcingArrivesAsQueued(t *testing.T) {
	t.Parallel()
	t.Run("a push with no code", func(t *testing.T) {
		st, code := c1Feed(t, "nv-push", protocol.StatusPending,
			c1Msg{kind: "update", status: protocol.StatusSourcing, reason: "Waiting for material: PART"})
		if st != protocol.StatusQueued || code != string(protocol.QueueWaitingForMaterial) {
			t.Fatalf("an old Core's sourcing push stored %s / %q, want queued / waiting_for_material", st, code)
		}
	})
	t.Run("a push carrying its own code keeps it", func(t *testing.T) {
		st, code := c1Feed(t, "nv-push-code", protocol.StatusQueued,
			c1Msg{kind: "update", status: protocol.StatusSourcing, reason: "Waiting for a slot at ALN_007",
				code: protocol.QueueWaitingForSlot})
		if st != protocol.StatusQueued || code != string(protocol.QueueWaitingForSlot) {
			t.Fatalf("stored %s / %q, want queued / waiting_for_slot", st, code)
		}
	})
	t.Run("the old ladder's walk never stores sourcing", func(t *testing.T) {
		st, _ := c1Feed(t, "nv-walk", protocol.StatusPending,
			c1Msg{kind: "update", status: protocol.StatusQueued},
			c1Msg{kind: "update", status: protocol.StatusSourcing, reason: "Waiting for material: PART"},
			c1Msg{kind: "ack"}, c1Msg{kind: "waybill"})
		if st != protocol.StatusInTransit {
			t.Fatalf("ended %s, want in_transit", st)
		}
	})
	t.Run("the boot snapshot", func(t *testing.T) {
		db := testHandlerDB(t)
		h := testHandler(t, db)
		id, err := db.CreateOrder("nv-snap", protocol.OrderTypeRetrieve, nil, false, 1, "LINE-1", "", "", false, "PART", "", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := h.orderMgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{OrderUUID: "nv-snap", Found: true,
			Status: string(protocol.StatusSourcing), QueueReason: "Waiting for material: PART"}); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		o, err := db.GetOrder(id)
		if err != nil {
			t.Fatalf("reload order: %v", err)
		}
		if o.Status != protocol.StatusQueued || o.QueueCode != string(protocol.QueueWaitingForMaterial) {
			t.Fatalf("an old Core's sourcing snapshot stored %s / %q, want queued / waiting_for_material", o.Status, o.QueueCode)
		}
	})
}

func TestMixedVersion_OldEdgeNewCore(t *testing.T) {
	t.Parallel()
	slot := c1Msg{kind: "update", status: protocol.StatusQueued, reason: "Waiting for a slot at ALN_007", code: protocol.QueueWaitingForSlot}
	cases := []struct {
		name     string
		at       protocol.Status
		msgs     []c1Msg
		want     protocol.Status
		wantCode string
	}{
		{"dispatched on its first hand: ack and waybill, no queued", protocol.StatusPending,
			[]c1Msg{{kind: "ack"}, {kind: "waybill"}}, protocol.StatusInTransit, ""},
		{"parked by its door: one queued push with its cause", protocol.StatusPending,
			[]c1Msg{slot}, protocol.StatusQueued, string(protocol.QueueWaitingForSlot)},
		{"parked, then dispatched", protocol.StatusPending,
			[]c1Msg{slot, {kind: "ack"}, {kind: "waybill"}}, protocol.StatusInTransit, ""},
		{"a fleet refusal queues under fleet_unavailable", protocol.StatusQueued,
			[]c1Msg{{kind: "update", status: protocol.StatusQueued, reason: "Robot system not responding — retrying",
				code: protocol.QueueFleetUnavailable}}, protocol.StatusQueued, string(protocol.QueueFleetUnavailable)},
		{"a resumed parent that parks again is told queued", protocol.StatusReshuffling,
			[]c1Msg{slot}, protocol.StatusQueued, string(protocol.QueueWaitingForSlot)},
		{"a resumed complex parent that dispatches is told dispatched", protocol.StatusReshuffling,
			[]c1Msg{{kind: "update", status: protocol.StatusDispatched}}, protocol.StatusDispatched, ""},
		{"a dissolved plain parent that dispatches: its ack and waybill", protocol.StatusReshuffling,
			[]c1Msg{{kind: "ack"}, {kind: "waybill"}}, protocol.StatusInTransit, ""},
		{"the skip door from pending", protocol.StatusPending,
			[]c1Msg{{kind: "skipped"}}, protocol.StatusSkipped, ""},
		{"the skip door from queued", protocol.StatusQueued,
			[]c1Msg{slot, {kind: "skipped"}}, protocol.StatusSkipped, ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			st, code := c1Feed(t, "ov-"+c.name, c.at, c.msgs...)
			if st != c.want {
				t.Fatalf("the Edge ended %s, want %s", st, c.want)
			}
			if c.wantCode != "" && code != c.wantCode {
				t.Fatalf("the Edge keeps code %q, want %q", code, c.wantCode)
			}
		})
	}
}
