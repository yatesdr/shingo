package engine

// order_follow_ons_test.go — the follow-ons of an order whose status is Core's
// versioned word hang on the messages that carry their causes
// (order_follow_ons.go): Core's order.delivered makes a held sequential
// pulling leg, and Core's order.cancelled sends back the carrier a leg
// cancelled at its staging wait left there. The status event declines such an
// order, so a status learned late, twice or alone makes nothing. An order that
// ends wakes its sibling's release intents from EventOrderCompleted, whoever
// ended it.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/messaging"
	"shingoedge/orders"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
)

// rewireOrdersOntoTheBus gives an engine that wireEventHandlers already wired
// an order manager that emits onto its bus, with the message hooks
// wireEventHandlers sets, so Core's word reaches the status-event subscribers
// as it does on a plant. The sequential fixture wires its engine over a
// manager that emits nowhere, and wiring it twice would subscribe twice.
func rewireOrdersOntoTheBus(eng *Engine) {
	eng.orderMgr = orders.NewManager(eng.db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.orderMgr.OnStagedMessage = eng.onStagedMessage
	eng.orderMgr.OnDeliveredMessage = eng.onDeliveredMessage
	eng.orderMgr.OnCancelledMessage = eng.onCancelledMessage
}

// busTrail is what the bus carried for the named orders: each status event as
// "<name> old->new", each EventOrderCompleted as "<name> completed", and each
// board nudge that names the order as "<name> touched". Locked: a carrier sent
// back runs off the test's goroutine and emits as it creates its move.
type busTrail struct {
	mu  sync.Mutex
	got []string
}

func trailOf(eng *Engine, names map[int64]string) *busTrail {
	tr := &busTrail{}
	eng.Events.Subscribe(func(evt Event) {
		var line string
		switch p := evt.Payload.(type) {
		case OrderStatusChangedEvent:
			if n, ok := names[p.OrderID]; ok {
				line = n + " " + p.OldStatus + "->" + p.NewStatus
			}
		case OrderCompletedEvent:
			if n, ok := names[p.OrderID]; ok {
				line = n + " completed"
			}
		case BoardNudgeEvent:
			if n, ok := names[p.OrderID]; ok {
				line = n + " touched"
			}
		}
		if line != "" {
			tr.mu.Lock()
			tr.got = append(tr.got, line)
			tr.mu.Unlock()
		}
	})
	return tr
}

// expect fails unless the events since the last expect are want.
func (tr *busTrail) expect(t *testing.T, step string, want ...string) {
	t.Helper()
	tr.mu.Lock()
	got := tr.got
	tr.got = nil
	tr.mu.Unlock()
	if !slices.Equal(got, want) {
		t.Errorf("%s: the bus carried %q, want %q", step, got, want)
	}
}

// coreWord is Core's versioned order.update through the wire handler.
func coreWord(h *messaging.EdgeHandler, uuid string, status protocol.Status, version int64) {
	h.HandleOrderUpdate(wire, &protocol.OrderUpdate{OrderUUID: uuid, Status: string(status), Version: version})
}

func orderRow(t *testing.T, db *store.DB, id int64) *storeorders.Order {
	t.Helper()
	o, err := db.GetOrder(id)
	testutil.MustNoErr(t, err, "read the order")
	return o
}

// orderHistory is an order's history rows as "old->new: detail", in write order.
func orderHistory(t *testing.T, db *store.DB, id int64) []string {
	t.Helper()
	hs, err := db.ListOrderHistory(id)
	testutil.MustNoErr(t, err, "read the history")
	slices.SortFunc(hs, func(a, b storeorders.History) int { return int(a.ID - b.ID) })
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, string(h.OldStatus)+"->"+string(h.NewStatus)+": "+h.Detail)
	}
	return out
}

// TestFollowOns_ThePartnersOrderDeliveredMakesTheHeldLegOnce: on a sequential
// changeover the pulling side's leg waits for its partner's delivery. With a
// Core that versions its orders, Core's order.delivered for the partner makes
// it, once, in whichever order the message and Core's delivered status land:
// the status alone makes none (its event reaches the subscriber, which
// declines a versioned order), and the status after the message, or the
// message again, makes no second. The partner confirms itself once both halves
// are in (the test engine auto-confirms), and that confirmed event is declined
// too. An older Core's order.delivered (version 0) makes it by the status
// event, as it always did.
func TestFollowOns_ThePartnersOrderDeliveredMakesTheHeldLegOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		version0      bool
		envelopeFirst bool
	}{
		{name: "order.delivered, then Core's delivered", envelopeFirst: true},
		{name: "Core's delivered, then order.delivered"},
		{name: "an older Core's order.delivered", version0: true, envelopeFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng, _, pullingNodeID, partnerNodeID, co := seedSequentialScenarioRaw(t, db, false)
			rewireOrdersOntoTheBus(eng)
			h := newEdgeHandlerFor(eng)
			partnerID, _ := seqTaskOf(t, db, co.ID, partnerNodeID)
			if partnerID == nil {
				t.Fatal("the partner has no leg at the start")
			}
			partner := orderRow(t, db, *partnerID)
			trail := trailOf(eng, map[int64]string{partner.ID: "partner"})
			heldLeg := func(step string) *int64 {
				t.Helper()
				next, old := seqTaskOf(t, db, co.ID, pullingNodeID)
				if old != nil {
					t.Errorf("%s: the pulling side has a removal leg %d, want none", step, *old)
				}
				return next
			}
			noHeldLeg := func(step string) {
				t.Helper()
				if next := heldLeg(step); next != nil {
					t.Errorf("%s: the pulling side has leg %d, want none yet", step, *next)
				}
				if got := ordersDroppingAt(t, db, "SEQ-A"); len(got) != 0 {
					t.Errorf("%s: orders bringing SEQ-A a carrier = %v, want none", step, got)
				}
			}
			oneHeldLeg := func(step string, want *int64) int64 {
				t.Helper()
				next := heldLeg(step)
				if next == nil {
					t.Fatalf("%s: the pulling side has no leg", step)
				}
				if want != nil && *next != *want {
					t.Errorf("%s: the pulling side's leg is %d, want %d still", step, *next, *want)
				}
				if got := ordersDroppingAt(t, db, "SEQ-A"); len(got) != 1 || got[0] != *next {
					t.Errorf("%s: orders bringing SEQ-A a carrier = %v, want exactly its leg %d", step, got, *next)
				}
				return *next
			}
			envelope := func() {
				h.HandleOrderDelivered(wire, &protocol.OrderDelivered{OrderUUID: partner.UUID,
					DeliveredAt: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)})
			}

			if tc.version0 {
				testutil.MustNoErr(t, db.UpdateOrderStatus(partner.ID, string(protocol.StatusInTransit)), "the partner drives")
				envelope()
				if o := orderRow(t, db, partner.ID); o.Status != protocol.StatusConfirmed || o.CoreVersion != 0 {
					t.Errorf("an older Core's order.delivered left the partner %s at version %d, want confirmed at 0",
						o.Status, o.CoreVersion)
				}
				trail.expect(t, "an older Core's order.delivered", "partner in_transit->delivered", "partner delivered->confirmed", "partner completed")
				leg := oneHeldLeg("an older Core's order.delivered", nil)
				envelope()
				oneHeldLeg("the message again", &leg)
				return
			}

			coreWord(h, partner.UUID, protocol.StatusInTransit, 2)
			trail.expect(t, "in_transit v2", "partner "+string(partner.Status)+"->in_transit")
			noHeldLeg("in_transit v2")

			var leg int64
			if tc.envelopeFirst {
				envelope()
				// The message makes the leg; it writes no status.
				if o := orderRow(t, db, partner.ID); o.Status != protocol.StatusInTransit || o.CoreVersion != 2 {
					t.Errorf("order.delivered moved the partner to %s at version %d, want in_transit at 2", o.Status, o.CoreVersion)
				}
				trail.expect(t, "order.delivered")
				leg = oneHeldLeg("order.delivered", nil)
				coreWord(h, partner.UUID, protocol.StatusDelivered, 3)
				trail.expect(t, "delivered v3", "partner in_transit->delivered", "partner delivered->confirmed", "partner completed")
				oneHeldLeg("delivered v3", &leg)
			} else {
				coreWord(h, partner.UUID, protocol.StatusDelivered, 3)
				// The status event reached createHeldPullingLeg, which declines a
				// versioned order: a status alone makes nothing.
				trail.expect(t, "delivered v3", "partner in_transit->delivered")
				noHeldLeg("delivered v3, no order.delivered yet")
				envelope()
				// The facts complete the delivery: the partner confirms itself,
				// then the message makes the leg.
				trail.expect(t, "order.delivered", "partner delivered->confirmed", "partner completed")
				leg = oneHeldLeg("order.delivered", nil)
			}

			envelope()
			coreWord(h, partner.UUID, protocol.StatusDelivered, 3)
			coreWord(h, partner.UUID, protocol.StatusDelivered, 4)
			trail.expect(t, "both halves again, and a newer delivered")
			oneHeldLeg("both halves again, and a newer delivered", &leg)
			// The confirmed row holds Core's newer delivered and notes its version.
			if o := orderRow(t, db, partner.ID); o.Status != protocol.StatusConfirmed || o.CoreVersion != 4 {
				t.Errorf("the partner ends %s at version %d, want confirmed at 4", o.Status, o.CoreVersion)
			}
			expectRows(t, "the partner's history", orderHistory(t, db, partner.ID), []string{
				"pending->submitted: auto-submitted at creation",
				"submitted->in_transit: core: in_transit (version 2)",
				"in_transit->delivered: core: delivered (version 3)",
				"delivered->confirmed: confirmed with count 1",
			})
		})
	}
}

// seedVersionedSupplyAtItsStagingWait is a two-robot supply at the changeover
// scenario's line that set its carrier down on STG-1 and stands at its station
// wait 0 there, at Core's word: Core's staged at version 3, then order.staged
// with the wait. The carrier on STG-1 is in the bins table, and Core's node
// reads go nowhere (the cancel's own read is the bins table).
func seedVersionedSupplyAtItsStagingWait(t *testing.T, eng *Engine, h *messaging.EdgeHandler, nodeID int64, uuid string, version int64) int64 {
	t.Helper()
	id, err := eng.db.CreateOrder(uuid, orders.TypeComplex, &nodeID, false, 1,
		"CO-NODE", "", "SRC-LOADER", protocol.Height{}, false, "PART-NEW", "", "")
	testutil.MustNoErr(t, err, "create the supply")
	raw, err := json.Marshal(twoRobotSupplySteps("SRC-LOADER", "STG-1", "CO-NODE"))
	testutil.MustNoErr(t, err, "encode its steps")
	testutil.MustNoErr(t, eng.db.UpdateOrderStepsAndFacts(id, string(raw), ""), "store its steps")
	testutil.MustNoErr(t, eng.db.UpdateOrderStatus(id, string(protocol.StatusInTransit)), "it drives")
	if version > 0 {
		coreWord(h, uuid, protocol.StatusStaged, version)
	} else {
		h.HandleOrderStaged(wire, &protocol.OrderStaged{OrderUUID: uuid, Detail: "robot at the staging wait"})
	}
	first := 0
	h.HandleOrderStaged(wire, &protocol.OrderStaged{OrderUUID: uuid, Detail: "robot at the staging wait",
		StationWait: &first, WaitKind: protocol.WaitKindStation})
	o := orderRow(t, eng.db, id)
	if o.Status != protocol.StatusStaged || o.StationWait == nil || *o.StationWait != 0 || o.CoreVersion != version {
		t.Fatalf("fixture: the supply reads %s at wait %v, version %d; want staged at its wait 0, version %d",
			o.Status, o.StationWait, o.CoreVersion, version)
	}
	coreNeverAnswers(t, eng)
	holdActableBins(t, eng, protocol.NodeBinsRow{Node: "STG-1", Occupied: true, PayloadCode: "PART-NEW"})
	return id
}

// movesOffStaging is the live move orders that take a carrier off STG-1.
func movesOffStaging(t *testing.T, db *store.DB) []storeorders.Order {
	t.Helper()
	live, err := db.ListActiveOrders()
	testutil.MustNoErr(t, err, "list the live orders")
	var out []storeorders.Order
	for _, o := range live {
		if o.OrderType == orders.TypeMove && o.SourceNode == "STG-1" {
			out = append(out, o)
		}
	}
	return out
}

// expectOneCarrierBack waits for the one move the cancel sends off its
// goroutine, checks where it goes, then waits on to see that no second one
// comes from any earlier step's goroutine.
func expectOneCarrierBack(t *testing.T, db *store.DB, nodeID int64) {
	t.Helper()
	eventually(t, "a move takes the carrier off STG-1", func() bool { return len(movesOffStaging(t, db)) > 0 })
	time.Sleep(200 * time.Millisecond)
	moves := movesOffStaging(t, db)
	if len(moves) != 1 {
		t.Fatalf("moves off STG-1 = %d, want 1: one cancel sends one carrier back", len(moves))
	}
	if m := moves[0]; m.DeliveryNode != "SRC-LOADER" || m.PayloadCode != "PART-NEW" ||
		m.ProcessNodeID == nil || *m.ProcessNodeID != nodeID {
		t.Errorf("the move goes %s->%s carrying %q at node %v, want ->SRC-LOADER carrying PART-NEW at node %d",
			m.SourceNode, m.DeliveryNode, m.PayloadCode, m.ProcessNodeID, nodeID)
	}
}

// expectNoCarrierBack is the negative: the step's decision is synchronous, and
// a carrier sent back would be a move off STG-1 within the wait.
func expectNoCarrierBack(t *testing.T, db *store.DB, step string) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	if moves := movesOffStaging(t, db); len(moves) != 0 {
		t.Errorf("%s: %d move(s) off STG-1, want none", step, len(moves))
	}
}

// TestFollowOns_CoresCancelledNoticeSendsTheWaitingCarrierBackOnce: a two-robot
// supply cancelled at its station wait after its drop at staging left its
// carrier there. With a Core that versions its orders the carrier goes back
// on Core's order.cancelled that says the order was staged, once: not on the
// status that ended the row (Core's cancelled tell, or the operator's own
// abort, whose event declines a versioned order), and not on the cancel
// reply to the station's own request, which carries no previous status. An
// older Core's cancel (version 0) sends it back from the status event.
func TestFollowOns_CoresCancelledNoticeSendsTheWaitingCarrierBackOnce(t *testing.T) {
	t.Parallel()
	const uuid = "supply-at-staging"
	fixture := func(t *testing.T, version int64) (*Engine, *messaging.EdgeHandler, int64, int64) {
		t.Helper()
		db := testEngineDB(t)
		_, nodeID, _, _, _, _ := seedChangeoverScenario(t, db)
		eng := testEngine(t, db)
		eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
		eng.wireEventHandlers()
		h := newEdgeHandlerFor(eng)
		return eng, h, nodeID, seedVersionedSupplyAtItsStagingWait(t, eng, h, nodeID, uuid, version)
	}
	notice := func(h *messaging.EdgeHandler, previous protocol.Status) {
		h.HandleOrderCancelled(wire, &protocol.OrderCancelled{OrderUUID: uuid, Reason: "terminated at Core",
			PreviousStatus: string(previous)})
	}
	stagedToCancelled := func(id, nodeID int64) OrderStatusChangedEvent {
		return OrderStatusChangedEvent{OrderID: id, OldStatus: string(protocol.StatusStaged),
			NewStatus: string(protocol.StatusCancelled), ProcessNodeID: &nodeID}
	}

	t.Run("Core cancels it", func(t *testing.T) {
		t.Parallel()
		eng, h, nodeID, id := fixture(t, 3)
		trail := trailOf(eng, map[int64]string{id: "supply"})

		coreWord(h, uuid, protocol.StatusCancelled, 4)
		trail.expect(t, "cancelled v4", "supply staged->cancelled", "supply completed")
		if o := orderRow(t, eng.db, id); o.Status != protocol.StatusCancelled || o.CoreVersion != 4 {
			t.Errorf("after Core's cancelled the supply reads %s at %d, want cancelled at 4", o.Status, o.CoreVersion)
		}
		// The status event's own decision, read directly: it declines.
		if p, ok := eng.carrierLeftWaiting(stagedToCancelled(id, nodeID)); ok {
			t.Errorf("the status event answered a versioned order's carrier (%+v); want it declined", p)
		}
		expectNoCarrierBack(t, eng.db, "Core's cancelled tell")

		notice(h, protocol.StatusStaged)
		trail.expect(t, "order.cancelled from staged")
		expectOneCarrierBack(t, eng.db, nodeID)
		if o := orderRow(t, eng.db, id); o.Status != protocol.StatusCancelled || o.CoreVersion != 4 {
			t.Errorf("after the notice the supply reads %s at %d, want cancelled at 4", o.Status, o.CoreVersion)
		}
		// The same notice again (a broker redelivers it): the carrier a live
		// move already takes off the park is not sent a second robot.
		notice(h, protocol.StatusStaged)
		expectOneCarrierBack(t, eng.db, nodeID)
		expectRows(t, "the supply's history", orderHistory(t, eng.db, id), []string{
			"in_transit->staged: core: staged (version 3)",
			"staged->cancelled: core: cancelled (version 4)",
		})
	})

	t.Run("the operator aborts it", func(t *testing.T) {
		t.Parallel()
		eng, h, nodeID, id := fixture(t, 3)
		trail := trailOf(eng, map[int64]string{id: "supply"})

		testutil.MustNoErr(t, eng.orderMgr.AbortOrderWithReason(id, "cancelled by operator"), "the operator's abort")
		trail.expect(t, "the abort", "supply staged->cancelled", "supply completed")
		expectNoCarrierBack(t, eng.db, "the operator's abort")

		// Core's reply to the station's own cancel carries no previous status.
		notice(h, "")
		trail.expect(t, "the cancel reply")
		if p, ok := eng.carrierLeftWaitingBy(id, true); !ok || p.park != "STG-1" || p.dest != "SRC-LOADER" {
			t.Fatalf("the notice's read of the carrier = (%+v, %v), want STG-1 back to SRC-LOADER", p, ok)
		}
		expectNoCarrierBack(t, eng.db, "the cancel reply")

		notice(h, protocol.StatusStaged)
		trail.expect(t, "order.cancelled from staged")
		expectOneCarrierBack(t, eng.db, nodeID)
		if o := orderRow(t, eng.db, id); o.Status != protocol.StatusCancelled || o.CoreVersion != 3 {
			t.Errorf("after the notice the supply reads %s at %d, want cancelled at 3", o.Status, o.CoreVersion)
		}
	})

	t.Run("an older Core cancels it", func(t *testing.T) {
		t.Parallel()
		eng, h, nodeID, id := fixture(t, 0)
		trail := trailOf(eng, map[int64]string{id: "supply"})

		notice(h, "")
		trail.expect(t, "an older Core's order.cancelled", "supply staged->cancelled", "supply completed")
		expectOneCarrierBack(t, eng.db, nodeID)
		if o := orderRow(t, eng.db, id); o.Status != protocol.StatusCancelled || o.CoreVersion != 0 {
			t.Errorf("after an older Core's cancel the supply reads %s at %d, want cancelled at 0", o.Status, o.CoreVersion)
		}
	})
}

// TestFollowOns_ALegThatEndsWakesItsSiblingsHeldReleaseOnce: the wake of an
// ended leg's sibling rides EventOrderCompleted (wiring.go), which the
// versioned applier fires where Core's tell ends the order, and the lifecycle
// where an older Core's message does. A press-index pair clicked while R1
// still drives to its wait holds both legs, the staged R2 on R1's lift (G7).
// R1 ending is R2's wake: R2's release goes, once, and its log names the wake.
func TestFollowOns_ALegThatEndsWakesItsSiblingsHeldReleaseOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		versioned bool
		end       protocol.Status
		ends      func(h *relHarness)
	}{
		{name: "Core's cancelled tell", versioned: true, end: protocol.StatusCancelled},
		{name: "Core's failed tell", versioned: true, end: protocol.StatusFailed},
		{name: "Core's confirmed tell", versioned: true, end: protocol.StatusConfirmed},
		{name: "an older Core's order.cancelled", end: protocol.StatusCancelled, ends: func(h *relHarness) {
			h.handler.HandleOrderCancelled(wire, &protocol.OrderCancelled{OrderUUID: h.order("evac").UUID, Reason: "terminated at Core"})
		}},
		{name: "an older Core's order.error", end: protocol.StatusFailed, ends: func(h *relHarness) {
			h.handler.HandleOrderError(wire, &protocol.OrderError{OrderUUID: h.order("evac").UUID, ErrorCode: "fleet_failed", Detail: "RDS error 60009"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRelHarness(t)
			var mu sync.Mutex
			var released []string
			h.eng.logFn = func(format string, args ...any) {
				if line := fmt.Sprintf(format, args...); strings.Contains(line, "released at node") {
					mu.Lock()
					released = append(released, line)
					mu.Unlock()
				}
			}
			h.steadyPair(pairSpec{mode: protocol.SwapModeTwoRobotPressIndex})
			statuses(h, "evac", protocol.StatusInTransit, "supply", protocol.StatusStaged)
			if tc.versioned {
				coreWord(h.handler, h.order("evac").UUID, protocol.StatusInTransit, 2)
				coreWord(h.handler, h.order("supply").UUID, protocol.StatusStaged, 2)
			}
			h.coreStagesAt("supply", 0)
			testutil.MustNoErr(t, held(pairClick(dispEmpty))(h), "the pair click while R1 drives")
			if got, want := h.outcome(nil), "ok | evac=in_transit supply=staged | rel=- | ingest=1 capred=0 | click=held:G7"; got != want {
				t.Fatalf("after the click:\n got  %q\n want %q", got, want)
			}
			expectRows(t, "intents after the click", pIntents(h), []string{"intent:evac=held@0", "intent:supply=held@0"})
			h.extra = nil
			h.mark()
			trail := trailOf(h.eng, map[int64]string{h.leg("evac"): "evac", h.leg("supply"): "supply"})

			if tc.versioned {
				coreWord(h.handler, h.order("evac").UUID, tc.end, 3)
			} else {
				tc.ends(h)
			}

			// A versioned supply's release leaves it staged with its intent
			// sent; an older Core's moves it at the tap.
			supplyAfter, supplyTrail := protocol.StatusStaged, "supply touched"
			if !tc.versioned {
				supplyAfter, supplyTrail = protocol.StatusInTransit, "supply staged->in_transit"
			}
			want := fmt.Sprintf("ok | evac=%s supply=%s | rel=supply | ingest=0 capred=0", tc.end, supplyAfter)
			if got := h.outcome(nil); got != want {
				t.Errorf("after R1 ends:\n got  %q\n want %q", got, want)
			}
			expectRows(t, "intents after R1 ends", pIntents(h), []string{"intent:evac=-", "intent:supply=sent@0"})
			// The bus runs subscribers in subscription order, the engine's before
			// this trail's: R2's release lands after the status event has gone
			// round and before the trail hears Completed, so it ran inside the
			// Completed dispatch, which is where the wake rides.
			trail.expect(t, "R1 ends", "evac in_transit->"+string(tc.end), supplyTrail, "evac completed")
			mu.Lock()
			defer mu.Unlock()
			expectRows(t, "the intent releases logged", released, []string{
				fmt.Sprintf("release intent: order %d released at node %d (sibling ended)", h.leg("supply"), h.nodeID),
			})
		})
	}
}
