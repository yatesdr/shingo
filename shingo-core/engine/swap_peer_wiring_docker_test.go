//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// swap_peer_wiring_docker_test.go — census 7: the death rule's TRIGGER.
//
// dispatch/swap_peer_test.go pins what HandleSwapPeerTerminal decides once it is
// called, by calling it. What makes it run in a plant is three subscribers in
// wiring.go — order failed, order skipped, order cancelled — each ending in a
// HandleSwapPeerTerminal call. None of them had a test: delete any one and every
// dispatch test still passes while a dead leg's partner flies on.
//
// So these emit the real event on the real bus of a started engine, with the dead
// leg's row already terminal the way the lifecycle leaves it, and read the
// partner back.
//
// COVERAGE PINS. Pass at bcbde0d2. MUTATION: delete the HandleSwapPeerTerminal
// call from any one of the three subscribers — its case fails naming the event.

// swapLegsJSON renders the two_robot legs the peer handler classifies by: the
// supply sets a carrier on the line, the evac lifts the line's and takes it away.
func swapLegsJSON(line, src, out string) (supply, evac string) {
	supply = `[{"action":"pickup","node":"` + src + `"},{"action":"dropoff","node":"` + line + `"}]`
	evac = `[{"action":"wait","node":"` + line + `"},{"action":"pickup","node":"` + line + `"},` +
		`{"action":"dropoff","node":"` + out + `"}]`
	return supply, evac
}

func TestSwapDeathRule_ARealTerminalEventReachesThePartner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		prefix     string
		dead       string // "supply" or "evac"
		deadStatus protocol.Status
		event      EventType
	}{
		{"supply failed", "SDW1", "supply", dispatch.StatusFailed, EventOrderFailed},
		{"supply skipped", "SDW2", "supply", dispatch.StatusSkipped, EventOrderSkipped},
		{"evac cancelled", "SDW3", "evac", dispatch.StatusCancelled, EventOrderCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			sd := testdb.SetupStandardData(t, db)
			eng := newTestEngine(t, db, simulator.New())
			out := &nodes.Node{Name: tc.prefix + "-OUT", Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(out), "create out node")
			supplySteps, evacSteps := swapLegsJSON(sd.LineNode.Name, sd.StorageNode.Name, out.Name)

			mk := func(uuid, sib, delivery, steps string) *orders.Order {
				o := &orders.Order{
					EdgeUUID: uuid, StationID: "line-1", OrderType: dispatch.OrderTypeComplex,
					Status: protocol.StatusInTransit, Quantity: 1, PayloadCode: sd.Payload.Code,
					ProcessNode: sd.LineNode.Name, DeliveryNode: delivery, SiblingOrderUUID: sib,
					Coordinated: true, StepsJSON: steps,
				}
				testutil.MustNoErr(t, db.CreateOrder(o), "create leg "+uuid)
				return o
			}
			supply := mk(tc.prefix+"-supply", tc.prefix+"-evac", sd.LineNode.Name, supplySteps)
			evac := mk(tc.prefix+"-evac", tc.prefix+"-supply", out.Name, evacSteps)
			dead, live := supply, evac
			if tc.dead == "evac" {
				dead, live = evac, supply
			}
			testdb.SeedOrderStatus(t, db, dead.ID, string(tc.deadStatus), "died in the test")

			switch tc.event {
			case EventOrderFailed:
				eng.Events.Emit(Event{Type: EventOrderFailed, Payload: OrderFailedEvent{
					OrderID: dead.ID, EdgeUUID: dead.EdgeUUID, StationID: dead.StationID,
					ErrorCode: "fleet_failed", Detail: "robot faulted and was aborted"}})
			case EventOrderSkipped:
				eng.Events.Emit(Event{Type: EventOrderSkipped, Payload: OrderSkippedEvent{
					OrderID: dead.ID, EdgeUUID: dead.EdgeUUID, StationID: dead.StationID,
					ErrorCode: "no_source_bin", Detail: "no bin at any source node"}})
			case EventOrderCancelled:
				eng.Events.Emit(Event{Type: EventOrderCancelled, Payload: OrderCancelledEvent{
					OrderID: dead.ID, EdgeUUID: dead.EdgeUUID, StationID: dead.StationID,
					Reason: "operator cancelled", PreviousStatus: string(protocol.StatusInTransit)}})
			}

			got, err := db.GetOrder(live.ID)
			testutil.MustNoErr(t, err, "reload the partner")
			if got.Status != dispatch.StatusCancelled {
				t.Fatalf("%s is %q after %s went %s through the engine's event, want cancelled. The handler "+
					"is pinned; what makes it run in a plant is this subscriber, and it did not unwind the pair",
					live.EdgeUUID, got.Status, dead.EdgeUUID, tc.deadStatus)
			}
		})
	}
}
