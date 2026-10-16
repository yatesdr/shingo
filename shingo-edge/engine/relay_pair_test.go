package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// relay_pair_test.go — a relay is not a pair, and no builder makes one.
//
// A relay is two legs where one sets a carrier down at a one-bin node before its
// first wait and the other picks up there. Core runs siblings as one job, so a
// relay sent as siblings could never go (census 24, bcbde0d2), and the Edge
// carried such pairs unpaired with a partner-death rule of its own (census 27).
// The only builder that made one, the single-robot changeover, is one order
// since 2026-10-16, and that Edge code is deleted. What stays: the census that no
// builder makes the shape, and the pin that a real pair's unwinding is Core's.

// startSwapChangeover starts a changeover on the phase-3 fixture in the given
// mode and returns the node task's two legs.
func startSwapChangeover(t *testing.T, mode protocol.SwapMode) (eng *Engine, db *store.DB, supplyID, evacID int64) {
	t.Helper()
	db = testEngineDB(t)
	processID, _, _, toStyleID := seedPhase3SwapScenarioMode(t, db, mode)
	eng = testEngine(t, db)
	eng.wireEventHandlers()
	drainAbandonOutbox(t, db)
	_, task := startChangeover(t, eng, db, processID, toStyleID)
	if task.NextMaterialOrderID == nil || task.OldMaterialReleaseOrderID == nil {
		t.Fatalf("fixture: the %s changeover did not create two legs (supply %v, evac %v)",
			mode, task.NextMaterialOrderID, task.OldMaterialReleaseOrderID)
	}
	return eng, db, *task.NextMaterialOrderID, *task.OldMaterialReleaseOrderID
}

// driveOrderTo sets an order's status and publishes the change on the bus, the
// way driveToInTransit does.
func driveOrderTo(t *testing.T, eng *Engine, orderID int64, to protocol.Status) {
	t.Helper()
	o, err := eng.db.GetOrder(orderID)
	testutil.MustNoErr(t, err, "get order")
	testutil.MustNoErr(t, eng.db.UpdateOrderStatus(orderID, string(to)), "set "+string(to))
	eng.Events.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID: orderID, OrderUUID: o.UUID, OrderType: o.OrderType,
		OldStatus: string(o.Status), NewStatus: string(to), ProcessNodeID: o.ProcessNodeID,
	}})
}

// cancelSentFor reports whether an OrderCancel for uuid is waiting in the outbox.
func cancelSentFor(t *testing.T, db *store.DB, uuid string) bool {
	t.Helper()
	for _, c := range decodeCancel(t, db) {
		if c.OrderUUID == uuid {
			return true
		}
	}
	return false
}

// NO BUILDER MAKES A RELAY PAIR (since 2026-10-16). A relay is two legs where
// one sets a carrier down at a one-bin node before its first wait and the other
// picks up there: the single-robot changeover's stage leg and swap leg were the
// only one. That changeover is one order now, which parks and collects its own
// carrier (buildSingleRobotChangeoverSwap), and the Edge code that carried a
// relay — sent to Core unpaired, its partner's death taken at the Edge — went
// with it. This census keeps the shape unreached; a builder that makes it again
// needs that code back, and its pins, from b07919aa's history.
func TestRelayPair_NoBuilderMakesOne(t *testing.T) {
	t.Parallel()
	node := &processes.Node{ID: 1, Name: "LINE", CoreNodeName: "LINE"}
	nodes := []processes.Node{*node}
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		for _, mode := range protocol.ConfigurableSwapModes() {
			for _, v := range goldenVariants(mode) {
				what := string(role) + "/" + string(mode) + "/" + v.name
				claim := goldenClaim(role, mode, "P-RUN", v.secondPaired, v.flip)
				if d, err := BuildSwapDispatch(node, claim); err == nil && isRelayPair(d.StepsA, d.StepsB) {
					t.Errorf("%s: the steady swap's legs are a relay", what)
				}
				from := goldenClaim(role, mode, "P-FROM", v.secondPaired, v.flip)
				to := goldenClaim(role, mode, "P-TO", v.secondPaired, v.flip)
				for _, sit := range []ChangeoverSituation{SituationSwap, SituationEvacuate} {
					plan := BuildChangeoverPlan([]ChangeoverNodeDiff{{CoreNodeName: "LINE", Situation: sit,
						FromClaim: from, ToClaim: to}}, nodes, false, nil, toolingChangeover{})
					for _, a := range plan.Actions {
						if isRelayPair(specSteps(a.SupplyOrder), specSteps(a.EvacOrder)) {
							t.Errorf("%s %s: the changeover's legs are a relay", what, sit)
						}
					}
				}
			}
		}
	}
}

// TestRelayPartnerDeath_LeavesACorePairedSwapToCore: a two_robot pair is
// Core's — HandleSwapPeerTerminal decides it, with exceptions the Edge does not
// know about (a moot evac leaves its supply; a supply parked on material
// outlives its evac) — so the Edge must not cancel the evac when the supply
// fails. It pinned the Edge's relay death rule not reaching a real pair while
// that rule existed; with the rule deleted (2026-10-16) it pins that nothing
// on the Edge takes its place.
func TestRelayPartnerDeath_LeavesACorePairedSwapToCore(t *testing.T) {
	t.Parallel()
	eng, db, supplyID, evacID := startSwapChangeover(t, protocol.SwapModeTwoRobot)
	drainAbandonOutbox(t, db)

	driveOrderTo(t, eng, supplyID, protocol.StatusFailed)

	evac, err := db.GetOrder(evacID)
	testutil.MustNoErr(t, err, "reload the evac")
	if evac.Status == protocol.StatusCancelled || cancelSentFor(t, db, evac.UUID) {
		t.Errorf("the Edge cancelled a two_robot evac (status %q) when its supply failed. That pair is Core's "+
			"to unwind, and Core's rule spares evacs the Edge cannot tell apart", evac.Status)
	}
}

// isRelayPair reports whether two legs are a relay: either one sets a carrier
// down at a one-bin node before its first wait and the other picks up there.
func isRelayPair(a, b []protocol.ComplexOrderStep) bool {
	return handsOff(a, b) || handsOff(b, a)
}

func handsOff(feeder, collector []protocol.ComplexOrderStep) bool {
	handOff := map[string]bool{}
	for _, s := range feeder {
		if s.Action == protocol.ActionWait {
			break
		}
		if s.Action == protocol.ActionDropoff && s.ExclusiveSlot && s.Node != "" {
			handOff[s.Node] = true
		}
	}
	for _, s := range collector {
		if s.Action == protocol.ActionPickup && handOff[s.Node] {
			return true
		}
	}
	return false
}
