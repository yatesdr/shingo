//go:build docker

package dispatch

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch/plan"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// child_ownership_pins_docker_test.go — P0 pin for lane F's child-order refusal
// log (getOwnedOrder; the child branch is checkOwnership), taken at 5c0beb74
// and moved by lane F.
//
// Two halves, and only one may move:
//   - the WIRE replies: invalid_state on release (complex_release.go:31-35),
//     not_found on cancel and redirect. These must NOT change.
//   - the LOG line: at 5c0beb74 it said the station "does not own" the order and
//     then named that same station as the owner. AFTER (lane F, child refusal
//     log): it says the order is a leg of a compound order Core runs. A
//     genuinely foreign station keeps the does-not-own line.
func TestPinDispatch_ChildOrderRefusal(t *testing.T) {
	t.Parallel()

	for _, door := range []string{"release", "cancel", "redirect"} {
		door := door
		t.Run(door, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			_, lineNode, _ := testdb.SetupTestData(t, db)
			d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())

			var mu sync.Mutex
			var lines []string
			d.DebugLog = func(format string, args ...any) {
				mu.Lock()
				lines = append(lines, fmt.Sprintf(format, args...))
				mu.Unlock()
			}

			parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.EdgeUUID, o.StationID, o.OrderType, o.Status = "pin-parent-"+door, "line-1", OrderTypeComplex, StatusInTransit
			})
			child := testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.EdgeUUID, o.StationID, o.Status = "pin-child-"+door, "line-1", StatusStaged
				o.ParentOrderID = &parent.ID
				o.DeliveryNode = lineNode.Name
			})

			env := testdb.Envelope() // line-1, the child's own station_id
			var wantCode string
			switch door {
			case "release":
				d.HandleOrderRelease(env, &protocol.OrderRelease{OrderUUID: child.EdgeUUID})
				wantCode = "invalid_state"
			case "cancel":
				d.HandleOrderCancel(env, &protocol.OrderCancel{OrderUUID: child.EdgeUUID, Reason: "pin"})
				wantCode = "not_found"
			case "redirect":
				d.HandleOrderRedirect(env, &protocol.OrderRedirect{OrderUUID: child.EdgeUUID, NewDeliveryNode: lineNode.Name})
				wantCode = "not_found"
			}

			// The wire: unchanged by lane F.
			codes := orderErrorCodes(t, db, child.EdgeUUID)
			if len(codes) != 1 || codes[0] != wantCode {
				t.Errorf("wire reply codes = %v, want [%s]", codes, wantCode)
			}
			got, err := db.GetOrder(child.ID)
			if err != nil {
				t.Fatalf("get child: %v", err)
			}
			if got.Status != StatusStaged {
				t.Errorf("child status = %q, want %q (refused, untouched)", got.Status, StatusStaged)
			}

			// The log says why: the order is a compound leg.
			wantLog := fmt.Sprintf("station line-1 cannot act on order %s: it is a leg of compound order %d, which Core runs",
				child.EdgeUUID, parent.ID)
			mu.Lock()
			defer mu.Unlock()
			found := false
			for _, l := range lines {
				if l == wantLog {
					found = true
				}
				if strings.Contains(l, "does not own") {
					t.Errorf("unexpected ownership line %q", l)
				}
			}
			if !found {
				t.Errorf("no line %q; dbg lines: %q", wantLog, lines)
			}
		})
	}
}

// TestPinDispatch_ForeignStationRefusal: an order that is not a compound leg,
// refused because another station owns it, keeps the does-not-own line and
// its not_found reply.
func TestPinDispatch_ForeignStationRefusal(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())

	var mu sync.Mutex
	var lines []string
	d.DebugLog = func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.Status = "pin-foreign", "line-2", StatusStaged
	})
	d.HandleOrderCancel(testdb.Envelope(), &protocol.OrderCancel{OrderUUID: o.EdgeUUID, Reason: "pin"})

	if codes := orderErrorCodes(t, db, o.EdgeUUID); len(codes) != 1 || codes[0] != "not_found" {
		t.Errorf("wire reply codes = %v, want [not_found]", codes)
	}
	wantLog := fmt.Sprintf("station line-1 does not own order %s (owner: line-2)", o.EdgeUUID)
	mu.Lock()
	defer mu.Unlock()
	for _, l := range lines {
		if l == wantLog {
			return
		}
	}
	t.Errorf("no line %q; dbg lines: %q", wantLog, lines)
}

// TestPinDispatch_OwnershipBySender pins checkOwnership for the five kinds of
// order whose station Core writes today, each born through its sender's door:
// an Edge's retrieve from stn-aaaa1111 and the test page's Kafka retrieve from
// core-test (both the Kafka intake, HandleOrderRequest, from an Edge-role
// envelope); the orders page's Manual Order (the same intake, from the
// Core-role envelope www apiManualOrderSubmit builds with core-operator); the
// test page's direct complex order (HandleComplexOrderRequest from the
// Edge-role envelope www dispatchComplex builds with core-direct); and a
// Core-born order with no actor (the issue the bins page's Return button makes,
// engine orderCarriedBinDown: no station, no station knowledge).
//
// Each row is which envelope senders may release, cancel, redirect or file a
// receipt against the order. Today the rule is the sender's station equal to
// the order's, so each actor owns its own orders and nothing else, and an Edge
// envelope with a BLANK station owns the Core-born order: blank equals blank.
// That last cell looks wrong (nothing should let an Edge command Core's own
// recovery) and is pinned as it is. A Core-role sender owns everything.
//
// Red when an actor stops owning its own order (the comparison reading a
// station the actor has left), when an Edge's uid owns an actor's order, or
// when the blank sender comes to own more than the Core-born order, which is
// what blanking the actors' station would do.
func TestPinDispatch_OwnershipBySender(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storage, line, bp := testdb.SetupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	envFrom := func(role, station string) *protocol.Envelope {
		return &protocol.Envelope{Src: protocol.Address{Role: role, Station: station},
			Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"}}
	}
	retrieve := func(env *protocol.Envelope, uuid, origin, desc string) {
		d.HandleOrderRequest(env, &protocol.OrderRequest{OrderUUID: uuid, OrderType: OrderTypeRetrieve,
			PayloadCode: bp.Code, PayloadDesc: desc, Quantity: 1, DeliveryNode: line.Name, OriginClass: origin})
	}
	retrieve(envFrom(protocol.RoleEdge, "stn-aaaa1111"), "own-edge", "", "")
	retrieve(envFrom(protocol.RoleCore, "core-operator"), "own-operator", protocol.OriginClassNoDemand, "")
	retrieve(envFrom(protocol.RoleEdge, "core-test"), "own-test", "", "test order from shingo core")
	d.HandleComplexOrderRequest(envFrom(protocol.RoleEdge, "core-direct"), &protocol.ComplexOrderRequest{
		OrderUUID: "own-direct", PayloadCode: bp.Code, PayloadDesc: "test complex order", Quantity: 1,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: storage.Name},
			{Action: protocol.ActionDropoff, Node: line.Name},
		},
	})
	_, _, err := d.IssueOrder(Need{Type: OrderTypeMove, Ref: "own-core", Quantity: 1,
		OriginClass: protocol.OriginClassNoDemand, Source: storage.Name, Delivery: line.Name}, plan.Plan{})
	testutil.MustNoErr(t, err, "issue the Core-born order")

	senders := []struct {
		name string
		env  *protocol.Envelope
	}{
		{"edge stn-aaaa1111", envFrom(protocol.RoleEdge, "stn-aaaa1111")},
		{"edge core-operator", envFrom(protocol.RoleEdge, "core-operator")},
		{"edge core-direct", envFrom(protocol.RoleEdge, "core-direct")},
		{"edge core-test", envFrom(protocol.RoleEdge, "core-test")},
		{"edge blank", envFrom(protocol.RoleEdge, "")},
		{"core", envFrom(protocol.RoleCore, "core-operator")},
	}
	want := map[string][]string{
		"own-edge":     {"edge stn-aaaa1111", "core"},
		"own-operator": {"edge core-operator", "core"},
		"own-direct":   {"edge core-direct", "core"},
		"own-test":     {"edge core-test", "core"},
		"own-core":     {"edge blank", "core"},
	}
	for _, uuid := range []string{"own-edge", "own-operator", "own-direct", "own-test", "own-core"} {
		o, err := db.GetOrderByUUID(uuid)
		testutil.MustNoErr(t, err, "read "+uuid+" (its door made no order)")
		owners := []string{}
		for _, s := range senders {
			if d.checkOwnership(s.env, o) {
				owners = append(owners, s.name)
			}
		}
		if !reflect.DeepEqual(owners, want[uuid]) {
			t.Errorf("%s is owned by %q, want %q", uuid, owners, want[uuid])
		}
	}
}
