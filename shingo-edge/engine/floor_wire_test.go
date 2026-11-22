package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// floor_wire_test.go: what an Edge does with a v0.19.0 Core's bodies
// (protocol.FloorCore), key by key as they are on the wire. The bodies are raw
// JSON on purpose: they hold whether or not this tree's structs still declare
// a key. Green at v0.19.0 itself, these also say that a v0.19.0 Edge does the
// same with the body of a Core that no longer sends the old keys.

// TestFloorWire_AnOwnedAckIsReadFromTheOwnedMapAlone: an ack that answers the
// owned map sends and settles the claims by it, whatever the old claims and
// bin_watch keys hold: absent (a Core that dropped them), null (a v0.19.0
// Core's ack), or a contradicting value.
func TestFloorWire_AnOwnedAckIsReadFromTheOwnedMapAlone(t *testing.T) {
	t.Parallel()
	oldKeys := map[string]string{
		"absent":        ``,
		"v0.19.0":       `,"claims":null`,
		"contradicting": `,"claims":{"GONE":"2222222222222222"},"bin_watch":"another-list"`,
	}
	for keys, extra := range oldKeys {
		for _, converged := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/converged=%t", keys, converged), func(t *testing.T) {
				t.Parallel()
				db := testEngineDB(t)
				e, _ := claimsEngine(t, db)
				seedStyledProcess(t, db, "P1", 1)
				quote := e.OwnedDigests()
				answer, want := differs(nil), []string{"P1:1"}
				if converged {
					answer, want = protocol.OwnedAnswer{Digest: quote[protocol.OwnedClaims]}, []string{}
				}
				owned, err := json.Marshal(map[string]protocol.OwnedAnswer{protocol.OwnedClaims: answer})
				testutil.MustNoErr(t, err, "encode the owned answer")
				body := `{"station_id":"edge.owned","server_ts":"2026-01-02T03:04:05Z","feeds":{},"owned":` + string(owned) + extra + `}`
				var ack protocol.EdgeHeartbeatAck
				testutil.MustNoErr(t, json.Unmarshal([]byte(body), &ack), "decode the ack")

				e.OnCoreAck(&ack)
				if got := claimsSent(t, db); !reflect.DeepEqual(got, want) {
					t.Errorf("published %v, want %v", got, want)
				}
			})
		}
	}
}

// TestFloorWire_ADeliveredWithLandingSentIsTheOrdersAlone: an order.delivered
// with landing_sent closes the order, keeps the carrier id on it and binds
// and counts nothing, whether or not it carries uop_remaining, delta_epoch and
// bin_payload_code (a v0.19.0 Core sends them; nothing at the floor needs
// them), onto an empty seat and onto one that holds the carrier already.
func TestFloorWire_ADeliveredWithLandingSentIsTheOrdersAlone(t *testing.T) {
	t.Parallel()
	carrierKeys := map[string]string{
		"absent":  ``,
		"v0.19.0": `,"uop_remaining":100,"delta_epoch":5,"bin_payload_code":"SYN-OTHER"`,
	}
	for keys, extra := range carrierKeys {
		for _, seated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/seated=%t", keys, seated), func(t *testing.T) {
				t.Parallel()
				db := testEngineDB(t)
				eng := testEngine(t, db)
				eng.wireEventHandlers()
				_, nodeID, _, _ := emptySeat(t, eng, "FLW", "PART-FLW", 100)
				const bin = int64(9201)
				wantBin, wantCount := int64(0), 0
				if seated {
					eng.HandleBinLanded(landing("FLW-NODE", bin, 4, 70, "PART-FLW", 51))
					wantBin, wantCount = bin, 70
				}
				id, err := db.CreateOrder("uuid-flw", orders.TypeRetrieve, &nodeID, false, 1, "FLW-NODE", "", "", protocol.Height{}, false, "", "", "")
				testutil.MustNoErr(t, err, "create order")
				testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(protocol.StatusInTransit)), "put the order in transit")

				body := fmt.Sprintf(`{"order_uuid":"uuid-flw","delivered_at":"2026-01-02T03:04:05Z","bin_id":%d,`+
					`"delivery_node":"FLW-NODE","landing_sent":true%s}`, bin, extra)
				var d protocol.OrderDelivered
				testutil.MustNoErr(t, json.Unmarshal([]byte(body), &d), "decode the delivered")
				testutil.MustNoErr(t, eng.orderMgr.HandleDeliveredNotice(&d), "handle the delivered")

				o, err := db.GetOrder(id)
				testutil.MustNoErr(t, err, "read the order")
				if o.Status != protocol.StatusDelivered || o.BinID == nil || *o.BinID != bin {
					t.Errorf("order = %s bin %v, want delivered with bin %d", o.Status, o.BinID, bin)
				}
				rt := runtimeOf(t, eng, nodeID)
				gotBin := int64(0)
				if rt.ActiveBinID != nil {
					gotBin = *rt.ActiveBinID
				}
				if gotBin != wantBin || rt.RemainingUOPCached != wantCount {
					t.Errorf("seat = bin %d count %d, want %d/%d: the delivered bound or counted",
						gotBin, rt.RemainingUOPCached, wantBin, wantCount)
				}
			})
		}
	}
}

// TestFloorWire_ADeliveredWithNoNoticeOfItsOwn pins the delivered event that
// has no order.delivered behind it: a version-0 reconcile snapshot
// (orders.ApplyCoreStatusSnapshot), whose event carries the row bin and
// nothing from the wire. At v0.19.0 it takes the older Core arm: no count, so
// nothing is bound and the pull-from-market auto-clear does not run.
func TestFloorWire_ADeliveredWithNoNoticeOfItsOwn(t *testing.T) {
	t.Parallel()
	f := armPullback(t)
	defer f.db.Close()
	bin := int64(77)
	f.eng.handleNodeOrderDelivered(OrderDeliveredEvent{
		OrderID: f.order.ID, OrderUUID: f.order.UUID, OrderType: f.order.OrderType,
		ProcessNodeID: &f.nodeID, BinID: &bin,
	})
	if got := f.clears.Load(); got != 0 {
		t.Errorf("bin-clear calls = %d, want 0", got)
	}
}

// TestFloorWire_AV019CoresBoundIsACountHere pins what a v0.19.0 Core's Bound
// (sent only to a station whose recorded schema is unreadable or below 20)
// does on this Edge: the key is ignored and the message is a count like any
// other. With no actor (a set-down) it binds nothing on an empty seat, keeps
// the count of the same carrier at the generation held and writes Core's at
// another; with a person's (an admin move) it binds an empty seat by the count
// and writes Core's on the same carrier; a different carrier always stands.
func TestFloorWire_AV019CoresBoundIsACountHere(t *testing.T) {
	t.Parallel()
	const bin, other = int64(9401), int64(9402)
	for _, c := range []struct {
		name, actor string
		epoch       int64
		seated      int64
		wantBin     int64
		wantCount   int
	}{
		{"set-down, empty seat", "", 4, 0, 0, 0},
		{"set-down, the same carrier", "", 4, bin, bin, 70},
		{"set-down, the same carrier, a newer generation", "", 5, bin, bin, 100},
		{"set-down, the same carrier, an older generation", "", 3, bin, bin, 100},
		{"set-down, another carrier", "", 4, other, other, 70},
		{"admin move, empty seat", protocol.AuditActorUI, 4, 0, bin, 100},
		{"admin move, the same carrier", protocol.AuditActorUI, 4, bin, bin, 100},
		{"admin move, another carrier", protocol.AuditActorUI, 4, other, other, 70},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			_, nodeID, _, _ := emptySeat(t, eng, "BND", "PART-BND", 100)
			if c.seated != 0 {
				eng.HandleBinLanded(landing("BND-NODE", c.seated, 4, 70, "PART-BND", 51))
			}
			body := fmt.Sprintf(`{"bin_id":%d,"core_node_name":"BND-NODE","new_remaining":100,"epoch":%d,`+
				`"bound":true,"actor":%q,"adjusted_at":"2026-01-02T03:04:05Z"}`, bin, c.epoch, c.actor)
			var adj protocol.UOPAdjustment
			testutil.MustNoErr(t, json.Unmarshal([]byte(body), &adj), "decode the adjustment")
			eng.HandleUOPAdjustment(adj)

			rt := runtimeOf(t, eng, nodeID)
			got := int64(0)
			if rt.ActiveBinID != nil {
				got = *rt.ActiveBinID
			}
			if got != c.wantBin || rt.RemainingUOPCached != c.wantCount {
				t.Errorf("seat = bin %d count %d, want %d/%d", got, rt.RemainingUOPCached, c.wantBin, c.wantCount)
			}
		})
	}
}
