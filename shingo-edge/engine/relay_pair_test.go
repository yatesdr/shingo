package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// relay_pair_test.go — census 24 (the Edge half) and census 27, after the ruling
// that a relay is not a pair.
//
// buildSingleRobotChangeoverSwap makes two legs: a stage leg that brings the new
// carrier to inbound staging and stops, and a swap leg that lifts the old
// carrier, collects the staged one, and sets it on the line. The swap leg's
// source for the new carrier IS the stage leg's delivery, so it cannot source
// until the stage leg has delivered. Core runs siblings as one job — both in one
// pass or neither — so sent as siblings these two legs can never go: census 24
// at bcbde0d2, both parked reserve-holding.
//
// Whether two legs are a relay is read off their steps, not their mode: one leg
// sets a carrier down at a one-bin node before its first wait, and the other
// picks up there. Such legs stay linked at the Edge and go to Core unpaired.
//
// Unpairing them at Core also takes Core's death rule off them. At origin/main
// that rule was what cancelled the swap leg when the stage leg died —
// HandleSwapPeerTerminal read neither leg as an evac, so any terminal on either
// side cancelled the other. The Edge now carries that rule for the one kind of
// pair Core cannot see.

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

// NO BUILDER MAKES A RELAY PAIR (since 2026-10-16). The single-robot
// changeover was the only one: a stage leg and a swap leg collecting what the
// stage leg parked, census 24's pair. It is one order now, which parks and
// collects its own carrier (buildSingleRobotChangeoverSwap), because a second
// order's part-naming collect took a stray bin standing on staging. With it went
// every producer of a relay, so coreSiblings and handleRelayPartnerDeath have no
// sender today. This census keeps that true, or says when it stops: a builder
// that makes a relay pair again needs the end-to-end pins this file had
// (unpaired at Core, the partner's death taken at the Edge), which are in its
// history at b07919aa.
func TestRelayPair_NoBuilderMakesOne(t *testing.T) {
	t.Parallel()
	node := &processes.Node{ID: 1, Name: "LINE", CoreNodeName: "LINE"}
	nodes := []processes.Node{*node}
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		for _, mode := range protocol.ConfigurableSwapModes() {
			for _, v := range goldenVariants(mode) {
				what := string(role) + "/" + string(mode) + "/" + v.name
				claim := goldenClaim(role, mode, "P-RUN", v.secondPaired, v.flip)
				if d, err := BuildSwapDispatch(node, claim); err == nil && legsRelay(d.StepsA, d.StepsB) {
					t.Errorf("%s: the steady swap's legs are a relay", what)
				}
				from := goldenClaim(role, mode, "P-FROM", v.secondPaired, v.flip)
				to := goldenClaim(role, mode, "P-TO", v.secondPaired, v.flip)
				for _, sit := range []ChangeoverSituation{SituationSwap, SituationEvacuate} {
					plan := BuildChangeoverPlan([]ChangeoverNodeDiff{{CoreNodeName: "LINE", Situation: sit,
						FromClaim: from, ToClaim: to}}, nodes, false, nil, toolingChangeover{})
					for _, a := range plan.Actions {
						if legsRelay(specSteps(a.SupplyOrder), specSteps(a.EvacOrder)) {
							t.Errorf("%s %s: the changeover's legs are a relay", what, sit)
						}
					}
				}
			}
		}
	}
}

// TestRelayPartnerDeath_LeavesACorePairedSwapToCore: the Edge's rule is for
// pairs Core cannot see. A two_robot pair is Core's — HandleSwapPeerTerminal
// decides it, with exceptions the Edge does not know about (a moot evac leaves
// its supply; a supply parked on material outlives its evac) — so the Edge must
// not cancel the evac when the supply fails.
//
// COVERAGE PIN. Passes at bcbde0d2, which has no Edge rule at all. MUTATION: drop
// the relay test from the Edge's death rule — the Edge then cancels the evac
// itself and this fails.
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
