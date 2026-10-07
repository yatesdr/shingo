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
// log (dispatcher.go:945 in getOwnedOrder; the child branch is checkOwnership
// :924-931), taken at 5c0beb74.
//
// Two halves, and only one may move:
//   - the WIRE replies: invalid_state on release (complex_release.go:31-35),
//     not_found on cancel (:958) and redirect (:1064). These must NOT change.
//   - the LOG line: today it says the station "does not own" the order and then
//     names that same station as the owner. Lane F rewrites it to say why (the
//     order is a compound child). The predicted text is in predictions/p0-bins-f.md.
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

			// The log: TODAY "station line-1 does not own order X (owner: line-1)".
			wantLog := fmt.Sprintf("station line-1 does not own order %s (owner: line-1)", child.EdgeUUID)
			mu.Lock()
			defer mu.Unlock()
			found := false
			for _, l := range lines {
				if l == wantLog {
					found = true
				}
				if strings.Contains(l, "does not own") && l != wantLog {
					t.Errorf("unexpected ownership line %q", l)
				}
			}
			if !found {
				t.Errorf("no line %q; dbg lines: %q", wantLog, lines)
			}
		})
	}
}
