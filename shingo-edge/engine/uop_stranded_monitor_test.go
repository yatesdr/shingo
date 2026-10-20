package engine

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// feed drives a sequence of (pending, bound) samples through the detector and
// returns how many times it fired and whether it ended active.
func feed(t *testing.T, samples []struct {
	pending int
	bound   bool
}) (fires int, endActive bool) {
	t.Helper()
	st := &strandedNodeState{}
	base := time.Now()
	for i, s := range samples {
		fire, active := evalStrandedGrowth(st, s.pending, s.bound, base.Add(time.Duration(i)*time.Minute))
		if fire {
			fires++
		}
		endActive = active
	}
	return fires, endActive
}

func TestEvalStrandedGrowth_SustainedGrowthFiresOnce(t *testing.T) {
	t.Parallel()
	// strandedWindow rises => strandedWindow+1 increasing samples fire once, and
	// continued growth stays active without re-firing.
	fires, active := feed(t, []struct {
		pending int
		bound   bool
	}{
		{10, false}, {20, false}, {30, false}, {40, false}, {50, false}, {60, false},
	})
	if fires != 1 {
		t.Errorf("fires = %d, want 1 (one alarm per growth window)", fires)
	}
	if !active {
		t.Error("want still active while pending keeps climbing unbound")
	}
}

func TestEvalStrandedGrowth_IdleLineNeverAlarms(t *testing.T) {
	t.Parallel()
	// Flat pending = an idle line: ticks are not arriving, so it must never alarm
	// no matter how large or how long.
	fires, active := feed(t, []struct {
		pending int
		bound   bool
	}{
		{500, false}, {500, false}, {500, false}, {500, false}, {500, false},
	})
	if fires != 0 || active {
		t.Errorf("idle line alarmed: fires=%d active=%v, want 0/false (no fixed threshold, growth only)", fires, active)
	}
}

func TestEvalStrandedGrowth_BoundNeverAlarms(t *testing.T) {
	t.Parallel()
	// A bound node is not stranded — growth against a bound bin is normal drain.
	fires, active := feed(t, []struct {
		pending int
		bound   bool
	}{
		{10, true}, {20, true}, {30, true}, {40, true}, {50, true},
	})
	if fires != 0 || active {
		t.Errorf("bound node alarmed: fires=%d active=%v, want 0/false", fires, active)
	}
}

func TestEvalStrandedGrowth_BindClearsMidWindow(t *testing.T) {
	t.Parallel()
	// Growth toward an alarm, then a bin binds: the window resets, no alarm.
	fires, active := feed(t, []struct {
		pending int
		bound   bool
	}{
		{10, false}, {20, false}, {30, false}, // climbing, not yet fired
		{0, true}, // bound — pending cleared onto the bin, window resets
	})
	if fires != 0 || active {
		t.Errorf("bind mid-climb alarmed: fires=%d active=%v, want 0/false (binding is the fix)", fires, active)
	}
}

func TestEvalStrandedGrowth_StallReArmsOnRenewedGrowth(t *testing.T) {
	t.Parallel()
	// Fire once, go flat (alarm clears — idle), then a renewed sustained climb
	// fires a second window. Proves the per-window dedup and the re-arm.
	fires, active := feed(t, []struct {
		pending int
		bound   bool
	}{
		{10, false}, {20, false}, {30, false}, {40, false}, // fire #1
		{40, false}, {40, false}, // flat: clears
		{50, false}, {60, false}, {70, false}, {80, false}, // fire #2
	})
	if fires != 2 {
		t.Errorf("fires = %d, want 2 (one per growth window, re-armed after the flat break)", fires)
	}
	if !active {
		t.Error("want active at the end of the second climb")
	}
}

// TestStrandedMonitor_Evaluate_FiresNamedAlarm drives the live scan over a
// seeded consume node whose pending_uop_delta climbs while unbound, and asserts
// the exact operator sentence lands in the engine's alarm map + on the bus.
//
// Through tick(), not evaluate(): the claim and the runtime row are read for
// the whole node set at the top of the scan, so tick is where the reads and
// the decision meet. The fixture is one node, so a scan is that node.
func TestStrandedMonitor_Evaluate_FiresNamedAlarm(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "PARKED", PayloadCode: "PART-PK", UOPCapacity: 200, InitialUOP: 0,
	})
	// Unbound slot: the SNF3 staged-but-unbound window.
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil), "clear active bin")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")

	eng := testEngine(t, db)
	var got []UOPStrandedEvent
	eng.Events.SubscribeTypes(func(evt Event) {
		if a, ok := evt.Payload.(UOPStrandedEvent); ok {
			got = append(got, a)
		}
	}, EventUOPStranded)

	sm := &strandedMonitor{eng: eng, states: map[int64]*strandedNodeState{}}
	base := time.Now()
	// Climb pending across strandedWindow+2 scans while unbound.
	for i := 0; i < strandedWindow+2; i++ {
		testutil.MustNoErr(t, db.AddPendingUOPDelta(nodeID, 7), "add pending")
		sm.tick(base.Add(time.Duration(i) * time.Minute))
	}

	if len(got) != 1 {
		t.Fatalf("EventUOPStranded fired %d times, want exactly 1 per window: %+v", len(got), got)
	}
	detail := eng.StrandedAlarmDetail(node.CoreNodeName)
	if detail == "" {
		t.Fatal("StrandedAlarmDetail empty, want the tile sentence")
	}
	// A few minutes in, this is a swap in progress. It says what is happening
	// and asks for nothing.
	if !strings.HasPrefix(detail, "Binding in progress — UOP accumulating.") {
		t.Errorf("detail = %q, want it to open by naming what is happening", detail)
	}
	if strings.Contains(detail, "Record Count") {
		t.Errorf("detail = %q — a few minutes into a swap is the NORMAL case; "+
			"asking the operator to intervene here is what taught them the notice means nothing", detail)
	}
	// The elapsed time has to be a number that says something. Whole hours meant
	// every notice inside the first hour read "staged 0h".
	if strings.Contains(detail, "0h") {
		t.Errorf("detail = %q renders the wait as 0h; below an hour it must render minutes", detail)
	}
	if !strings.Contains(detail, "min") {
		t.Errorf("detail = %q, want the wait in minutes", detail)
	}
	if got := got[0]; got.NeedsAction {
		t.Error("event says needs_action for a swap that has been running four minutes")
	}

	// Binding clears the alarm.
	bound := int64(99)
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &bound), "bind a bin")
	sm.tick(base.Add(time.Duration(strandedWindow+3) * time.Minute))
	if d := eng.StrandedAlarmDetail(node.CoreNodeName); d != "" {
		t.Errorf("after bind, alarm detail = %q, want cleared", d)
	}
}

// TestStrandedDetail_AsksOnlyOnceTheWaitIsLong pins the two sentences and the
// line between them.
//
// The chip used to open with "Record Count on the bin tab" from the first
// second, which put an alarm face on the ordinary case — a carrier at the line
// during a swap, counts held until the bind lands, exactly what is supposed to
// happen. An alarm that is usually nothing is ignored when it is something. The
// call to action now waits until the gap is longer than a swap takes.
func TestStrandedDetail_AsksOnlyOnceTheWaitIsLong(t *testing.T) {
	t.Parallel()
	const ctaAfter = 30 * time.Minute
	for _, tc := range []struct {
		name    string
		unbound time.Duration
		wantCTA bool
		wantIn  string
	}{
		{"seconds in", 20 * time.Second, false, "just now"},
		{"one minute", time.Minute, false, "1 min"},
		{"mid swap", 12 * time.Minute, false, "12 min"},
		{"one minute short", 29 * time.Minute, false, "29 min"},
		{"at the limit", 30 * time.Minute, true, "30 min"},
		{"well past", 95 * time.Minute, true, "1h 35m"},
		{"exactly two hours", 2 * time.Hour, true, "2 hr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatStrandedDetail("BIN-7", tc.unbound, ctaAfter, "LINE1-IN")
			if asks := strings.Contains(got, "Record Count"); asks != tc.wantCTA {
				t.Errorf("after %s: asks for action = %v, want %v\n  %s", tc.unbound, asks, tc.wantCTA, got)
			}
			if !strings.Contains(got, tc.wantIn) {
				t.Errorf("after %s: want the wait rendered as %q\n  %s", tc.unbound, tc.wantIn, got)
			}
			if strings.Contains(got, "0h") {
				t.Errorf("after %s: rendered as 0h, which tells the operator nothing\n  %s", tc.unbound, got)
			}
			if !strings.HasPrefix(got, "Binding in progress — UOP accumulating.") {
				t.Errorf("after %s: want the sentence to open by naming what is happening\n  %s", tc.unbound, got)
			}
		})
	}
}

// ── The produce arm: the sweep of a standing pile with no carrier coming ──
//
// Each 60 s scan, and once at boot, an UNBOUND produce seat with a hold and no
// live order placing a carrier at the node has its hold dropped, guarded on
// the seat as the scan read it, with one hold_dropped (sweep) row. A bound
// seat is never touched. Unless a pin says otherwise the seat holds 3,000 of
// WIDGET-A (pack 100) with no bin bound: the ALN_010 shape.

// sweepSeat is p2Seat with the stranded monitor that scans it.
func sweepSeat(t *testing.T, held int) (eng *Engine, sink *fakeDeltaSink, sm *strandedMonitor, processID, nodeID, styleID, claimID int64) {
	t.Helper()
	eng, sink, processID, nodeID, styleID, claimID = p2Seat(t, held, "WIDGET-A")
	return eng, sink, &strandedMonitor{eng: eng, states: map[int64]*strandedNodeState{}}, processID, nodeID, styleID, claimID
}

var sweepDrop = protocol.HoldDropped{Reason: protocol.HoldDropSweep, Qty: 3000, PayloadCode: "WIDGET-A",
	Node: "PRODUCE-NODE", CalledBy: "edge:sweep"}

// TestProduceHoldSweep_BootDropsAStandingPileAndTheNextBindTakesOne — the
// ALN_010 shape and the rollout write-off: a bin-less pile of 3,000 with no
// live order is dropped by the boot pass with one sweep row, and the next bin
// bound takes the next stroke alone (+1, not +3,001).
func TestProduceHoldSweep_BootDropsAStandingPileAndTheNextBindTakesOne(t *testing.T) {
	t.Parallel()
	eng, sink, sm, processID, nodeID, styleID, _ := sweepSeat(t, 3000)

	sm.sweepAtBoot()
	assertSeat(t, eng, nodeID, 0, 0, "")
	assertDrops(t, eng.db, sweepDrop)

	bin := int64(7501)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "the next bin binds")
	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 1, 0, "")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 1 {
		t.Errorf("deltas = %+v, want one produce_tick of 1", sink.binCalls)
	}
}

// TestProduceHoldSweep_ClosesANoBinEpisodeOnABoundSeat — strokes logged on an
// unbound seat with nothing coming stay open through a scan; a bin binds and,
// with no tick after it, the next scan sends the episode as one no_bin row of
// 5. The bin's first tick then counts +1 and sends nothing more.
func TestProduceHoldSweep_ClosesANoBinEpisodeOnABoundSeat(t *testing.T) {
	t.Parallel()
	eng, sink, sm, processID, nodeID, styleID, _ := sweepSeat(t, 0)
	charSPTick(eng, processID, styleID, 2)
	charSPTick(eng, processID, styleID, 3)
	sm.tick(time.Now())
	assertDrops(t, eng.db)

	bin := int64(7602)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "the next bin binds")
	sm.tick(time.Now())
	noBin := protocol.HoldDropped{Reason: protocol.HoldDropNoBin, Qty: 5, PayloadCode: "WIDGET-A",
		Node: "PRODUCE-NODE", CalledBy: "edge:sweep"}
	assertDrops(t, eng.db, noBin)

	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 1, 0, "")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 1 {
		t.Errorf("deltas = %+v, want one produce_tick of 1", sink.binCalls)
	}
	assertDrops(t, eng.db, noBin)
}

// TestProduceHoldSweep_TheScanDropsAStandingPile — the same drop from the 60 s
// scan (strandedMonitor.tick), before its consume-only skip.
func TestProduceHoldSweep_TheScanDropsAStandingPile(t *testing.T) {
	t.Parallel()
	eng, _, sm, _, nodeID, _, _ := sweepSeat(t, 3000)
	sm.tick(time.Now())
	assertSeat(t, eng, nodeID, 0, 0, "")
	assertDrops(t, eng.db, sweepDrop)
}

// TestProduceHoldSweep_KeepsAPileACarrierIsComingFor — a live supply leg
// placing a carrier at the node: the hold is the next bin's, and stays.
func TestProduceHoldSweep_KeepsAPileACarrierIsComingFor(t *testing.T) {
	t.Parallel()
	eng, _, sm, _, nodeID, _, claimID := sweepSeat(t, 3000)
	p2Supply(t, eng, claimID)
	sm.sweepAtBoot()
	sm.tick(time.Now())
	assertSeat(t, eng, nodeID, 0, 3000, "WIDGET-A")
	assertDrops(t, eng.db)
}

// TestProduceHoldSweep_NeverTouchesABoundSeat — a bucket beside a bound bin (a
// bin at its pack waiting for a late REQUEST) is the next bin's whether or not
// an order exists yet; the sweep skips every bound seat.
func TestProduceHoldSweep_NeverTouchesABoundSeat(t *testing.T) {
	t.Parallel()
	eng, _, sm, _, nodeID, _, _ := sweepSeat(t, 30)
	bin := int64(7502)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "a bin at the seat")
	sm.sweepAtBoot()
	sm.tick(time.Now())
	assertSeat(t, eng, nodeID, 0, 30, "WIDGET-A")
	assertDrops(t, eng.db)
}

// TestProduceHoldSweep_ASeatChangedSinceTheScanReadItRefusesTheDrop — the scan
// read the seat unbound with 3,000 held; a bin binds before the drop takes the
// count lock. The drop names the seat as read, so it is refused: no change, no
// row.
func TestProduceHoldSweep_ASeatChangedSinceTheScanReadItRefusesTheDrop(t *testing.T) {
	t.Parallel()
	eng, _, _, _, nodeID, _, claimID := sweepSeat(t, 3000)
	node, err := eng.db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read node")
	stale := runtimeOf(t, eng, nodeID)
	bin := int64(7503)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "a bin binds after the read")

	eng.sweepProduceHold(node, p2Claim(t, eng, claimID), stale)
	assertSeat(t, eng, nodeID, 0, 3000, "WIDGET-A")
	assertDrops(t, eng.db)
}

// TestProduceHoldSweep_AnUnreadableOrderKeepsThePile — a live leg whose release
// facts do not parse may be placing a carrier here: no drop.
func TestProduceHoldSweep_AnUnreadableOrderKeepsThePile(t *testing.T) {
	t.Parallel()
	eng, _, sm, _, nodeID, _, claimID := sweepSeat(t, 3000)
	id := p2Supply(t, eng, claimID)
	_, err := eng.db.DB.Exec(`UPDATE orders SET release_facts='{"places":' WHERE id=?`, id)
	testutil.MustNoErr(t, err, "corrupt the facts")
	sm.sweepAtBoot()
	assertSeat(t, eng, nodeID, 0, 3000, "WIDGET-A")
	assertDrops(t, eng.db)
}
