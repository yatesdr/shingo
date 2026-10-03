//go:build docker

package engine

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/orders"
)

// WHAT A BURIED PLAIN RETRIEVE LOOKS LIKE FROM THE OUTSIDE, ONCE
// HandleOrderRequest RETURNS.
//
// These pin the observable end state of a plain retrieve whose source is buried
// in a lane: the parent's status, its compound children, the first child's
// dispatch, the queue cause on a congested lane, and the envelopes Core queues
// for the Edge that asked. They say nothing about WHICH goroutine planned the
// dig, on purpose: intake planned it until the two-holders fix moved the planning
// to the scanner (intake_buried_two_holders_docker_test.go), and the end state an
// Edge and an operator can see is what that move must keep.

// edgeEnvelopesFor lists, in order, the envelope type and any order status Core
// queued for the Edge about one order.
var edgeStatusRE = regexp.MustCompile(`"status":"([a-z_]+)"`)

func edgeEnvelopesFor(t *testing.T, db *store.DB, uuid string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT msg_type, payload FROM outbox ORDER BY id`)
	testutil.MustNoErr(t, err, "read outbox")
	defer rows.Close()
	var out []string
	for rows.Next() {
		var msgType string
		var payload []byte
		testutil.MustNoErr(t, rows.Scan(&msgType, &payload), "scan outbox")
		if !strings.Contains(string(payload), `"`+uuid+`"`) {
			continue
		}
		entry := msgType
		if m := edgeStatusRE.FindSubmatch(payload); m != nil {
			entry += ":" + string(m[1])
		}
		out = append(out, entry)
	}
	testutil.MustNoErr(t, rows.Err(), "iterate outbox")
	return out
}

func buriedRequest(uuid string, sc *testdb.CompoundScenario) *protocol.OrderRequest {
	return &protocol.OrderRequest{
		OrderUUID:    uuid,
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name,
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1,
	}
}

// A clear destination and a free shuffle slot: the dig is planned before
// HandleOrderRequest returns and its first leg is with the fleet. No order waits
// a tick: the test runs no scan of its own, and on this otherwise idle database
// the only pass that can have planned it is the one the queued event runs
// synchronously inside HandleOrderRequest (engine/wiring.go, EventOrderQueued).
//
// The Edge has been sent the order's projection and the queued reply. At
// ec11ecbd intake planned the dig itself and sent the projection alone; it now
// queues every plain order, buried or not, and says so. No envelope names the
// dig: the Edge's mirror reads queued while Core digs, as it always has for a
// burial the scanner found on replay.
func TestIntakeBuried_ClearDestination_DigPlannedBeforeReturn(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix: "IBCLR", NumSlots: 2, NumShuffles: 1, TargetSlot: 2, TargetAge: 2 * time.Hour,
	})
	eng := newTestEngine(t, db, simulator.New())

	eng.Dispatcher().HandleOrderRequest(testEnvelope(), buriedRequest("ib-clear", sc))

	parent := testdb.RequireOrder(t, db, "ib-clear")
	if parent.Status != dispatch.StatusReshuffling {
		t.Fatalf("parent status = %q, want %q", parent.Status, dispatch.StatusReshuffling)
	}
	children, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list children")
	if len(children) < 2 {
		t.Fatalf("%d compound children, want the unbury and the retrieve", len(children))
	}
	first := children[0]
	for _, c := range children[1:] {
		if c.ID < first.ID {
			first = c
		}
	}
	if first.VendorOrderID == "" || protocol.IsAcquiring(first.Status) || first.Status == dispatch.StatusPending {
		t.Errorf("first child %d not with the fleet: status %q vendor %q", first.ID, first.Status, first.VendorOrderID)
	}
	for _, c := range children {
		if c.ID != first.ID && c.VendorOrderID != "" {
			t.Errorf("child %d dispatched alongside the first — compound legs go one at a time", c.ID)
		}
	}

	got := edgeEnvelopesFor(t, db, "ib-clear")
	if want := []string{"data.order.projected:pending", "order.update:queued"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("envelopes queued for the Edge = %v, want %v", got, want)
	}
}

// THE CONGESTION CASES, end to end through HandleOrderRequest. Each is a lane
// that is crowded, not broken, so each must wait — queued, a cause naming why,
// never terminal (wait-not-fail).
func TestIntakeBuried_CongestionWaits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		congest func(t *testing.T, db *store.DB, eng *Engine, sc *testdb.CompoundScenario)
	}{
		{"lane locked by another dig", func(t *testing.T, db *store.DB, eng *Engine, sc *testdb.CompoundScenario) {
			// A dig already in the fleet's hands: dispatched, so the scanner leaves
			// it alone and its lock stands for the whole test.
			holder := testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.Status = dispatch.StatusDispatched
				o.PayloadCode = sc.Payload.Code
			})
			if !eng.Dispatcher().LaneLock().TryLock(sc.Lane.ID, holder.ID) {
				t.Fatal("fixture: could not take the lane lock")
			}
		}},
		{"no free shuffle slot", func(t *testing.T, db *store.DB, _ *Engine, sc *testdb.CompoundScenario) {
			squat := &bins.Bin{BinTypeID: sc.BinType.ID, Label: sc.ShuffleSlots[0].Name + "-SQUAT",
				NodeID: &sc.ShuffleSlots[0].ID, Status: "available"}
			testutil.MustNoErr(t, db.CreateBin(squat), "occupy the shuffle slot")
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
				Prefix: "IBCG" + string(rune('A'+i)), NumSlots: 2, NumShuffles: 1, TargetSlot: 2, TargetAge: 2 * time.Hour,
			})
			eng := newTestEngine(t, db, simulator.New())
			c.congest(t, db, eng, sc)

			eng.Dispatcher().HandleOrderRequest(testEnvelope(), buriedRequest("ib-cong", sc))

			o := testdb.RequireOrder(t, db, "ib-cong")
			t.Logf("status=%s code=%s cause=%s reason=%q edge=%v", o.Status, o.QueueCode, o.QueueCause,
				o.QueueReason, edgeEnvelopesFor(t, db, "ib-cong"))
			if protocol.IsTerminal(o.Status) {
				t.Fatalf("a congested lane TERMINALIZED the order (%q, %q)", o.Status, o.ErrorDetail)
			}
			if !protocol.IsAcquiring(o.Status) {
				t.Fatalf("status = %q, want it waiting (queued or sourcing)", o.Status)
			}
			if o.QueueCause == "" || o.QueueReason == "" {
				t.Errorf("waiting with a blank cause or reason (cause=%q reason=%q)", o.QueueCause, o.QueueReason)
			}
		})
	}
}
