package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// changeover_cancel_start_test.go — the SPECIFICATION for cancelling
// pre-dispatch orders at changeover initiation.
//
// These tests are written BEFORE the code and are expected to fail until the
// cancel lands. They assert BEHAVIOUR through StartProcessChangeover — never the
// mechanism — because the mechanism changes underneath them: the cancel finds
// its orders through the runtime slots at first and through a destination-keyed
// surface afterwards, and neither of those is the thing being specified.
//
// WHY A CANCEL AT ALL. An operator starting a changeover is direct communication
// that the line is going a different direction. A pre-dispatch order has no
// carrier assigned — nothing is moving, nothing can be stranded — so leaving it
// alive serves nobody: it survives the cutover, keeps its claim on the old
// style's payload, and eventually delivers material to a cell that stopped
// running that style hours ago.
//
// WHY IT IS SAFE, WHICH IS THE WHOLE HISTORY OF THIS FILE. An AbortNodeOrders
// sweep used to run here and was removed (8553178a) because it cancelled BY
// NODE: on a press-index swap the in-flight legs are frequently carrying the
// very empty carriers the changeover's own index legs need to pick up, and
// cancelling those mid-delivery is a permanent deadlock (HK 2026-07-28 —
// orders 1249/1251 escaped it by accident). This cancel is scoped BY STATUS to
// protocol.IsPreDispatch, so it provably cannot touch a carrier. That is the
// entire difference between the two, and it is why the same operation is now
// correct where it was once dangerous.

// seedSecondProcess creates an independent process + node, for asserting the
// cancel does not reach across process boundaries.
func seedSecondProcess(t *testing.T, db *store.DB) (processID, nodeID int64) {
	t.Helper()
	processID, err := db.CreateProcess("OTHER-PROC", "unrelated process", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create other process")
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "OTHER-NODE",
		Code:         "OTH1",
		Name:         "Other Node",
		Sequence:     1,
		Enabled:      true,
	})
	testutil.MustNoErr(t, err, "create other node")
	db.EnsureProcessNodeRuntime(nodeID)
	return processID, nodeID
}

// seedOrderAt creates an order attached to a node, drives it to the given
// status, and parks it in the node's active runtime slot.
//
// delivery_node is set to the node's CORE name as well as the process_node_id
// being attached, so the order is discoverable both by the runtime-slot surface
// and by the destination-keyed one. These tests specify what happens to the
// order, not how it was found.
func seedOrderAt(t *testing.T, db *store.DB, nodeID int64, coreNode, uuid, payload string, status protocol.Status) int64 {
	t.Helper()
	id, err := db.CreateOrder(uuid, orders.TypeMove, &nodeID, false, 1,
		coreNode, "", "SOURCE-OLD", "", false, payload)
	testutil.MustNoErr(t, err, "create order "+uuid)
	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(status)), "set status "+string(status))
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &id, nil), "attach order to runtime")
	return id
}

func reloadStatus(t *testing.T, db *store.DB, orderID int64) protocol.Status {
	t.Helper()
	o, err := db.GetOrder(orderID)
	testutil.MustNoErr(t, err, "reload order")
	return o.Status
}

// TestChangeoverStart_CancelsPreDispatchAtParticipant is the floor complaint,
// stated as a test. Every status in protocol.IsPreDispatch gets its own subtest:
// `queued` is the one operators hit, but pending and sourcing are the same fact
// one step earlier and a cancel that missed them would leave the same stale
// order behind for a narrower reason.
//
// The order is a SIMPLE move on purpose. BuildConsumePlan downgrades a swap to a
// simple move when the head node is empty, and an empty head IS the starvation
// condition this project exists for — so a type filter would systematically miss
// the case the work was called for.
func TestChangeoverStart_CancelsPreDispatchAtParticipant(t *testing.T) {
	t.Parallel()
	for _, status := range []protocol.Status{
		orders.StatusQueued,
		orders.StatusPending,
		orders.StatusSourcing,
	} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
			eng := testEngine(t, db)

			orderID := seedOrderAt(t, db, nodeID, "CO-NODE", "uuid-cancel-"+string(status), "PART-OLD", status)

			if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err != nil {
				t.Fatalf("changeover refused with only a %s order at the node: %v", status, err)
			}

			if got := reloadStatus(t, db, orderID); got != orders.StatusCancelled {
				t.Errorf("order status = %s, want cancelled — a pre-dispatch order has no "+
					"carrier and the operator has said the line is going elsewhere", got)
			}
		})
	}
}

// TestChangeoverStart_CancelsBothSwapSiblings pins the sibling rule. A complex
// consume request is created as a LINKED PAIR, and cancelling one leg leaves the
// survivor parked on QueueWaitingForPartner waiting for a partner that will
// never claim a bin — a strictly worse state than the one the cancel set out to
// clear.
func TestChangeoverStart_CancelsBothSwapSiblings(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)

	supplyID := seedOrderAt(t, db, nodeID, "CO-NODE", "uuid-swap-supply", "PART-OLD", orders.StatusQueued)
	evacID, err := db.CreateOrder("uuid-swap-evac", orders.TypeComplex, &nodeID, false, 1,
		"CO-NODE", "", "SOURCE-OLD", "", false, "PART-OLD")
	testutil.MustNoErr(t, err, "create evac leg")
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(orders.StatusQueued)), "queue evac leg")
	testutil.MustNoErr(t, db.LinkOrderSiblings(supplyID, evacID), "link siblings")
	// Both legs of a swap occupy the head node's two runtime slots.
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &supplyID, &evacID), "attach both legs")

	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err != nil {
		t.Fatalf("changeover refused with two queued legs at the node: %v", err)
	}

	if got := reloadStatus(t, db, supplyID); got != orders.StatusCancelled {
		t.Errorf("supply leg = %s, want cancelled", got)
	}
	if got := reloadStatus(t, db, evacID); got != orders.StatusCancelled {
		t.Errorf("evac leg = %s, want cancelled — cancelling one leg of a linked pair "+
			"strands the other on waiting_for_partner forever", got)
	}
}

// TestChangeoverStart_LeavesAnotherProcessAlone scopes the cancel. A changeover
// on one process must not reach into another's orders, however similar they
// look — the node set is the changeover's own participants and nothing else.
func TestChangeoverStart_LeavesAnotherProcessAlone(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	_, otherNodeID := seedSecondProcess(t, db)
	eng := testEngine(t, db)

	mine := seedOrderAt(t, db, nodeID, "CO-NODE", "uuid-mine", "PART-OLD", orders.StatusQueued)
	theirs := seedOrderAt(t, db, otherNodeID, "OTHER-NODE", "uuid-theirs", "PART-OLD", orders.StatusQueued)

	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err != nil {
		t.Fatalf("changeover refused: %v", err)
	}

	if got := reloadStatus(t, db, mine); got != orders.StatusCancelled {
		t.Errorf("own-process order = %s, want cancelled", got)
	}
	if got := reloadStatus(t, db, theirs); got == orders.StatusCancelled {
		t.Error("an order at another process's node was cancelled — the cancel must be " +
			"scoped to this changeover's participants")
	}
}

// TestChangeoverStart_LeavesBlockingOrdersAlone is the negative that keeps the
// cancel honest, and it is the Hopkinsville lesson in test form. A carrier in
// motion must be REFUSED, never cancelled — cancelling it mid-delivery is the
// deadlock the removed sweep would have caused.
func TestChangeoverStart_LeavesBlockingOrdersAlone(t *testing.T) {
	t.Parallel()
	for _, status := range []protocol.Status{
		orders.StatusInTransit,
		orders.StatusStaged,
		orders.StatusFaulted,
	} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
			eng := testEngine(t, db)

			orderID := seedOrderAt(t, db, nodeID, "CO-NODE", "uuid-block-"+string(status), "PART-OLD", status)

			if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err == nil {
				t.Fatalf("changeover started with a %s order at the node — live choreography must block", status)
			}
			if got := reloadStatus(t, db, orderID); got != status {
				t.Errorf("order moved to %s — a blocking order must be left EXACTLY as it was; "+
					"cancelling a carrier mid-delivery is the HK 2026-07-28 deadlock", got)
			}
		})
	}
}

// TestChangeoverStart_CancelReportsWhatItCancelled pins the return contract the
// abandon path depends on. Cancelling at initiation destroys a request the
// process may still need if the changeover is later abandoned, and it self-heals
// only where the claim has auto-reorder. The minimum owed to the operator is
// that the system can say what it took away, which requires the cancel to return
// the list rather than discard it.
func TestChangeoverStart_CancelReportsWhatItCancelled(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)

	orderID := seedOrderAt(t, db, nodeID, "CO-NODE", "uuid-reported", "PART-OLD", orders.StatusQueued)

	plan, err := eng.planChangeover(processID, toStyleID)
	testutil.MustNoErr(t, err, "plan changeover")
	cancelled, cerr := eng.cancelPreDispatchAtParticipants(plan)
	testutil.MustNoErr(t, cerr, "cancel")
	if len(cancelled) != 1 || cancelled[0] != orderID {
		t.Errorf("cancelled = %v, want exactly [%d] — the abandon path needs the ID list "+
			"to tell the operator what was taken away", cancelled, orderID)
	}
}

// TestChangeoverStart_ClearsTheRuntimeSlotItCancelled — a pointer left aiming at
// a cancelled order is the phantom-badge family: the board reads the slot, finds
// a terminal order, and renders state for work that no longer exists.
//
// The invariant is "the slot never names dead work", NOT "the slot is empty".
// This asserted emptiness until 2026-08-03, when the applier started repointing
// the slots at the legs it creates (see changeover_applier.go) — so the expected
// end state is now the fresh supply leg sitting where the cancelled order was.
// Emptiness was the accidental shape of the correct answer, and pinning it would
// have made the Springfield ALN_001 fix look like a regression.
func TestChangeoverStart_ClearsTheRuntimeSlotItCancelled(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)

	cancelledID := seedOrderAt(t, db, nodeID, "CO-NODE", "uuid-slotclear", "PART-OLD", orders.StatusQueued)

	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err != nil {
		t.Fatalf("changeover refused: %v", err)
	}

	runtime, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "reload runtime")
	if runtime == nil {
		return
	}
	for _, slot := range []struct {
		name string
		id   *int64
	}{
		{"active", runtime.ActiveOrderID},
		{"staged", runtime.StagedOrderID},
	} {
		if slot.id == nil {
			continue
		}
		if *slot.id == cancelledID {
			t.Errorf("%s slot still points at order %d after it was cancelled", slot.name, cancelledID)
			continue
		}
		// Whatever replaced it must be live work, or the phantom badge is
		// back wearing a different order ID.
		o, gerr := db.GetOrder(*slot.id)
		if gerr != nil || o == nil {
			t.Errorf("%s slot points at order %d which does not load: %v", slot.name, *slot.id, gerr)
			continue
		}
		if orders.IsTerminal(protocol.Status(o.Status)) {
			t.Errorf("%s slot points at order %d in terminal status %s", slot.name, o.ID, o.Status)
		}
	}
}

// TestChangeoverPlan_ParticipantsNeverContainALoaderNode is the invariant that
// REPLACES a carve-out rather than documenting one.
//
// Earlier revisions of this design carried a swap-mode clause in the cancel to
// spare threshold L1s and operator REQUEST EMPTY orders, on the reasoning that
// manual_swap loader windows could legitimately be participants. That reasoning
// was wrong: loaders do not change over. domain.Loader carries no style, no
// active_style_id and no swap mode, and a changeover's node set is built from
// STYLE CLAIMS — so a loader window cannot enter it.
//
// Asserting it here rather than branching on it in the cancel puts the check
// where the truth lives. If a future change ever does put a loader node in the
// set, this fails and the carve-out conversation restarts from evidence — rather
// than a special case sitting in the cancel forever, unexplained and load-bearing.
func TestChangeoverPlan_ParticipantsNeverContainALoaderNode(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID, _, _ := seedChangeoverScenario(t, db)

	// A manual_swap loader window on the same process, claimed by the ACTIVE
	// style — the shape the deleted carve-out was written to defend against.
	loaderNodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "SMN-LOADER",
		Code:         "SMN1",
		Name:         "Loader Window",
		Sequence:     2,
		Enabled:      true,
	})
	testutil.MustNoErr(t, err, "create loader node")
	db.EnsureProcessNodeRuntime(loaderNodeID)

	eng := testEngine(t, db)
	plan, err := eng.planChangeover(processID, toStyleID)
	testutil.MustNoErr(t, err, "plan changeover")

	for _, p := range plan.participants {
		if p.CoreNodeName == "SMN-LOADER" {
			t.Fatalf("a loader window entered plan.participants (role %q) — loaders do not "+
				"change over, and the cancel relies on that instead of a swap-mode clause. "+
				"If this is now legitimate, the carve-out discussion reopens", p.Role)
		}
	}
	for _, d := range plan.diffs {
		if d.CoreNodeName == "SMN-LOADER" {
			t.Fatal("a loader window entered plan.diffs — same invariant, one level down")
		}
	}
}
