package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
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
// window, the A/B fallthrough and unbound-tick drains, the cutover, and the
// admin Clear and qty edit. Pinned before the change; each test names the
// expected change it flips under, or says it stays.

// bucketDeltasInOutbox decodes every pending LinesideBucketDelta.
func bucketDeltasInOutbox(t *testing.T, db *store.DB) []protocol.LinesideBucketDelta {
	t.Helper()
	msgs, err := db.ListPendingOutbox(1000)
	testutil.MustNoErr(t, err, "list outbox")
	var out []protocol.LinesideBucketDelta
	for _, m := range msgs {
		if m.MsgType != string(protocol.SubjectLinesideBucketDelta) {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "decode envelope")
		var data protocol.Data
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &data), "decode data")
		var d protocol.LinesideBucketDelta
		testutil.MustNoErr(t, json.Unmarshal(data.Body, &d), "decode bucket delta")
		out = append(out, d)
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

// ── Drain ───────────────────────────────────────────────────────────────

// THE CHANGEOVER WINDOW (the 2026-05-19 fix). The release captured the pulled
// parts stamped with the TO-style's id, but the process still runs the
// from-style and its ticks carry the from-style. The pile drains anyway (Drain
// ignores style), and the bucket delta is attributed to the pile's own stamp.
// Stays: the pile drains lineside-first until cutover. The StyleID on the
// bucket message goes under change #1.
func TestCounterDelta_ChangeoverWindowDrainsAPileStampedWithTheToStyle(t *testing.T) {
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
	_, err = db.CaptureLinesideBucket(nodeID, "", toStyleID, "SYN-PART-1", 10)
	testutil.MustNoErr(t, err, "capture under the to-style")

	eng := testEngine(t, db)
	sink := &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)
	eng.handleCounterDelta(CounterDeltaEvent{ProcessID: processID, StyleID: fromStyleID, Delta: 3})

	if p := pileRows(t, db, nodeID)["SYN-PART-1/active"]; p.Qty != 7 {
		t.Errorf("pile = %+v, want active 7 (10 - 3): the from-style tick drains the to-style-stamped pile", p)
	}
	if len(sink.bucketCalls) != 1 || sink.bucketCalls[0].Delta != -3 || sink.bucketCalls[0].StyleID != toStyleID {
		t.Errorf("bucket calls = %+v, want one -3 attributed to the pile's stamp (style %d)", sink.bucketCalls, toStyleID)
	}
	if len(sink.binCalls) != 0 {
		t.Errorf("bin calls = %+v, want none: the pile covered the whole tick", sink.binCalls)
	}
}

// A/B FALLTHROUGH. Neither side pulls; the first parked side takes the tick,
// and it drains that side's pile first: consume_drain on the pile, the remainder
// as ab_fallthrough on the bin.
// Stays (the drain); the bucket message's shape changes under change #1.
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
	_, err := db.CaptureLinesideBucket(nodeA, "", f.StyleID, "SYN-PART-A", 2)
	testutil.MustNoErr(t, err, "capture pile at A")

	eng := testEngine(t, db)
	sink := &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)
	eng.handleCounterDelta(CounterDeltaEvent{ProcessID: f.ProcessID, StyleID: f.StyleID, Delta: 5})

	if rows := pileRows(t, db, nodeA); len(rows) != 0 {
		t.Errorf("piles at A = %+v, want none: 2 against a tick of 5 drains to zero and the row goes", rows)
	}
	if len(sink.bucketCalls) != 1 || sink.bucketCalls[0].NodeID != nodeA || sink.bucketCalls[0].Delta != -2 ||
		sink.bucketCalls[0].Reason != protocol.ReasonConsumeDrain {
		t.Errorf("bucket calls = %+v, want one consume_drain -2 at A", sink.bucketCalls)
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
// tick drains the pile and sends its consume_drain at once; only the remainder
// is held in pending_uop_delta, and no bin delta goes out.
// Stays.
func TestCounterDelta_UnboundTickDrainsPileAndHoldsOnlyTheRemainder(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "GAP-PILE", PayloadCode: "SYN-PART-1", UOPCapacity: 100, InitialUOP: 100,
	})
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil), "open the gap")
	_, err := db.CaptureLinesideBucket(nodeID, "", styleID, "SYN-PART-1", 2)
	testutil.MustNoErr(t, err, "capture pile")

	eng := testEngine(t, db)
	sink := &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)
	eng.handleCounterDelta(CounterDeltaEvent{ProcessID: processID, StyleID: styleID, Delta: 5})

	if len(sink.bucketCalls) != 1 || sink.bucketCalls[0].Delta != -2 {
		t.Errorf("bucket calls = %+v, want one -2: the pile drains regardless of the bin", sink.bucketCalls)
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

// completeCutover (the changeover's active-style flip) does not touch piles:
// every pile at the process's nodes stays active with its qty and sends
// nothing. After the flip, a pile of a part the new style still consumes keeps
// draining, and a pile of a part it no longer consumes stays active and undrained.
// Flips under change #2 (and #3 for the undrained pile): the cutover strands
// every pile at the process's nodes, including the untouched node, sends a level
// for each, and the next ticks go to the bin.
func TestCompleteCutover_LeavesEveryPileActiveAndDraining(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, err := db.CreateProcess("CUT-PILE-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	fromStyleID, err := db.CreateStyle("CUT-PILE-FROM", "", processID)
	testutil.MustNoErr(t, err, "create from-style")
	toStyleID, err := db.CreateStyle("CUT-PILE-TO", "", processID)
	testutil.MustNoErr(t, err, "create to-style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	// SWAP changes part at the cutover; KEEP runs the same part in both styles.
	nodes := map[string]int64{}
	for i, n := range []struct{ name, fromPart, toPart string }{
		{"CUT-PILE-SWAP", "SYN-PART-1", "SYN-PART-2"},
		{"CUT-PILE-KEEP", "SYN-PART-3", "SYN-PART-3"},
	} {
		nodeID, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: processID, CoreNodeName: n.name, Code: n.name[len(n.name)-4:],
			Name: n.name, Sequence: i + 1, Enabled: true,
		})
		testutil.MustNoErr(t, err, "create node")
		nodes[n.name] = nodeID
		fromClaimID, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{
			StyleID: fromStyleID, CoreNodeName: n.name, Role: protocol.ClaimRoleConsume,
			SwapMode: protocol.SwapModeSimple, PayloadCode: n.fromPart, UOPCapacity: 100,
		})
		testutil.MustNoErr(t, err, "from claim")
		_, err = upsertClaimRetiredMode(db, processes.NodeClaimInput{
			StyleID: toStyleID, CoreNodeName: n.name, Role: protocol.ClaimRoleConsume,
			SwapMode: protocol.SwapModeSimple, PayloadCode: n.toPart, UOPCapacity: 100,
		})
		testutil.MustNoErr(t, err, "to claim")
		_, err = db.EnsureProcessNodeRuntime(nodeID)
		testutil.MustNoErr(t, err, "ensure runtime")
		bin := int64(700 + i)
		testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &fromClaimID, &bin, 50), "bind bin")
	}
	swap, keep := nodes["CUT-PILE-SWAP"], nodes["CUT-PILE-KEEP"]
	_, err = db.CaptureLinesideBucket(swap, "", fromStyleID, "SYN-PART-1", 10)
	testutil.MustNoErr(t, err, "pile at SWAP")
	_, err = db.CaptureLinesideBucket(keep, "", fromStyleID, "SYN-PART-3", 8)
	testutil.MustNoErr(t, err, "pile at KEEP")

	eng := testEngine(t, db)
	sink := &fakeDeltaSink{db: db}
	eng.SetInventoryDeltaSink(sink)
	co, err := eng.StartProcessChangeover(processID, toStyleID, "test", "pile pin")
	testutil.MustNoErr(t, err, "start changeover")
	// Satisfy the cutover gate directly, as TestWiring_ABPairsAcrossStyles does:
	// this pins what the cutover does to piles, not the changeover flow.
	tasks, err := db.ListChangeoverNodeTasks(co.ID)
	testutil.MustNoErr(t, err, "list tasks")
	for _, task := range tasks {
		for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
			if orderID == nil {
				continue
			}
			for _, s := range []string{string(orders.StatusSubmitted), string(orders.StatusAcknowledged),
				string(orders.StatusInTransit), string(orders.StatusDelivered), string(orders.StatusConfirmed)} {
				_ = db.UpdateOrderStatus(*orderID, s)
			}
		}
		testutil.MustNoErr(t, db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskReleased), "release task")
	}
	sink.bucketCalls = nil
	testutil.MustNoErr(t, eng.CompleteProcessProductionCutover(processID), "cutover")

	p, err := db.GetProcess(processID)
	testutil.MustNoErr(t, err, "read process")
	if p.ActiveStyleID == nil || *p.ActiveStyleID != toStyleID {
		t.Fatalf("active style = %v, want %d: the cutover did not flip", p.ActiveStyleID, toStyleID)
	}
	if got := pileRows(t, db, swap); len(got) != 1 || got["SYN-PART-1/active"].Qty != 10 {
		t.Errorf("piles at SWAP after cutover = %+v, want SYN-PART-1 still active with 10", got)
	}
	if got := pileRows(t, db, keep); len(got) != 1 || got["SYN-PART-3/active"].Qty != 8 {
		t.Errorf("piles at KEEP after cutover = %+v, want SYN-PART-3 still active with 8", got)
	}
	if len(sink.bucketCalls) != 0 {
		t.Errorf("bucket calls during cutover = %+v, want none", sink.bucketCalls)
	}

	// The first to-style tick: KEEP's pile drains; SWAP's pile is of a part the
	// to-style does not consume, so SWAP's tick goes to its bin and the pile sits.
	eng.handleCounterDelta(CounterDeltaEvent{ProcessID: processID, StyleID: toStyleID, Delta: 3})
	if got := pileRows(t, db, keep)["SYN-PART-3/active"]; got.Qty != 5 {
		t.Errorf("KEEP pile after a to-style tick = %+v, want active 5: it keeps draining", got)
	}
	if got := pileRows(t, db, swap)["SYN-PART-1/active"]; got.Qty != 10 {
		t.Errorf("SWAP pile after a to-style tick = %+v, want active 10, undrained", got)
	}
}

// ── Admin ───────────────────────────────────────────────────────────────

func adminPileFixture(t *testing.T, prefix string, qty int) (*store.DB, *Engine, int64, int64) {
	t.Helper()
	db := testEngineDB(t)
	_, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: prefix, PayloadCode: "SYN-PART-1", UOPCapacity: 100, InitialUOP: 100,
	})
	b, err := db.CaptureLinesideBucket(nodeID, "", styleID, "SYN-PART-1", qty)
	testutil.MustNoErr(t, err, "capture pile")
	return db, testEngine(t, db), nodeID, b.ID
}

// Clear deletes the pile and sends an operator_correction delta of -qty, flushed
// at once.
// Stays (Clear stays); the message becomes level 0 under change #1.
func TestAdminAdjustLinesideBucket_ClearDeletesPileAndSendsNegativeDelta(t *testing.T) {
	t.Parallel()
	db, eng, nodeID, bucketID := adminPileFixture(t, "ADM-CLEAR", 12)
	eng.SetInventoryDeltaSink(uop.New(db, "stn-test", db, db, db))

	testutil.MustNoErr(t, eng.AdminAdjustLinesideBucket(bucketID, 0, true), "clear")

	if rows := pileRows(t, db, nodeID); len(rows) != 0 {
		t.Errorf("piles after Clear = %+v, want none", rows)
	}
	d := bucketDeltasInOutbox(t, db)
	if len(d) != 1 || d[0].Delta != -12 || d[0].Reason != protocol.ReasonOperatorCorrectionBucket ||
		d[0].CoreNodeName != "ADM-CLEAR-NODE" || d[0].PayloadCode != "SYN-PART-1" {
		t.Errorf("bucket deltas = %+v, want one operator_correction -12 for ADM-CLEAR-NODE/SYN-PART-1", d)
	}
}

// The qty edit sets the pile to an exact qty and sends the difference; upward is
// allowed, which mints parts no bin paid for.
// Flips under change #4: the qty edit is deleted.
func TestAdminAdjustLinesideBucket_EditUpwardSendsPositiveDelta(t *testing.T) {
	t.Parallel()
	db, eng, nodeID, bucketID := adminPileFixture(t, "ADM-EDIT", 5)
	eng.SetInventoryDeltaSink(uop.New(db, "stn-test", db, db, db))

	testutil.MustNoErr(t, eng.AdminAdjustLinesideBucket(bucketID, 20, false), "edit")

	if p := pileRows(t, db, nodeID)["SYN-PART-1/active"]; p.Qty != 20 {
		t.Errorf("pile after edit = %+v, want active 20", p)
	}
	d := bucketDeltasInOutbox(t, db)
	if len(d) != 1 || d[0].Delta != 15 || d[0].Reason != protocol.ReasonOperatorCorrectionBucket {
		t.Errorf("bucket deltas = %+v, want one operator_correction +15", d)
	}
}

// With no inventory sink wired, Clear changes nothing: the row write lives
// inside the sink's AdjustBucket, behind the `inventoryDelta != nil` gate, yet
// the call returns nil and logs "cleared".
// Flips under change #4: Clear becomes unconditional (the gate is dropped).
func TestAdminAdjustLinesideBucket_ClearWithoutSinkLeavesThePile(t *testing.T) {
	t.Parallel()
	db, eng, _, bucketID := adminPileFixture(t, "ADM-NOSINK", 7)
	eng.SetInventoryDeltaSink(nil)

	testutil.MustNoErr(t, eng.AdminAdjustLinesideBucket(bucketID, 0, true), "clear")

	b, err := db.GetLinesideBucket(bucketID)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("pile deleted; today the nil-sink Clear is a silent no-op")
	}
	testutil.MustNoErr(t, err, "read pile")
	if b.Qty != 7 {
		t.Errorf("pile qty = %d, want 7 (untouched)", b.Qty)
	}
}
