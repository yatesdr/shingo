//go:build docker

package engine

import (
	"encoding/json"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/store"
)

// release_layer_order112_docker_test.go — the order-112 class (SYNTH-round3 §2):
// every station wait, with a node and bare, produces `staged` at Core and sends
// OrderStaged to the Edge.
//
// Plant run 2026-08-30, order 112: Core never wrote a second `staged` after the
// first release. The survivor arm was its only rescuer, and S5 deletes that arm,
// so this pin has to hold first.
//
// The fleet side is the simulator driven by hand, which emits a status change
// only when the vendor state CHANGES — the same rule the RDS poller applies
// (rds/poller.go, `if newState == oldState { continue }`).

// pinOutcome asserts one characterised outcome. While bug names a known defect,
// the cell holds today's outcome and says what it should be; the fix commit
// deletes the tag, and from then on the cell asserts want.
func pinOutcome(t *testing.T, bug, got, today, want string) {
	t.Helper()
	if bug != "" {
		if got != today {
			t.Fatalf("bug:%s characterisation moved: got %q, today was %q (want after the fix: %q)", bug, got, today, want)
		}
		t.Logf("bug:%s RED as expected: got %q, want %q", bug, got, want)
		return
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// stagedEvidence reads back the two halves of "the order staged": the rows
// Core wrote (order_history entries in `staged`) and the OrderStaged envelopes
// it queued for the Edge.
func stagedEvidence(t *testing.T, db *store.DB, orderID int64, uuid string) (rows, envelopes int) {
	t.Helper()
	hist, err := db.ListOrderHistory(orderID)
	testutil.MustNoErr(t, err, "order history")
	for _, h := range hist {
		if h.Status == dispatch.StatusStaged {
			rows++
		}
	}
	msgs, err := db.ListPendingOutbox(500)
	testutil.MustNoErr(t, err, "outbox")
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderStaged {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "envelope")
		var p protocol.OrderStaged
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &p), "order.staged")
		if p.OrderUUID == uuid {
			envelopes++
		}
	}
	return rows, envelopes
}

func TestReleaseLayer_Order112_EveryStationWaitStages(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		firstNode bool // the first wait carries a node; the second is bare (else the reverse)
		// missedRunning: after the first release the robot finishes its segment
		// between two polls, so the fleet reads WAITING both times.
		missedRunning bool
		bug           string
		today, want   string
	}{
		{name: "wait with a node, then a bare wait", firstNode: true,
			want: "stage1=1/1 stage2=2/2 status=staged blocks=5"},
		{name: "bare wait, then a wait with a node", firstNode: false,
			want: "stage1=1/1 stage2=2/2 status=staged blocks=5"},
		// The fleet never reports the second WAITING as a change, because the
		// RUNNING between them was never observed. Nothing tells Core the robot
		// is parked again: the row stays in_transit and the Edge gets no
		// OrderStaged, so the board never offers the second release.
		{name: "RUNNING between the waits not observed", firstNode: true, missedRunning: true, bug: "order-112",
			today: "stage1=1/1 stage2=1/1 status=in_transit blocks=5",
			want:  "stage1=1/1 stage2=2/2 status=staged blocks=5"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			storageNode, lineNode, bp := setupTestData(t, db)
			createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-112-S1")
			createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-112-S2")
			createTestBinAtNode(t, db, bp.Code, lineNode.ID, "BIN-112-L1")

			sim := simulator.New()
			eng := newTestEngine(t, db, sim)
			d := eng.Dispatcher()

			first, second := lineNode.Name, ""
			if !c.firstNode {
				first, second = "", storageNode.Name
			}
			uuid := fmt.Sprintf("o112-%v-%v", c.firstNode, c.missedRunning)
			d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
				OrderUUID: uuid, PayloadCode: bp.Code, Quantity: 1,
				Steps: []protocol.ComplexOrderStep{
					{Action: protocol.ActionPickup, Node: storageNode.Name},
					{Action: protocol.ActionDropoff, Node: lineNode.Name},
					{Action: protocol.ActionWait, Node: first, WaitKind: dispatch.WaitKindStation},
					{Action: protocol.ActionPickup, Node: lineNode.Name},
					{Action: protocol.ActionDropoff, Node: storageNode.Name},
					{Action: protocol.ActionWait, Node: second, WaitKind: dispatch.WaitKindStation},
					{Action: protocol.ActionPickup, Node: storageNode.Name},
					{Action: protocol.ActionDropoff, Node: lineNode.Name},
				},
			})
			order, err := db.GetOrderByUUID(uuid)
			testutil.MustNoErr(t, err, "load order")
			if order == nil || order.VendorOrderID == "" {
				t.Fatalf("order %s was not dispatched", uuid)
			}
			vid := order.VendorOrderID

			sim.DriveState(vid, "RUNNING")
			sim.DriveState(vid, "WAITING")
			r1, e1 := stagedEvidence(t, db, order.ID, uuid)

			d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{OrderUUID: uuid})
			if !c.missedRunning {
				sim.DriveState(vid, "RUNNING")
			}
			sim.DriveState(vid, "WAITING")
			r2, e2 := stagedEvidence(t, db, order.ID, uuid)

			fresh, err := db.GetOrder(order.ID)
			testutil.MustNoErr(t, err, "reload")
			got := fmt.Sprintf("stage1=%d/%d stage2=%d/%d status=%s blocks=%d",
				r1, e1, r2, e2, fresh.Status, len(sim.GetOrder(vid).Blocks))
			pinOutcome(t, c.bug, got, c.today, c.want)
		})
	}
}
