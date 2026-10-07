//go:build docker

package dispatch

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
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
			_, lineNode, _ := setupTestData(t, db)
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
	setupTestData(t, db)
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
