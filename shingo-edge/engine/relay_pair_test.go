package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
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

// TestChangeoverApplier_ARelayPairGoesOutUnpairedAtCore is census 24, the Edge
// half: a single-robot changeover's two legs reach Core naming no sibling, and
// stay linked to each other at the Edge.
//
// DEFECT PIN. Fails at bcbde0d2: the applier pre-mints both uuids and sends each
// leg naming the other, so Core's pair rule holds the stage leg for a swap leg
// that cannot source until the stage leg has gone.
func TestChangeoverApplier_ARelayPairGoesOutUnpairedAtCore(t *testing.T) {
	t.Parallel()
	_, db, stageID, swapID := startSwapChangeover(t, protocol.SwapModeSimple)

	reqs := complexRequestsOnTheWire(t, db)
	if len(reqs) != 2 {
		t.Fatalf("want the stage leg and the swap leg on the wire, got %d request(s)", len(reqs))
	}
	for _, r := range reqs {
		if r.SiblingOrderUUID != "" {
			t.Errorf("leg %s went to Core naming sibling %s. One leg collects what the other delivers, and Core "+
				"runs siblings as one job, so as siblings the two can never go", r.OrderUUID, r.SiblingOrderUUID)
		}
	}
	stage, err := db.GetOrder(stageID)
	testutil.MustNoErr(t, err, "get the stage leg")
	swap, err := db.GetOrder(swapID)
	testutil.MustNoErr(t, err, "get the swap leg")
	if stage.SiblingOrderID == nil || *stage.SiblingOrderID != swapID ||
		swap.SiblingOrderID == nil || *swap.SiblingOrderID != stageID {
		t.Errorf("the legs are not linked at the Edge (stage → %v, swap → %v). The RELEASE button, the "+
			"supply-bin guard and the relay death rule all read that link", stage.SiblingOrderID, swap.SiblingOrderID)
	}
}

// TestRelayPartnerDeath_TakesThePartner is census 27, re-homed from Core: when
// either leg of a relay pair dies, the other is cancelled — at the Edge, and at
// Core.
//
//   - The stage leg dies: the swap leg's only source for the new carrier was the
//     one the stage leg was bringing. Left alone it waits for material that is
//     not coming, and the anomaly board is the only thing that ever notices.
//   - The swap leg dies: the stage leg stages a carrier nobody will collect.
//
// Accept-half-swap gets no exception, unlike Core's arm for a paired swap: a
// relay's collector cannot finish without its feeder's carrier, so there is no
// half to keep.
//
// DEFECT PIN. Fails at bcbde0d2: the Edge has no death rule, and Core's covered
// this pair only because the pair was sent as siblings (census 24).
func TestRelayPartnerDeath_TakesThePartner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		dead   string // "stage" or "swap"
		status protocol.Status
	}{
		{"stage leg fails", "stage", protocol.StatusFailed},
		{"stage leg skipped", "stage", protocol.StatusSkipped},
		{"stage leg cancelled", "stage", protocol.StatusCancelled},
		{"swap leg fails", "swap", protocol.StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, db, stageID, swapID := startSwapChangeover(t, protocol.SwapModeSimple)
			drainAbandonOutbox(t, db)
			dead, live := stageID, swapID
			if tc.dead == "swap" {
				dead, live = swapID, stageID
			}

			driveOrderTo(t, eng, dead, tc.status)

			got, err := db.GetOrder(live)
			testutil.MustNoErr(t, err, "reload the partner")
			if got.Status != protocol.StatusCancelled {
				t.Fatalf("the %s leg's partner is %q after it went %s, want cancelled — Core cannot see this "+
					"pair, so nothing else will end it", tc.dead, got.Status, tc.status)
			}
			if !cancelSentFor(t, db, got.UUID) {
				t.Errorf("the partner was cancelled at the Edge but no OrderCancel for %s went to Core, which "+
					"is still holding its order", got.UUID)
			}
		})
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
