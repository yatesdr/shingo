package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/lineside"
	"shingoedge/store/processes"
	"shingoedge/uop"
)

// lineside_bucket_pins_test.go — the Edge surfaces the bucket-level change
// touches that no other test already asserts: the drain in the changeover
// window, the A/B fallthrough and unbound-tick drains, the cutover and the
// admin style flip, the admin Clear, and a process delete. Pinned before the
// change; each test names the expected change it flipped under, or says it
// stays.

// levelsInOutbox decodes every pending LinesideBucketLevel.
func levelsInOutbox(t *testing.T, db *store.DB) []protocol.LinesideBucketLevel {
	t.Helper()
	msgs, err := db.ListPendingOutbox(1000)
	testutil.MustNoErr(t, err, "list outbox")
	var out []protocol.LinesideBucketLevel
	for _, m := range msgs {
		if m.MsgType != string(protocol.SubjectLinesideBucketLevel) {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "decode envelope")
		var data protocol.Data
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &data), "decode data")
		var l protocol.LinesideBucketLevel
		testutil.MustNoErr(t, json.Unmarshal(data.Body, &l), "decode level")
		out = append(out, l)
	}
	return out
}

// levelsByKey indexes levels as "<core>/<payload>/<state>" → qty (the last one
// wins, as it does at Core).
func levelsByKey(levels []protocol.LinesideBucketLevel) map[string]int {
	out := map[string]int{}
	for _, l := range levels {
		out[l.CoreNodeName+"/"+l.PayloadCode+"/"+string(l.State)] = l.Qty
	}
	return out
}

func pileRows(t *testing.T, db *store.DB, nodeID int64) map[string]lineside.Bucket {
	t.Helper()
	rows, err := db.ListLinesideBuckets(nodeID)
	testutil.MustNoErr(t, err, "list buckets")
	out := map[string]lineside.Bucket{}
	for _, r := range rows {
		out[r.PayloadCode+"/"+r.State] = r
	}
	return out
}

// activePile reads a node's active pile of a payload; ok is false when there
// is none.
func activePile(t *testing.T, db *store.DB, nodeID int64, payload string) (qty int, ok bool) {
	t.Helper()
	b, ok := pileRows(t, db, nodeID)[payload+"/"+lineside.StateActive]
	return b.Qty, ok
}

// ── Drain ───────────────────────────────────────────────────────────────

// THE CHANGEOVER WINDOW (the 2026-05-19 fix). The release captured the pulled
// parts while the process still runs the from-style, and its ticks carry the
// from-style. The pile drains anyway, until the cutover strands it.
// Stays. The pile's style stamp (and the StyleID the bucket message carried)
// went under change #1; the drain is one dirty mark carrying Drained 3.
func TestCounterDelta_ChangeoverWindowDrainsThePile(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, fromStyleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "CO-WINDOW", PayloadCode: "SYN-PART-1", UOPCapacity: 100, InitialUOP: 50,
	})
	toStyleID, err := db.CreateStyle("CO-WINDOW-TO", "", processID)
	testutil.MustNoErr(t, err, "create to-style")
	_, err = upsertClaimRetiredMode(db, processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "CO-WINDOW-NODE", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSimple, PayloadCode: "SYN-PART-2", UOPCapacity: 100,
	})
	testutil.MustNoErr(t, err, "to-style claim")
	_, err = db.CaptureLinesideBucket(nodeID, "SYN-PART-1", 10)
	testutil.MustNoErr(t, err, "capture at the release")

	eng := testEngine(t, db)
	sink := &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)
	eng.handleCounterDelta(CounterDeltaEvent{ProcessID: processID, StyleID: fromStyleID, Delta: 3})

	if p := pileRows(t, db, nodeID)["SYN-PART-1/active"]; p.Qty != 7 {
		t.Errorf("pile = %+v, want active 7 (10 - 3): the from-style tick drains the pile", p)
	}
	if len(sink.bucketCalls) != 1 || sink.bucketCalls[0].Drained != 3 || sink.bucketCalls[0].State != lineside.StateActive {
		t.Errorf("bucket calls = %+v, want one active mark carrying Drained 3", sink.bucketCalls)
	}
	if len(sink.binCalls) != 0 {
		t.Errorf("bin calls = %+v, want none: the pile covered the whole tick", sink.binCalls)
	}
}

// A/B FALLTHROUGH. Neither side pulls; the first parked side takes the tick,
// and it drains that side's pile first: a drain on the pile, the remainder as
// ab_fallthrough on the bin.
// Stays (the drain); the bucket message's shape changed under change #1.
func TestCounterDelta_ABFallthroughDrainsTheParkedSidesPileFirst(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	b1, b2 := int64(61), int64(62)
	f := seedWalkerProcess(t, db, "FALLPILE", []walkerNode{
		{Suffix: "A", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSequential,
			PayloadCode: "SYN-PART-A", Capacity: 40, Remaining: 100, BoundBin: &b1,
			Paired: "FALLPILE_B", ActivePull: false},
		{Suffix: "B", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSequential,
			PayloadCode: "SYN-PART-A", Capacity: 40, Remaining: 100, BoundBin: &b2,
			Paired: "FALLPILE_A", ActivePull: false},
	})
	nodeA := f.node(t, "A")
	_, err := db.CaptureLinesideBucket(nodeA, "SYN-PART-A", 2)
	testutil.MustNoErr(t, err, "capture pile at A")

	eng := testEngine(t, db)
	sink := &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)
	eng.handleCounterDelta(CounterDeltaEvent{ProcessID: f.ProcessID, StyleID: f.StyleID, Delta: 5})

	if rows := pileRows(t, db, nodeA); len(rows) != 0 {
		t.Errorf("piles at A = %+v, want none: 2 against a tick of 5 drains to zero and the row goes", rows)
	}
	if len(sink.bucketCalls) != 1 || sink.bucketCalls[0].NodeID != nodeA || sink.bucketCalls[0].Drained != 2 {
		t.Errorf("bucket calls = %+v, want one mark at A carrying Drained 2", sink.bucketCalls)
	}
	if len(sink.binCalls) != 1 || sink.binCalls[0].BinID != b1 || sink.binCalls[0].Delta != -3 ||
		sink.binCalls[0].Reason != protocol.ReasonABFallthrough {
		t.Errorf("bin calls = %+v, want one ab_fallthrough -3 against bin %d", sink.binCalls, b1)
	}
	if got := tickOutcomes(t, db, f)["A"]; got.Remaining != 97 {
		t.Errorf("node A = %+v, want remaining 97 (only the remainder reaches the bin)", got)
	}
}

// HOLD-AND-REPLAY WITH A PILE. No bin is bound (the pickup-to-delivery gap). The
// tick drains the pile and marks it at once; only the remainder is held in
// pending_uop_delta, and no bin delta goes out.
// Stays.
func TestCounterDelta_UnboundTickDrainsPileAndHoldsOnlyTheRemainder(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "GAP-PILE", PayloadCode: "SYN-PART-1", UOPCapacity: 100, InitialUOP: 100,
	})
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil), "open the gap")
	_, err := db.CaptureLinesideBucket(nodeID, "SYN-PART-1", 2)
	testutil.MustNoErr(t, err, "capture pile")

	eng := testEngine(t, db)
	sink := &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)
	eng.handleCounterDelta(CounterDeltaEvent{ProcessID: processID, StyleID: styleID, Delta: 5})

	if len(sink.bucketCalls) != 1 || sink.bucketCalls[0].Drained != 2 {
		t.Errorf("bucket calls = %+v, want one mark carrying Drained 2: the pile drains regardless of the bin", sink.bucketCalls)
	}
	if len(sink.binCalls) != 0 {
		t.Errorf("bin calls = %+v, want none while no bin is bound", sink.binCalls)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.PendingUOPDelta != 3 || rt.RemainingUOPCached != 100 {
		t.Errorf("runtime pending/remaining = %d/%d, want 3/100: only the remainder is held",
			rt.PendingUOPDelta, rt.RemainingUOPCached)
	}
}

// ── Cutover ─────────────────────────────────────────────────────────────

// cutoverFixture is a process running fromStyle with three nodes: SWAP changes
// part at the cutover, KEEP runs the same part in both styles, and IDLE has no
// claim in either style (the changeover does not touch it) but holds a pile.
// Every node carries a pile and a bound bin with 50 in it.
type cutoverFixture struct {
	db                 *store.DB
	eng                *Engine
	mut                *uop.Mutator
	processID          int64
	fromStyle, toStyle int64
	swap, keep, idle   int64
	swapCore, keepCore string
	idleCore           string
}

func newCutoverFixture(t *testing.T, prefix string) *cutoverFixture {
	t.Helper()
	db := testEngineDB(t)
	f := &cutoverFixture{db: db}
	var err error
	f.processID, err = db.CreateProcess(prefix+"-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	f.fromStyle, err = db.CreateStyle(prefix+"-FROM", "", f.processID)
	testutil.MustNoErr(t, err, "create from-style")
	f.toStyle, err = db.CreateStyle(prefix+"-TO", "", f.processID)
	testutil.MustNoErr(t, err, "create to-style")
	testutil.MustNoErr(t, db.SetActiveStyle(f.processID, &f.fromStyle), "set active style")

	for i, n := range []struct {
		suffix, fromPart, toPart, pilePart string
		id                                 *int64
		core                               *string
	}{
		{"SWAP", "SYN-PART-1", "SYN-PART-2", "SYN-PART-1", &f.swap, &f.swapCore},
		{"KEEP", "SYN-PART-3", "SYN-PART-3", "SYN-PART-3", &f.keep, &f.keepCore},
		{"IDLE", "", "", "SYN-PART-4", &f.idle, &f.idleCore},
	} {
		name := prefix + "-" + n.suffix
		nodeID, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: f.processID, CoreNodeName: name, Code: n.suffix,
			Name: name, Sequence: i + 1, Enabled: true,
		})
		testutil.MustNoErr(t, err, "create node")
		*n.id, *n.core = nodeID, name
		_, err = db.EnsureProcessNodeRuntime(nodeID)
		testutil.MustNoErr(t, err, "ensure runtime")
		if n.fromPart != "" {
			fromClaimID, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{
				StyleID: f.fromStyle, CoreNodeName: name, Role: protocol.ClaimRoleConsume,
				SwapMode: protocol.SwapModeSimple, PayloadCode: n.fromPart, UOPCapacity: 100,
			})
			testutil.MustNoErr(t, err, "from claim")
			_, err = upsertClaimRetiredMode(db, processes.NodeClaimInput{
				StyleID: f.toStyle, CoreNodeName: name, Role: protocol.ClaimRoleConsume,
				SwapMode: protocol.SwapModeSimple, PayloadCode: n.toPart, UOPCapacity: 100,
			})
			testutil.MustNoErr(t, err, "to claim")
			bin := int64(700 + i)
			testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &fromClaimID, &bin, 50), "bind bin")
		}
		_, err = db.CaptureLinesideBucket(nodeID, n.pilePart, 10+i)
		testutil.MustNoErr(t, err, "pile")
	}

	f.eng = testEngine(t, db)
	f.mut = uop.New(db, "stn-test", db, db)
	f.eng.SetInventoryDeltaSink(f.mut)
	_, err = f.mut.ResendLevels() // Core holds the three active piles
	testutil.MustNoErr(t, err, "boot resend")
	ackOutbox(t, db)
	return f
}

func ackOutbox(t *testing.T, db *store.DB) {
	t.Helper()
	msgs, err := db.ListPendingOutbox(1000)
	testutil.MustNoErr(t, err, "list outbox")
	for _, m := range msgs {
		testutil.MustNoErr(t, db.AckOutbox(m.ID), "ack outbox")
	}
}

// cutover runs the changeover's cutover, with its gate satisfied directly as
// TestWiring_ABPairsAcrossStyles does: this pins what the cutover does to piles,
// not the changeover flow.
func (f *cutoverFixture) cutover(t *testing.T) {
	t.Helper()
	co, err := f.eng.StartProcessChangeover(f.processID, f.toStyle, "test", "pile pin")
	testutil.MustNoErr(t, err, "start changeover")
	tasks, err := f.db.ListChangeoverNodeTasks(co.ID)
	testutil.MustNoErr(t, err, "list tasks")
	for _, task := range tasks {
		for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
			if orderID == nil {
				continue
			}
			for _, s := range []string{string(orders.StatusSubmitted), string(orders.StatusAcknowledged),
				string(orders.StatusInTransit), string(orders.StatusDelivered), string(orders.StatusConfirmed)} {
				_ = f.db.UpdateOrderStatus(*orderID, s)
			}
		}
		testutil.MustNoErr(t, f.db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskReleased), "release task")
	}
	ackOutbox(t, f.db)
	testutil.MustNoErr(t, f.eng.CompleteProcessProductionCutover(f.processID), "cutover")
}

// assertStranded checks every node's pile went stranded with its qty and that
// the strand sent both levels for each: active 0, stranded N.
func (f *cutoverFixture) assertStranded(t *testing.T) {
	t.Helper()
	levels := levelsByKey(levelsInOutbox(t, f.db))
	for _, n := range []struct {
		node int64
		core string
		part string
		qty  int
	}{{f.swap, f.swapCore, "SYN-PART-1", 10}, {f.keep, f.keepCore, "SYN-PART-3", 11}, {f.idle, f.idleCore, "SYN-PART-4", 12}} {
		rows := pileRows(t, f.db, n.node)
		if len(rows) != 1 || rows[n.part+"/"+lineside.StateStranded].Qty != n.qty {
			t.Errorf("piles at %s = %+v, want only %s stranded with %d", n.core, rows, n.part, n.qty)
		}
		if q, ok := levels[n.core+"/"+n.part+"/active"]; !ok || q != 0 {
			t.Errorf("active level for %s/%s = %d (sent %v), want 0 sent", n.core, n.part, q, ok)
		}
		if q, ok := levels[n.core+"/"+n.part+"/stranded"]; !ok || q != n.qty {
			t.Errorf("stranded level for %s/%s = %d (sent %v), want %d sent", n.core, n.part, q, ok, n.qty)
		}
	}
}

// THE CUTOVER STRANDS EVERY PILE AT THE PROCESS'S NODES, the node the
// changeover does not touch included, and sends two levels for each (active
// 0, stranded N). After it, a to-style tick at KEEP, whose part runs in both
// styles, goes to the bin: the stranded pile never drains.
// Flipped under changes #2 and #3 (was
// TestCompleteCutover_LeavesEveryPileActiveAndDraining): the cutover used to
// leave every pile active and send nothing; KEEP's pile kept draining and
// SWAP's sat active and undrained, still counted by Core.
func TestCompleteCutover_StrandsEveryPileAndSendsBothLevels(t *testing.T) {
	t.Parallel()
	f := newCutoverFixture(t, "CUT-PILE")
	f.cutover(t)

	p, err := f.db.GetProcess(f.processID)
	testutil.MustNoErr(t, err, "read process")
	if p.ActiveStyleID == nil || *p.ActiveStyleID != f.toStyle {
		t.Fatalf("active style = %v, want %d: the cutover did not flip", p.ActiveStyleID, f.toStyle)
	}
	f.assertStranded(t)

	f.eng.handleCounterDelta(CounterDeltaEvent{ProcessID: f.processID, StyleID: f.toStyle, Delta: 3})
	if got := pileRows(t, f.db, f.keep)["SYN-PART-3/stranded"]; got.Qty != 11 {
		t.Errorf("KEEP pile after a to-style tick = %+v, want stranded 11: a stranded pile never drains", got)
	}
}

// THE ADMIN STYLE FLIP IS A CUTOVER TOO, and re-setting the style a process
// already runs is not a flip. A stranded pile does not revive when the style
// comes back: the from-style's tick at SWAP goes to the bin, and the next pull
// of that part is a new active pile beside the stranded one.
// New pin (change #2).
func TestSetProcessActiveStyle_StrandsEveryPileAndNothingRevives(t *testing.T) {
	t.Parallel()
	f := newCutoverFixture(t, "FLIP-PILE")

	testutil.MustNoErr(t, f.eng.SetProcessActiveStyle(f.processID, &f.fromStyle), "re-set the running style")
	if levels := levelsInOutbox(t, f.db); len(levels) != 0 {
		t.Errorf("re-setting the running style sent %+v, want nothing: not a flip", levels)
	}
	if got := pileRows(t, f.db, f.swap)["SYN-PART-1/active"]; got.Qty != 10 {
		t.Errorf("SWAP pile after the no-op = %+v, want active 10", got)
	}

	testutil.MustNoErr(t, f.eng.SetProcessActiveStyle(f.processID, &f.toStyle), "flip to the to-style")
	f.assertStranded(t)
	ackOutbox(t, f.db)

	// The style comes back. Nothing revives: there is nothing active to
	// strand, so no level goes out, and a from-style tick at SWAP drains no
	// pile and takes its 3 from the bin.
	testutil.MustNoErr(t, f.eng.SetProcessActiveStyle(f.processID, &f.fromStyle), "flip back")
	if levels := levelsInOutbox(t, f.db); len(levels) != 0 {
		t.Errorf("flipping back sent %+v, want nothing", levels)
	}
	f.eng.handleCounterDelta(CounterDeltaEvent{ProcessID: f.processID, StyleID: f.fromStyle, Delta: 3})
	if got := pileRows(t, f.db, f.swap)["SYN-PART-1/stranded"]; got.Qty != 10 {
		t.Errorf("SWAP stranded pile after the style came back = %+v, want 10 undrained", got)
	}
	rt, err := f.db.GetProcessNodeRuntime(f.swap)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.RemainingUOPCached != 47 {
		t.Errorf("SWAP bin = %d, want 47: the tick deducts from the bin", rt.RemainingUOPCached)
	}

	_, err = f.mut.CaptureToLineside(uop.CaptureEvent{
		NodeID: f.swap, CoreNodeName: f.swapCore, BinID: 700, PayloadCode: "SYN-PART-1", BinEpoch: 0,
		Disposition: uop.ReleaseDisposition{Mode: uop.DispositionCaptureLineside, LinesideCapture: map[string]int{"SYN-PART-1": 4}},
	})
	testutil.MustNoErr(t, err, "capture")
	rows := pileRows(t, f.db, f.swap)
	if rows["SYN-PART-1/active"].Qty != 4 || rows["SYN-PART-1/stranded"].Qty != 10 {
		t.Errorf("SWAP piles after the next pull = %+v, want active 4 beside stranded 10", rows)
	}
}

// A STRANDED PILE IS NOT IN THE REPORT. The seat's bucket term is the active
// pile's level as Core holds it, and after the strand that is 0.
// New pin (change #2).
func TestReportLinesideLevels_StrandedPileIsNotReported(t *testing.T) {
	t.Parallel()
	f := newCutoverFixture(t, "REP-PILE")
	testutil.MustNoErr(t, f.db.SetProcessNodeRuntimeLinesidePayload(f.keep, "SYN-PART-3", true, "delivery"), "name KEEP's carrier")

	f.eng.reportLinesideLevels()
	if got := keepBucketQty(t, f); got != "11" {
		t.Fatalf("KEEP bucket_qty before the strand = %s, want 11", got)
	}
	ackOutbox(t, f.db)

	testutil.MustNoErr(t, f.eng.SetProcessActiveStyle(f.processID, &f.toStyle), "flip")
	f.eng.reportLinesideLevels()
	if got := keepBucketQty(t, f); got != "" && got != "0" {
		t.Errorf("KEEP bucket_qty after the strand = %s, want 0: the stranded 11 does not count", got)
	}
}

func keepBucketQty(t *testing.T, f *cutoverFixture) string {
	t.Helper()
	for _, r := range reportedRaw(t, f.db) {
		if string(r["core_node_name"]) == `"`+f.keepCore+`"` {
			return string(r["bucket_qty"])
		}
	}
	t.Fatalf("no report row for %s", f.keepCore)
	return ""
}

// ── Admin ───────────────────────────────────────────────────────────────

func adminPileFixture(t *testing.T, prefix string, qty int) (*store.DB, *Engine, int64, int64) {
	t.Helper()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: prefix, PayloadCode: "SYN-PART-1", UOPCapacity: 100, InitialUOP: 100,
	})
	_, err := db.CaptureLinesideBucket(nodeID, "SYN-PART-1", qty)
	testutil.MustNoErr(t, err, "capture pile")
	return db, testEngine(t, db), nodeID, pileRows(t, db, nodeID)["SYN-PART-1/active"].ID
}

// Clear deletes the pile and sends its level, 0, flushed at once.
// Stays (Clear stays); the message became level 0 under change #1 (it was an
// operator_correction delta of -12).
func TestAdminClearLinesideBucket_DeletesPileAndSendsLevelZero(t *testing.T) {
	t.Parallel()
	db, eng, nodeID, bucketID := adminPileFixture(t, "ADM-CLEAR", 12)
	eng.SetInventoryDeltaSink(uop.New(db, "stn-test", db, db))

	testutil.MustNoErr(t, eng.AdminClearLinesideBucket(bucketID), "clear")

	if rows := pileRows(t, db, nodeID); len(rows) != 0 {
		t.Errorf("piles after Clear = %+v, want none", rows)
	}
	l := levelsInOutbox(t, db)
	if len(l) != 1 || l[0].Qty != 0 || l[0].State != protocol.LinesideBucketActive ||
		l[0].CoreNodeName != "ADM-CLEAR-NODE" || l[0].PayloadCode != "SYN-PART-1" {
		t.Errorf("levels = %+v, want one active level of 0 for ADM-CLEAR-NODE/SYN-PART-1", l)
	}
}

// With no inventory sink wired, Clear still deletes the pile: the delete is
// unconditional, and only the level has nowhere to go.
// Flipped under change #4 (was TestAdminAdjustLinesideBucket_
// ClearWithoutSinkLeavesThePile): the row write lived inside the sink's
// AdjustBucket, behind the `inventoryDelta != nil` gate, so the call returned
// nil, logged "cleared" and changed nothing.
func TestAdminClearLinesideBucket_WithoutSinkStillDeletesThePile(t *testing.T) {
	t.Parallel()
	db, eng, nodeID, bucketID := adminPileFixture(t, "ADM-NOSINK", 7)
	eng.SetInventoryDeltaSink(nil)

	testutil.MustNoErr(t, eng.AdminClearLinesideBucket(bucketID), "clear")

	if rows := pileRows(t, db, nodeID); len(rows) != 0 {
		t.Errorf("piles after a sinkless Clear = %+v, want none", rows)
	}
}

// A process delete takes its piles, and each one's level goes out as 0.
// New pin (the brief's U3: deleting a process deletes its piles; it used to be
// refused while a pile held parts).
func TestDeleteProcess_SendsLevelZeroForItsPiles(t *testing.T) {
	t.Parallel()
	f := newCutoverFixture(t, "DEL-PILE")
	_, err := f.db.StrandLinesidePiles(f.processID) // one stranded row among them
	testutil.MustNoErr(t, err, "strand")
	_, err = f.db.CaptureLinesideBucket(f.swap, "SYN-PART-1", 2)
	testutil.MustNoErr(t, err, "a new active pile")

	testutil.MustNoErr(t, f.eng.DeleteProcess(f.processID), "delete process")

	for _, n := range []int64{f.swap, f.keep, f.idle} {
		if rows := pileRows(t, f.db, n); len(rows) != 0 {
			t.Errorf("piles at node %d after the delete = %+v, want none", n, rows)
		}
	}
	levels := levelsByKey(levelsInOutbox(t, f.db))
	for _, k := range []string{
		f.swapCore + "/SYN-PART-1/active", f.swapCore + "/SYN-PART-1/stranded",
		f.keepCore + "/SYN-PART-3/stranded", f.idleCore + "/SYN-PART-4/stranded",
	} {
		if q, ok := levels[k]; !ok || q != 0 {
			t.Errorf("level %s = %d (sent %v), want 0 sent", k, q, ok)
		}
	}
}
