package engine

import (
	"regexp"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store/processes"
	"shingoedge/uop"
)

// carrier_handoff_pins_test.go — Lane H's Edge pins (BRIEF-unification-C2c
// "Lane H", 2026-10-10; DESIGN-H; DECISIONS D9–D12). Every pin here compiles
// at the base and is RED there by behaviour, except the ones marked CONTROL,
// which are green before and after. The pins that need the landing subject
// itself (protocol.BinLanded), which does not exist at the base, are in
// carrier_handoff_pending_test.go behind the laneh_pending tag.
//
// Where a pin needs "the landing" and the base has no landing, it uses the
// base's nearest door: the Bound uop_adjustment (DESIGN-H §1a A1), which H.7
// keeps for one release as the old-Core path routed through the landing verb.
// The assertion is the ruling, not the door, so it holds through the switch.

// binPtr is a bin id as the store's pointer argument. The tests that seated a
// carrier through the delivered bind (SetProcessNodeRuntimeForDeliveredBin,
// deleted with H.5) seat it through the same statement's other name.
func binPtr(id int64) *int64 { return &id }

// tickConsume emits one consume tick of delta for the style.
func tickConsume(eng *Engine, processID, styleID int64, delta int64) {
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: delta,
	}})
}

func runtimeOf(t *testing.T, eng *Engine, nodeID int64) *processes.RuntimeState {
	t.Helper()
	rt, err := eng.db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt == nil {
		t.Fatal("runtime row missing")
	}
	return rt
}

// emptySeat seeds a consume node with nothing bound and returns its ids.
func emptySeat(t *testing.T, eng *Engine, prefix, part string, capacity int) (processID, nodeID, styleID, claimID int64) {
	t.Helper()
	processID, nodeID, styleID, claimID = seedConsumeNode(t, eng.db, consumeNodeConfig{
		Prefix: prefix, PayloadCode: part, UOPCapacity: capacity, InitialUOP: 0,
	})
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, nil, 0), "empty the seat")
	return processID, nodeID, styleID, claimID
}

// landBound is the base's landing: Core's Bound announcement of bin at node.
func landBound(eng *Engine, node string, binID, epoch int64, count int) {
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID: binID, CoreNodeName: node, NewRemaining: count, Epoch: epoch, Bound: true,
		Actor: protocol.ActorCoreLifecycle,
	})
}

// deliver emits order.delivered for bin at the node, carrying Core's count.
func deliver(t *testing.T, eng *Engine, nodeID int64, uuid, node string, binID, epoch int64, count int) {
	t.Helper()
	orderID, err := eng.db.CreateOrder(uuid, orders.TypeRetrieve, &nodeID, false, 1, node, "", "", false, "", "", "")
	testutil.MustNoErr(t, err, "create order")
	bid := binID
	testutil.MustNoErr(t, eng.db.UpdateOrderBinID(orderID, &bid), "order bin")
	uop := count
	eng.Events.Emit(Event{Type: EventOrderDelivered, Payload: OrderDeliveredEvent{
		OrderID: orderID, OrderUUID: uuid, OrderType: orders.TypeRetrieve,
		ProcessNodeID: &nodeID, BinID: &bid, BinUOP: &uop, BinEpoch: epoch,
	}})
}

// TestPinH5_LandingThenDeliveredKeepsTheTicks — H.5: order.delivered closes
// the order, binds nothing and writes no count. The carrier lands at N, the
// line counts k, then the order's delivered arrives naming the same bin at
// the same generation with Core's arrival snapshot N.
//
// At the base: RED. OnDelivered (wiring_delivered.go) rebinds through
// SetRuntimeForDeliveredBin, WHERE node only, and writes N back over N − k.
func TestPinH5_LandingThenDeliveredKeepsTheTicks(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	processID, nodeID, styleID, _ := emptySeat(t, eng, "HLD1", "PART-HLD1", 100)

	const bin, epoch, n, k = int64(8101), int64(4), 100, 3
	landBound(eng, "HLD1-NODE", bin, epoch, n)
	for i := 0; i < k; i++ {
		tickConsume(eng, processID, styleID, 1)
	}
	deliver(t, eng, nodeID, "uuid-hld1", "HLD1-NODE", bin, epoch, n)

	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != n-k {
		t.Errorf("count = %d, want %d: the delivered wrote Core's arrival snapshot over the %d parts "+
			"the line counted after the carrier landed (H.5: delivered writes no count)", got, n-k, k)
	}
}

// TestPinH5_DeliveredThenLandingKeepsTheTicks — H.4: the same bin already bound
// at an equal or newer generation keeps its count. The reverse order of the
// pin above: the delivered binds (the base's only final-drop bind), k ticks,
// then the landing for the same bin at the same generation.
//
// At the base: RED. The Bound arm (handler_uop_adjustment.go
// bindAdjustmentFromCore) writes Core's count over the bound bin's.
func TestPinH5_DeliveredThenLandingKeepsTheTicks(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	processID, nodeID, styleID, _ := emptySeat(t, eng, "HLD2", "PART-HLD2", 100)

	const bin, epoch, n, k = int64(8102), int64(4), 100, 3
	deliver(t, eng, nodeID, "uuid-hld2", "HLD2-NODE", bin, epoch, n)
	for i := 0; i < k; i++ {
		tickConsume(eng, processID, styleID, 1)
	}
	landBound(eng, "HLD2-NODE", bin, epoch, n)

	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != n-k {
		t.Errorf("count = %d, want %d: a landing for the bin already bound at the same generation "+
			"must keep its count (H.4)", got, n-k)
	}
}

// TestPinH4_RedeliveredLandingKeepsTheTicks — H.4 / D10: a redelivered landing
// (the Edge has no inbound dedup and the landing never expires, DESIGN-H S2)
// carries the same bin, generation and placement; it re-binds nothing and
// writes no count.
//
// At the base: RED, the Bound arm rewrites N.
func TestPinH4_RedeliveredLandingKeepsTheTicks(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	processID, nodeID, styleID, _ := emptySeat(t, eng, "HLD3", "PART-HLD3", 100)

	const bin, epoch, n, k = int64(8103), int64(4), 100, 3
	landBound(eng, "HLD3-NODE", bin, epoch, n)
	for i := 0; i < k; i++ {
		tickConsume(eng, processID, styleID, 1)
	}
	landBound(eng, "HLD3-NODE", bin, epoch, n)

	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != n-k {
		t.Errorf("count = %d, want %d: the redelivered landing rewrote the count", got, n-k)
	}
}

// releaseEmptyAt binds bin at the seat with count, stages an order for it and
// releases it empty through the operator's door (RELEASE EMPTY = capture
// lineside with nothing captured).
func releaseEmptyAt(t *testing.T, eng *Engine, nodeID, claimID int64, node string, bin int64, count int) {
	t.Helper()
	b := bin
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &b, count), "bind the outgoing bin")
	srv := mockBinUOPServer(t, map[int64]int{})
	bridgeCore(t, eng, srv.URL)
	orderID, err := eng.db.CreateOrder("uuid-rel-"+node, orders.TypeRetrieve,
		&nodeID, false, 1, node, "", "", false, "", "", "")
	testutil.MustNoErr(t, err, "create order")
	testutil.MustNoErr(t, eng.db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil), "runtime order")
	testutil.MustNoErr(t, eng.db.UpdateOrderStatus(orderID, "staged"), "stage the order")
	disp := ReleaseDisposition{Mode: DispositionCaptureLineside, LinesideCapture: map[string]int{}}
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderID, disp), "release empty")
}

func pileQty(t *testing.T, eng *Engine, nodeID int64) int {
	t.Helper()
	bs, err := eng.db.ListLinesideBuckets(nodeID)
	testutil.MustNoErr(t, err, "list piles")
	n := 0
	for _, b := range bs {
		if b.State == "active" {
			n += b.Qty
		}
	}
	return n
}

// TestPinR3_ReleasedEmptyIsNotChargedThenTheNextBinOfThePartTakesTheHold —
// R3 (a bin released as empty is not charged afterwards: pile, then held) and
// R2 (held ticks replay onto the next bin of the same part); built per D12 as
// the seat flag released_empty and pending_uop_payload.
//
// Pile P, release empty, P + 2 ticks: pile 0, the old bin untouched at 0, 2
// held. The carrier leaves; the next bin of that part lands at N; one more
// tick reads N − 3 (the 2 held and the 1). Whether the replay lands at the
// bind or at the next tick is the build's choice; N − 3 after one tick holds
// either way.
//
// At the base: RED. The released bin stays bound until the lift, so the two
// ticks past the pile are charged to it (−2) and nothing is held; the next
// bin reads N − 1.
func TestPinR3_ReleasedEmptyIsNotChargedThenTheNextBinOfThePartTakesTheHold(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	const part, node = "PART-HR3", "HR3-NODE"
	processID, nodeID, styleID, claimID := emptySeat(t, eng, "HR3", part, 100)

	const p = 3
	_, err := db.CaptureLinesideBucket(nodeID, part, p)
	testutil.MustNoErr(t, err, "seed the pile")
	releaseEmptyAt(t, eng, nodeID, claimID, node, 8201, 4)

	for i := 0; i < p+2; i++ {
		tickConsume(eng, processID, styleID, 1)
	}
	rt := runtimeOf(t, eng, nodeID)
	if got := pileQty(t, eng, nodeID); got != 0 {
		t.Errorf("pile = %d, want 0 (the pile drains first)", got)
	}
	if rt.RemainingUOPCached != 0 {
		t.Errorf("released-empty bin count = %d, want 0: a bin released as empty is not charged (R3)", rt.RemainingUOPCached)
	}
	if rt.PendingUOPDelta != 2 {
		t.Errorf("held = %d, want 2: past the pile, ticks are held for the next bin of the part (R3)", rt.PendingUOPDelta)
	}

	testutil.MustNoErr(t, eng.carrierLeft(nodeID, node), "the lift")
	const n = 100
	landBound(eng, node, 8202, 1, n)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(nodeID, part, true, "delivery"), "the carrier is the part")
	tickConsume(eng, processID, styleID, 1)
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != n-3 {
		t.Errorf("next bin of the part = %d, want %d (N, less the 2 held, less the 1) (R2)", got, n-3)
	}
}

// TestPinR2_HeldTicksAreNotReplayedOntoADifferentPart — R2: held ticks replay
// onto the next bin OF THE SAME PART (DESIGN-H §8; D12 keeps X's hold). Hold k
// ticks of part X, cut over to Y, land a Y bin at N; one Y tick reads N − 1.
// Also the "a different part takes none" half of the R3 pin above.
//
// At the base: RED, N − k − 1. pending_uop_delta has no part and replays onto
// whatever binds next; the cutover leaves it alone on purpose
// (lineside_strand.go: "held ticks belong to the next bin").
func TestPinR2_HeldTicksAreNotReplayedOntoADifferentPart(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	processID, nodeID, fromStyleID, toStyleID, fromClaimID, toClaimID := seedChangeoverScenario(t, db)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &fromClaimID, nil, 0), "empty the seat")

	const k = 2
	for i := 0; i < k; i++ {
		tickConsume(eng, processID, fromStyleID, 1)
	}
	if got := runtimeOf(t, eng, nodeID).PendingUOPDelta; got != k {
		t.Fatalf("fixture: held = %d, want %d", got, k)
	}

	testutil.MustNoErr(t, eng.SetProcessActiveStyle(processID, &toStyleID), "cut over to Y")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &toClaimID, 0), "seat on Y's claim")
	const n = 200
	landBound(eng, "CO-NODE", 8301, 1, n)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(nodeID, "PART-NEW", true, "delivery"), "the carrier is Y")
	tickConsume(eng, processID, toStyleID, 1)

	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != n-1 {
		t.Errorf("Y bin = %d, want %d: %d ticks of PART-OLD were replayed onto a PART-NEW carrier (R2)", got, n-1, k)
	}
}

// TestPinH_ClaimAdvanceLeavesTheCount — Also in H (SYNTH §5; DESIGN-H §9.1):
// a claim change does not write the count. The two completion-cascade sites
// (wiring_completion.go applyStagedDelivery and advanceClaimOnRelease) pass
// back a count read when the completion context was loaded; a tick landing
// between that read and the write is erased.
//
// At the base: RED for both, SetClaimAndCount writes the snapshot.
func TestPinH_ClaimAdvanceLeavesTheCount(t *testing.T) {
	t.Parallel()

	t.Run("advanceClaimOnRelease", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		eng := testEngine(t, db)
		eng.wireEventHandlers()
		processID, nodeID, toStyleID, fromClaimID, _ := seedDirectChangeover(t, db)
		b := int64(8401)
		testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &fromClaimID, &b, 3000), "bind")
		ctx := releaseCtx(t, eng, db, processID, nodeID, toStyleID)
		before := ctx.runtime.RemainingUOPCached

		fromStyle := activeStyleOf(t, eng, processID)
		tickConsume(eng, processID, fromStyle, 2)
		advanceClaimOnRelease(eng, ctx)

		if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != before-2 {
			t.Errorf("count = %d, want %d: the claim advance wrote back the count it read before the tick", got, before-2)
		}
	})

	t.Run("applyStagedDelivery", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		eng := testEngine(t, db)
		eng.wireEventHandlers()
		processID, nodeID, fromStyleID, toStyleID, fromClaimID, _ := seedChangeoverScenario(t, db)
		b := int64(8402)
		testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &fromClaimID, &b, 50), "bind")
		_, task := startChangeover(t, eng, db, processID, toStyleID)
		if task.NextMaterialOrderID == nil {
			t.Skip("fixture planned no staging order for this node")
		}
		order, err := db.GetOrder(*task.NextMaterialOrderID)
		testutil.MustNoErr(t, err, "read staging order")
		node, err := db.GetProcessNode(nodeID)
		testutil.MustNoErr(t, err, "read node")
		rt, err := db.EnsureProcessNodeRuntime(nodeID)
		testutil.MustNoErr(t, err, "read runtime")
		ctx := &orderCompletionCtx{order: order, node: node, runtime: rt, e: eng, toStyleID: toStyleID, nodeTask: task}
		if !matchStagedDelivery(ctx) {
			t.Skip("fixture does not reach staged_delivery")
		}

		tickConsume(eng, processID, fromStyleID, 2)
		applyStagedDelivery(eng, ctx)

		if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != rt.RemainingUOPCached-2 {
			t.Errorf("count = %d, want %d: the staged delivery's claim advance wrote back a stale count",
				got, rt.RemainingUOPCached-2)
		}
	})
}

func activeStyleOf(t *testing.T, eng *Engine, processID int64) int64 {
	t.Helper()
	p, err := eng.db.GetProcess(processID)
	testutil.MustNoErr(t, err, "read process")
	if p.ActiveStyleID == nil {
		t.Fatal("process has no active style")
	}
	return *p.ActiveStyleID
}

// countWrite matches a statement that writes remaining_uop_cached.
var countWrite = regexp.MustCompile(`(?is)UPDATE\s+process_node_runtime_states\s+SET.*remaining_uop_cached\s*=`)

// TestPinH_ClaimAdvanceStatementWritesNoCount — Also in H, the two sites that
// re-read before they write (changeover_restore.go reconcileNodeTask,
// operator_node_changeover.go SwitchNodeToTarget). Their race is not
// reachable from one goroutine, so the pin is on the statement, as DESIGN-H
// §9.1 asks: the claim advance is a claim-only UPDATE, the same statement
// count with one column less.
//
// At the base: RED, both issue processes.SetRuntime (active_claim_id,
// remaining_uop_cached).
func TestPinH_ClaimAdvanceStatementWritesNoCount(t *testing.T) {
	t.Parallel()

	t.Run("reconcileNodeTask", func(t *testing.T) {
		t.Parallel()
		db, counter := testEngineDBCounting(t)
		nodeID, toStyleID, toClaimID, task := seedStagingRestoreTask(t, db)
		bin := int64(8411)
		testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &toClaimID, &bin, 3, 175), "bind")
		eng := testEngine(t, db)
		eng.SetInventoryDeltaSink(&fakeDeltaSink{db: db})

		counter.Reset()
		if !eng.reconcileNodeTask(task, toStyleID) {
			t.Fatal("reconcileNodeTask did not advance the staging task")
		}
		for _, s := range counter.Statements() {
			if countWrite.MatchString(s) {
				t.Errorf("the restore's claim advance wrote the count: %s", oneLine(s))
			}
		}
	})

	t.Run("SwitchNodeToTarget", func(t *testing.T) {
		t.Parallel()
		db, counter := testEngineDBCounting(t)
		eng := testEngine(t, db)
		processID, nodeID, _, toStyleID, fromClaimID, _ := seedChangeoverScenario(t, db)
		eng.wireEventHandlers()
		_, _ = startChangeover(t, eng, db, processID, toStyleID)
		testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &fromClaimID, binPtr(8412), 1, 1750), "bind")

		counter.Reset()
		testutil.MustNoErr(t, eng.SwitchNodeToTarget(processID, nodeID), "switch")
		for _, s := range counter.Statements() {
			if countWrite.MatchString(s) {
				t.Errorf("the switch's claim advance wrote the count: %s", oneLine(s))
			}
		}
	})
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestPinH_OrderBSimpleCompletionLeavesADifferentCarrier — Also in H
// (DESIGN-H §9.2): Order B's simple completion checks which carrier is on the
// seat before clearing it. Order B carried bin X away; bin Y has since landed
// on the seat. Y stays bound with its count.
//
// At the base: RED. applyOrderBSimple calls carrierLeft with no bin check.
func TestPinH_OrderBSimpleCompletionLeavesADifferentCarrier(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, claimID := emptySeat(t, eng, "HOB", "PART-HOB", 100)

	const departed, landed = int64(8501), int64(8502)
	y := landed
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &y, 90), "Y on the seat")
	orderID, err := db.CreateOrder("uuid-hob", orders.TypeMove, &nodeID, false, 1, "HOB-OUT", "", "", false, "", "", "")
	testutil.MustNoErr(t, err, "create Order B")
	x := departed
	testutil.MustNoErr(t, db.UpdateOrderBinID(orderID, &x), "Order B carried X")
	order, err := db.GetOrder(orderID)
	testutil.MustNoErr(t, err, "read order")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read node")
	rt := runtimeOf(t, eng, nodeID)
	ctx := &orderCompletionCtx{order: order, node: node, runtime: rt, e: eng,
		nodeTask: &processes.NodeTask{State: domain.NodeTaskLineCleared}}

	applyOrderBSimple(eng, ctx)

	got := runtimeOf(t, eng, nodeID)
	if got.ActiveBinID == nil || *got.ActiveBinID != landed || got.RemainingUOPCached != 90 {
		t.Errorf("seat = bin %v count %d, want bin %d count 90: Order B for bin %d cleared the carrier that landed after it",
			got.ActiveBinID, got.RemainingUOPCached, landed, departed)
	}
}

// TestPinH_PickupHandlerReadsItsOrderOnce — Also in H (DESIGN-H §9.3):
// HandleBinPickedUp reads the order by uuid, and onPickupForIntents reads it
// again. The named delta is −1 read per pickup. Counted on the statement text
// of orders.GetByUUID ("WHERE o.uuid = ?").
//
// At the base: RED, 2 reads.
func TestPinH_PickupHandlerReadsItsOrderOnce(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	eng := testEngine(t, db)
	_, nodeID, _, claimID := emptySeat(t, eng, "HPU", "PART-HPU", 100)
	b := int64(8601)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &b, 10), "bind")
	_, err := db.CreateOrder("uuid-hpu", orders.TypeComplex, &nodeID, false, 1, "HPU-OUT", "", "", false, "", "", "")
	testutil.MustNoErr(t, err, "create order")

	counter.Reset()
	eng.HandleBinPickedUp("uuid-hpu", b, "HPU-NODE")
	n := 0
	for _, s := range counter.Statements() {
		if strings.Contains(s, "o.uuid = ?") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the pickup read its order by uuid %d times, want 1", n)
	}
}

// TestPinF7_LocalClearThenTickThenUnfencedEchoKeepsTheTick — condor F7
// (DESIGN-H §9.5, S7): a person's CLEAR is applied twice at the station, once
// from the HTTP reply and again from the unfenced A4 broadcast of the same
// act. The local clear (operator_bin_ops.go ClearBin) writes 0 at E+1; one
// tick charges −1 at E+1, and Core applies its delta; Core's A4
// {B, 0, E+1, lifecycle} then arrives. Both books read −1.
//
// The clear's own write is reproduced by its store statement: the door is a
// loader door and a loader takes no PLC tick, so the tick is on a line seat.
// The observable is the A4 arm, which is the same for either.
//
// At the base: RED. fencedCount declines (no AsOfNet) and the absolute arm
// writes 0 over −1.
func TestPinF7_LocalClearThenTickThenUnfencedEchoKeepsTheTick(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	processID, nodeID, styleID, claimID := emptySeat(t, eng, "HF7C", "PART-HF7C", 100)
	const bin, e = int64(8701), int64(5)
	b := bin
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &claimID, &b, e, 40), "bound at E")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeClaimCountAndEpoch(nodeID, &claimID, 0, bin, e+1), "the local CLEAR")

	tickConsume(eng, processID, styleID, 1)
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID: bin, CoreNodeName: "HF7C-NODE", NewRemaining: 0, Epoch: e + 1, Actor: protocol.ActorCoreLifecycle,
	})

	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != -1 {
		t.Errorf("count = %d, want -1: the clear's echo erased the tick counted after it", got)
	}
}

// TestPinF7_LocalLoadThenTickThenUnfencedEchoKeepsTheTick — condor F7, LOAD:
// ManualLoad writes N at E+1 from the reply; one tick; Core's A4 {B, N, E+1}
// arrives. The station reads N − 1.
//
// At the base: RED, N.
func TestPinF7_LocalLoadThenTickThenUnfencedEchoKeepsTheTick(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	processID, nodeID, styleID, claimID := emptySeat(t, eng, "HF7L", "PART-HF7L", 100)
	const bin, e, n = int64(8702), int64(6), 60
	b := bin
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &claimID, &b, e+1, n), "the local LOAD")

	tickConsume(eng, processID, styleID, 1)
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID: bin, CoreNodeName: "HF7L-NODE", NewRemaining: n, Epoch: e + 1, Actor: protocol.ActorCoreLifecycle,
	})

	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != n-1 {
		t.Errorf("count = %d, want %d: the load's echo erased the tick counted after it", got, n-1)
	}
}

// ── Controls: green at the base and after Lane H ──

// TestControlH_EmptySeatLandingSeedsCoresCount — CONTROL (BRIEF Lane H pins):
// a landing on an empty seat binds the carrier with Core's count and
// generation.
func TestControlH_EmptySeatLandingSeedsCoresCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, _ := emptySeat(t, eng, "HCE", "PART-HCE", 100)

	landBound(eng, "HCE-NODE", 8801, 7, 64)

	rt := runtimeOf(t, eng, nodeID)
	if rt.ActiveBinID == nil || *rt.ActiveBinID != 8801 || rt.RemainingUOPCached != 64 || rt.ActiveBinEpoch != 7 {
		t.Errorf("seat = bin %v count %d epoch %d, want 8801/64/7", rt.ActiveBinID, rt.RemainingUOPCached, rt.ActiveBinEpoch)
	}
}

// TestControlH_ADifferentCarrierWrites — CONTROL: a landing of a different
// carrier binds it with its own count (the old one left; DESIGN-H §5 L8 is
// refused only by the placement stamp, D10, which this fixture does not set).
func TestControlH_ADifferentCarrierWrites(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, claimID := emptySeat(t, eng, "HCD", "PART-HCD", 100)
	old := int64(8802)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &claimID, &old, 2, 12), "old carrier")

	landBound(eng, "HCD-NODE", 8803, 1, 77)

	rt := runtimeOf(t, eng, nodeID)
	if rt.ActiveBinID == nil || *rt.ActiveBinID != 8803 || rt.RemainingUOPCached != 77 {
		t.Errorf("seat = bin %v count %d, want 8803/77", rt.ActiveBinID, rt.RemainingUOPCached)
	}
}

// TestControlH_ANewerGenerationWrites — CONTROL: Core's announcement of a new
// generation of the bound bin (a reset by a person) writes its count.
func TestControlH_ANewerGenerationWrites(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, claimID := emptySeat(t, eng, "HCG", "PART-HCG", 100)
	b := int64(8804)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &claimID, &b, 3, 12), "bound at 3")

	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID: b, CoreNodeName: "HCG-NODE", NewRemaining: 80, Epoch: 4, Actor: "admin",
	})

	rt := runtimeOf(t, eng, nodeID)
	if rt.RemainingUOPCached != 80 || rt.ActiveBinEpoch != 4 {
		t.Errorf("seat = count %d epoch %d, want 80/4", rt.RemainingUOPCached, rt.ActiveBinEpoch)
	}
}

// TestControlH_TheFencedCountRebases — CONTROL: a fenced count (A5) rebases on
// Core's cursor and keeps the ticks Core had not applied (count_fence.go).
func TestControlH_TheFencedCountRebases(t *testing.T) {
	t.Parallel()
	eng, nodeID, acc, binID := fenceFixture(t, "HC-FENCE")
	_ = acc.Consumed(uopTick(binID, 10))
	acc.Flush()
	_ = acc.Consumed(uopTick(binID, 2)) // not flushed

	eng.HandleUOPAdjustment(fenced(eng, binID, "HC-FENCE", 50, -10, 1))

	if got := runtimeCount(t, eng, nodeID); got != 48 {
		t.Errorf("count = %d, want 48 (50 plus the unflushed -2)", got)
	}
}

// uopTick is a consume tick of n on the fence fixture's bin (epoch 3).
func uopTick(binID int64, n int) uop.TickEvent {
	return uop.TickEvent{BinID: binID, PayloadCode: "PART-A", BinRemainder: n, BinEpoch: 3}
}

// TestControlH_ProduceTickWithNoBinIsHeldAndReplayed — CONTROL: a produce tick
// at a seat with no carrier, while the next carrier is coming (a live order
// placing it), is held, and replays onto that carrier.
func TestControlH_ProduceTickWithNoBinIsHeldAndReplayed(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	processID, nodeID, styleID, claimID := seedProduceNode(t, db, protocol.SwapModeSimple)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, nil, 0), "empty the seat")
	p2Supply(t, eng, claimID)

	tickConsume(eng, processID, styleID, 1)
	tickConsume(eng, processID, styleID, 1)
	if got := runtimeOf(t, eng, nodeID).PendingUOPDelta; got != 2 {
		t.Fatalf("held = %d, want 2", got)
	}
	landBound(eng, "PRODUCE-NODE", 8805, 1, 0)
	tickConsume(eng, processID, styleID, 1)

	rt := runtimeOf(t, eng, nodeID)
	if rt.RemainingUOPCached != 3 || rt.PendingUOPDelta != 0 {
		t.Errorf("seat = count %d held %d, want 3/0", rt.RemainingUOPCached, rt.PendingUOPDelta)
	}
}
