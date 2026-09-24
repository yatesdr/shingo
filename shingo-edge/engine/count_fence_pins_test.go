package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/uop"
)

// The Edge half of the record-count fence (SYNTH-round2 S7, citrine-kestrel
// §8 S4). Core writes a counted number N absolutely and says where in this
// station's stream it took it: AsOfNet, the running net Core had applied, and
// AsOfSeq, the last seq. The station rebases:
//
//	remaining = N + (flushed_net - AsOfNet) + unflushed
//
// so every window Core had not yet applied when it wrote N is subtracted on
// both sides, and neither side overwrites the other.

// fenceFixture binds bin 4401 at epoch 3 on a fresh node and wires the real
// accumulator (never started, so only an explicit Flush sends). Returns the
// engine, the node id, the accumulator and the bin id.
func fenceFixture(t *testing.T, coreNode string) (*Engine, int64, *uop.Mutator, int64) {
	t.Helper()
	eng := newCoverageEngine(t)
	nodeID, binID := boundNodeFixture(t, eng, coreNode, 4401, 3)
	acc := uop.New(eng.db, eng.cfg.StationID(), eng.db, eng.db, eng.db)
	eng.SetInventoryDeltaSink(acc)
	return eng, nodeID, acc, binID
}

func runtimeCount(t *testing.T, eng *Engine, nodeID int64) int {
	t.Helper()
	rt, err := eng.db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	return rt.RemainingUOPCached
}

func i64(v int64) *int64 { return &v }

// fenced builds the adjustment Core sends for a count of n taken when it had
// applied this station's stream up to (seq, net).
func fenced(eng *Engine, binID int64, node string, n int, net, seq int64) protocol.UOPAdjustment {
	return protocol.UOPAdjustment{
		BinID: binID, CoreNodeName: node, NewRemaining: n, Epoch: 3, Actor: "operator-under-test",
		AsOfNet: i64(net), AsOfSeq: i64(seq), AsOfStation: eng.cfg.StationID(),
	}
}

// TestP0f_EdgeRebasesOnCoresCursor is the inverted
// TestPin_P0f_EdgeTakesCoresNumberOverItsInFlightTicks.
//
// The station has flushed -10 (seq 1, applied at Core) and -5 (seq 2, in
// flight) when 50 is counted at Core. Core reads 45 once the -5 lands. The
// adjustment says Core had applied net -10 at seq 1; the station's flushed net
// is -15, so it writes 50 + (-15 - -10) + 0 = 45. Both sides read 45: the -5
// is subtracted on both, which is low if the counter already saw those parts
// gone (citrine: the safe direction).
func TestP0f_EdgeRebasesOnCoresCursor(t *testing.T) {
	t.Parallel()
	eng, nodeID, acc, binID := fenceFixture(t, "FENCE-P0F")

	acc.RecordBin(binID, "PART-A", -10, protocol.ReasonConsumeTick, 3)
	acc.Flush()
	acc.RecordBin(binID, "PART-A", -5, protocol.ReasonConsumeTick, 3)
	acc.Flush()

	eng.HandleUOPAdjustment(fenced(eng, binID, "FENCE-P0F", 50, -10, 1))

	if got := runtimeCount(t, eng, nodeID); got != 45 {
		t.Errorf("Edge count = %d, want 45 (= Core after the in-flight -5)", got)
	}
}

// TestFence_UnflushedTicksStayOnTheEdge: a tick recorded but not yet flushed
// is in neither Core's number nor the station's flushed net, so the rebase
// adds it back.
func TestFence_UnflushedTicksStayOnTheEdge(t *testing.T) {
	t.Parallel()
	eng, nodeID, acc, binID := fenceFixture(t, "FENCE-UNFLUSHED")

	acc.RecordBin(binID, "PART-A", -10, protocol.ReasonConsumeTick, 3)
	acc.Flush()
	acc.RecordBin(binID, "PART-A", -2, protocol.ReasonConsumeTick, 3) // not flushed

	eng.HandleUOPAdjustment(fenced(eng, binID, "FENCE-UNFLUSHED", 50, -10, 1))

	if got := runtimeCount(t, eng, nodeID); got != 48 {
		t.Errorf("Edge count = %d, want 48 (50, plus the -2 Core has not seen yet)", got)
	}
}

// TestFence_OlderAdjustmentIsIgnored is the inverted
// TestPin_OlderAdjustmentArrivingLateOverwritesTheNewer. The count of 40 was
// taken at seq 5; the count of 60 at seq 3 is delivered after it. The runtime
// row records the seq the count it holds was taken at, and refuses an older
// one for the same (bin, epoch).
func TestFence_OlderAdjustmentIsIgnored(t *testing.T) {
	t.Parallel()
	eng, nodeID, _, binID := fenceFixture(t, "FENCE-ORDER")

	eng.HandleUOPAdjustment(fenced(eng, binID, "FENCE-ORDER", 40, 0, 5))
	eng.HandleUOPAdjustment(fenced(eng, binID, "FENCE-ORDER", 60, 0, 3))

	if got := runtimeCount(t, eng, nodeID); got != 40 {
		t.Errorf("Edge count = %d, want 40 (the older count must not overwrite the newer)", got)
	}
	// The same seq again (a redelivery, or a second count with nothing
	// applied in between) is not older, and lands.
	eng.HandleUOPAdjustment(fenced(eng, binID, "FENCE-ORDER", 38, 0, 5))
	if got := runtimeCount(t, eng, nodeID); got != 38 {
		t.Errorf("Edge count = %d, want 38 (an equal seq is not older)", got)
	}
}

// TestFence_OldCoreKeepsTheAbsoluteWrite: no AsOfNet (a Core built before the
// fence, or a count with no anchored cursor) is today's behaviour.
func TestFence_OldCoreKeepsTheAbsoluteWrite(t *testing.T) {
	t.Parallel()
	eng, nodeID, acc, binID := fenceFixture(t, "FENCE-OLD")
	acc.RecordBin(binID, "PART-A", -10, protocol.ReasonConsumeTick, 3)
	acc.Flush()

	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID: binID, CoreNodeName: "FENCE-OLD", NewRemaining: 50, Epoch: 3, Actor: "operator-under-test",
	})
	if got := runtimeCount(t, eng, nodeID); got != 50 {
		t.Errorf("Edge count = %d, want 50 (no fence: absolute write)", got)
	}
}

// TestFence_AnotherStationsCursorIsNotRebasedOn: the fence measures one
// station's stream. A station it does not name takes the absolute number.
func TestFence_AnotherStationsCursorIsNotRebasedOn(t *testing.T) {
	t.Parallel()
	eng, nodeID, acc, binID := fenceFixture(t, "FENCE-FOREIGN")
	acc.RecordBin(binID, "PART-A", -10, protocol.ReasonConsumeTick, 3)
	acc.Flush()

	adj := fenced(eng, binID, "FENCE-FOREIGN", 50, -99, 7)
	adj.AsOfStation = "some-other-station"
	eng.HandleUOPAdjustment(adj)
	if got := runtimeCount(t, eng, nodeID); got != 50 {
		t.Errorf("Edge count = %d, want 50 (a fence for another station's stream is not ours)", got)
	}
}

// TestFence_EmptySlotGuardStillHolds: lane T's guard is not weakened. A fenced
// correction for a carrier that has left an empty slot at an older stamp does
// not rebind it.
func TestFence_EmptySlotGuardStillHolds(t *testing.T) {
	t.Parallel()
	eng, nodeID, _, binID := fenceFixture(t, "FENCE-EMPTY")
	rt, err := eng.db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	b := binID
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, rt.ActiveClaimID, &b, 5, 25), "move to 5")
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeWithBin(nodeID, rt.ActiveClaimID, nil, 0), "empty the slot")

	eng.HandleUOPAdjustment(fenced(eng, binID, "FENCE-EMPTY", 50, 0, 1)) // epoch 3 < 5

	after, err := eng.db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if after.ActiveBinID != nil {
		t.Errorf("active bin = %v, want nil: a departed carrier's older stamp must not rebind", *after.ActiveBinID)
	}
}

// TestLineCount_TicksAfterTheReplyAreNotLost: the count taken at the line.
// Core replies with the fence and broadcasts the same adjustment. The station
// consumes 3 parts after the reply; when the broadcast arrives it rebases, so
// those 3 stay subtracted rather than being overwritten by Core's number.
func TestLineCount_TicksAfterTheReplyAreNotLost(t *testing.T) {
	t.Parallel()
	eng, nodeID, acc, binID := fenceFixture(t, "FENCE-LINE")
	acc.RecordBin(binID, "PART-A", -10, protocol.ReasonConsumeTick, 3)
	acc.Flush() // seq 1, net -10, applied at Core

	station := eng.cfg.StationID()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "bin_id": binID, "expected": 15, "uop_remaining": 50,
			"delta_epoch": 3, "as_of_net": -10, "as_of_seq": 1, "as_of_station": station,
		})
	}))
	defer srv.Close()
	eng.coreClient = NewCoreClient(srv.URL)

	testutil.MustNoErr(t, eng.RecordBinCount(nodeID, 50, "line-operator"), "RecordBinCount")
	if got := runtimeCount(t, eng, nodeID); got != 50 {
		t.Fatalf("after the reply = %d, want 50", got)
	}

	// Three parts consumed after the reply: the runtime moves, the
	// accumulator records them, and a flush sends them.
	testutil.MustNoErr(t, eng.db.UpdateProcessNodeUOP(nodeID, 47), "tick")
	acc.RecordBin(binID, "PART-A", -3, protocol.ReasonConsumeTick, 3)
	acc.Flush()

	eng.HandleUOPAdjustment(fenced(eng, binID, "FENCE-LINE", 50, -10, 1))
	if got := runtimeCount(t, eng, nodeID); got != 47 {
		t.Errorf("after the broadcast = %d, want 47: the 3 parts consumed after the reply must survive it", got)
	}
}

// TestFence_Statements counts what the fence costs on the Pi. An unfenced
// count at a bound slot is 3 statements (node, runtime, the write). A fenced
// one adds the flushed-net SELECT and records the seq in the same write: 4.
// A refused (older) fenced count is also 4: the refusal is the write's WHERE.
func TestFence_Statements(t *testing.T) {
	t.Parallel()
	eng, counter := newCountingCoverageEngine(t)
	nodeID, binID := boundNodeFixture(t, eng, "FENCE-COST", 4401, 3)
	acc := uop.New(eng.db, eng.cfg.StationID(), eng.db, eng.db, eng.db)
	eng.SetInventoryDeltaSink(acc)
	acc.RecordBin(binID, "PART-A", -10, protocol.ReasonConsumeTick, 3)
	acc.Flush()

	counter.Reset()
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{BinID: binID, CoreNodeName: "FENCE-COST", NewRemaining: 50, Epoch: 3, Actor: "op"})
	if got := counter.Count(); got != 3 {
		t.Errorf("unfenced count issued %d statements, want 3", got)
	}

	counter.Reset()
	eng.HandleUOPAdjustment(fenced(eng, binID, "FENCE-COST", 50, -10, 5))
	if got := counter.Count(); got != 4 {
		t.Errorf("fenced count issued %d statements, want 4 (one SELECT for the flushed net)", got)
	}
	if got := runtimeCount(t, eng, nodeID); got != 50 {
		t.Errorf("fenced count = %d, want 50", got)
	}

	counter.Reset()
	eng.HandleUOPAdjustment(fenced(eng, binID, "FENCE-COST", 70, -10, 4))
	if got := counter.Count(); got != 4 {
		t.Errorf("refused fenced count issued %d statements, want 4", got)
	}
	if got := runtimeCount(t, eng, nodeID); got != 50 {
		t.Errorf("after an older fenced count = %d, want 50", got)
	}
}

// TestFence_AdjustmentsRacingTicksAndFlushesKeepTheInvariant: fenced counts
// arrive while the PLC ticks and the accumulator flushes. Every tick moves the
// runtime count and the station's net-plus-pending by the same amount, so after
// a fenced count of N as of net A the difference runtime - (net + pending)
// must be N - A, whatever interleaving happened. A rebase that read the net
// and the unflushed counts across a flush, or across a tick's runtime write
// and its record, would be off by that window.
func TestFence_AdjustmentsRacingTicksAndFlushesKeepTheInvariant(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	mut := uop.New(db, "stn-test", db, db, db)
	eng.SetInventoryDeltaSink(mut)
	processID, nodeID, _, fromClaimID, _ := seedDirectChangeover(t, db)
	claim, err := db.GetStyleNodeClaim(fromClaimID)
	testutil.MustNoErr(t, err, "read claim")
	const binID, epoch = int64(15), int64(1)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeForDeliveredBin(nodeID, &fromClaimID, binID, epoch, 7032), "bind carrier")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read node")

	const counted, asOfNet = 5000, int64(0)
	adj := protocol.UOPAdjustment{
		BinID: binID, CoreNodeName: node.CoreNodeName, NewRemaining: counted, Epoch: epoch,
		Actor: "operator-under-test", AsOfNet: i64(asOfNet), AsOfSeq: i64(0), AsOfStation: eng.cfg.StationID(),
	}
	eng.HandleUOPAdjustment(adj)

	const ticks = 200
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < ticks; i++ {
			eng.handleCounterDelta(CounterDeltaEvent{ProcessID: processID, StyleID: claim.StyleID, Delta: 1})
			if i%7 == 0 {
				mut.Flush()
			}
		}
	}()
	// The invariant, read as one instant: under countMu (no tick between its
	// runtime write and its record) and the flush lock (no window between the
	// net and the pending snapshot).
	gap := func() int64 {
		eng.countMu.Lock()
		defer eng.countMu.Unlock()
		var g int64
		testutil.MustNoErr(t, mut.WithPending(func(p uop.Pending) error {
			n, err := db.InventoryDeltaNet(protocol.InvDeltaScopeBin, "15", epoch)
			if err != nil {
				return err
			}
			rt, err := db.GetProcessNodeRuntime(nodeID)
			if err != nil {
				return err
			}
			g = int64(rt.RemainingUOPCached) - (n + int64(p.Bin(binID, epoch)))
			return nil
		}), "read the invariant")
		return g
	}
	adjustments, broken := 0, 0
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		eng.HandleUOPAdjustment(adj)
		adjustments++
		if g := gap(); g != counted-asOfNet {
			broken++
			if broken <= 3 {
				t.Errorf("after fenced count %d: runtime - (net + pending) = %d, want %d", adjustments, g, counted-asOfNet)
			}
		}
	}
	mut.Flush()

	net, err := db.InventoryDeltaNet(protocol.InvDeltaScopeBin, "15", epoch)
	testutil.MustNoErr(t, err, "read net")
	if net == 0 {
		t.Fatal("no net flushed: the ticks did not reach the bin, so the race was not exercised")
	}
	got := runtimeCount(t, eng, nodeID)
	if want := counted + int(net-asOfNet); got != want {
		t.Errorf("after %d fenced counts racing %d ticks: runtime = %d, want %d (counted %d + net %d)",
			adjustments, ticks, got, want, counted, net)
	}
	t.Logf("%d fenced counts raced %d ticks; net %d", adjustments, ticks, net)
}
