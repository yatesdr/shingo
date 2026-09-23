//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/fleet/simulator"
	"shingocore/store/demands"
	"shingocore/store/payloads"
)

// episodeBinding is the binding under test, plus its catalog payload so
// expected_orders has a real denominator.
func episodeBinding(t *testing.T, eng *Engine, payload string, capacity int) thresholdEntry {
	t.Helper()
	if capacity > 0 {
		if err := eng.db.CreatePayload(&payloads.Payload{Code: payload, UOPCapacity: capacity}); err != nil {
			t.Fatalf("create payload: %v", err)
		}
	}
	return thresholdEntry{
		stationID: "PLANT.LINE1", coreNodeName: "SLN_002", payloadCode: payload, threshold: 100,
	}
}

// A LEVEL IS NOT AN EDGE, and this is the whole reason the grain exists.
//
// checkBindings runs on every incoming delta, and "total < threshold" stays
// true for as long as it is true. Minting per evaluation would produce an id
// per ORDER — which is exactly how 2026-07-21 rendered as hundreds of
// unrelated firings instead of one demand that cost 484 orders.
func TestThresholdEpisode_OneEpisodeAcrossManyEvaluations(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	m, sink := loggingMonitor(t, db)
	eng := m.eng
	b := episodeBinding(t, eng, "PANEL-EP1", 18)

	for i := 0; i < 5; i++ {
		m.checkBindings([]thresholdEntry{b}, 40, "below_threshold", false)
	}
	// NO MINT WAS EVEN ATTEMPTED after the first. The row count below cannot see
	// this: the partial unique index rejects a second INSERT, and the monitor
	// keeps the first id either way. What only this line sees is whether the
	// "already below" check still answers before the write, rather than the
	// index answering after it on every delta.
	if n := sink.countContaining("open demand episode key="); n != 0 {
		t.Errorf("%d failed mint(s) across five evaluations of one open demand, want 0 — the open-episode check must answer before the INSERT", n)
	}

	open, err := db.ListOpenThresholdEpisodes()
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("five evaluations of one continuous demand produced %d episodes, want 1", len(open))
	}
	// WHAT ENFORCES WHAT, because these are two different guarantees. The
	// partial unique index is what makes a duplicate IMPOSSIBLE; the monitor's
	// read of the open row before it writes is what keeps the invariant from
	// being enforced by a failed INSERT on every delta — the failed-mint count
	// above is that half.
	// expected_orders is the system's stated intent, stamped ONCE:
	// ceil((100-40)/18) = 4.
	got, err := db.GetDemandOrigin(open[0].OriginID)
	if err != nil {
		t.Fatalf("read episode: %v", err)
	}
	if got.ExpectedOrders == nil || *got.ExpectedOrders != 4 {
		t.Errorf("expected_orders = %v, want 4 — ceil((threshold-opened_total)/capacity)", got.ExpectedOrders)
	}
	if got.OpenedTotal != 40 {
		t.Errorf("opened_total = %d, want the reading that DECIDED (40)", got.OpenedTotal)
	}
}

// The rising edge. Before the grain this branch did nothing at all — recovery
// was the absence of firing, so there was no way to say a demand had ENDED and
// therefore no way to say what one had cost.
func TestThresholdEpisode_RisingEdgeCloses(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	b := episodeBinding(t, eng, "PANEL-EP2", 18)

	m.checkBindings([]thresholdEntry{b}, 40, "below_threshold", false)
	open, _ := db.ListOpenThresholdEpisodes()
	if len(open) != 1 {
		t.Fatalf("no episode opened: %d", len(open))
	}
	originID := open[0].OriginID

	m.checkBindings([]thresholdEntry{b}, 120, "below_threshold", false)

	got, err := db.GetDemandOrigin(originID)
	if err != nil || got == nil {
		t.Fatalf("read episode: %v", err)
	}
	if got.ClosedAt == nil {
		t.Fatal("recovering above threshold must close the demand")
	}
	if got.CloseReason != protocol.CloseReasonRecovered {
		t.Errorf("close_reason = %q, want %q", got.CloseReason, protocol.CloseReasonRecovered)
	}
	// And the place is reusable: the next crossing is a NEW demand.
	m.checkBindings([]thresholdEntry{b}, 30, "below_threshold", false)
	open, _ = db.ListOpenThresholdEpisodes()
	if len(open) != 1 || open[0].OriginID == originID {
		t.Errorf("the next falling edge must open a fresh episode, got %d open, id reused=%v",
			len(open), len(open) == 1 && open[0].OriginID == originID)
	}
}

// EVERY CORE RESTART WOULD OTHERWISE DOUBLE EVERY OPEN DEMAND.
//
// startupSweep rebuilds its caches and re-evaluates every binding. With empty
// maps a binding still below threshold reads as a first crossing and mints a
// second episode for a place that already has one open. Restarting Core is the
// remedy an operator reaches for BECAUSE the counts look wrong — i.e. exactly
// when demands are open — so this is the highest-frequency path, not an edge
// case.
func TestThresholdEpisode_SurvivesRestart(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	m, sink := loggingMonitor(t, db)
	eng := m.eng
	b := episodeBinding(t, eng, "PANEL-EP3", 18)

	m.checkBindings([]thresholdEntry{b}, 40, "below_threshold", false)
	open, _ := db.ListOpenThresholdEpisodes()
	if len(open) != 1 {
		t.Fatalf("no episode opened: %d", len(open))
	}
	original := open[0].OriginID

	// The restart: a whole new monitor over the same database, seeing the same
	// still-breached level. No rehydrate — it reads the open row.
	restarted := NewThresholdMonitor(eng)
	restarted.checkBindings([]thresholdEntry{b}, 38, "below_threshold", false)
	if n := sink.countContaining("open demand episode key="); n != 0 {
		t.Errorf("%d failed mint(s) after the restart, want 0 — the restarted monitor must know the demand is open before it tries to write one", n)
	}

	open, err := db.ListOpenThresholdEpisodes()
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("after a restart there are %d open episodes for one place, want 1", len(open))
	}
	if open[0].OriginID != original {
		t.Errorf("post-restart origin %s != %s — the demand was lost across the restart",
			open[0].OriginID, original)
	}
	// There is nothing to rehydrate: the restarted monitor reads the open row
	// the moment it evaluates the place, which is the failed-mint assertion
	// above. What a restart that lost the demand would cost is a fire with no
	// origin on it — pinned end to end in TestReadThrough_RestartMidEpisode.
}

// A DENOMINATOR NOBODY CAN COMPUTE IS NOT 1, AND IT IS NOT 0.
//
// Both render as a real ratio somebody would draw a conclusion from. NULL plus
// a recorded reason is a different state, and the surface shows a dash for it.
func TestThresholdEpisode_UnknowableDenominatorIsNull(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	// No catalog payload at all — the capacity is unknowable.
	b := episodeBinding(t, eng, "PANEL-EP4-ABSENT", 0)

	m.checkBindings([]thresholdEntry{b}, 40, "below_threshold", false)

	open, _ := db.ListOpenThresholdEpisodes()
	if len(open) != 1 {
		t.Fatalf("no episode opened: %d", len(open))
	}
	got, err := db.GetDemandOrigin(open[0].OriginID)
	if err != nil {
		t.Fatalf("read episode: %v", err)
	}
	if got.ExpectedOrders != nil {
		t.Errorf("expected_orders = %d, want NULL — a capacity that cannot be read is not a denominator of 1",
			*got.ExpectedOrders)
	}
	if got.ExpectedUnknownReason == "" {
		t.Error("a NULL denominator must record WHY, or the surface cannot distinguish it from a missing write")
	}
}

// THE DENOMINATOR MOVED, SO THE EPISODE ENDS.
//
// Carrying one episode across a threshold edit would make its cost_ratio a
// division by a number that was not in force for most of its life. Both rows
// then record the transition honestly.
//
// The reason must be threshold_changed specifically — not `recovered`, which
// would claim the line got its material, and not threshold_removed, which would
// claim the binding went away. Three different facts about the plant.
func TestThresholdEpisode_ThresholdChangeClosesAndReopens(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	b := episodeBinding(t, eng, "PANEL-EP6", 18)

	m.checkBindings([]thresholdEntry{b}, 40, "below_threshold", false)
	open, _ := db.ListOpenThresholdEpisodes()
	if len(open) != 1 {
		t.Fatalf("no episode opened: %d", len(open))
	}
	original := open[0].OriginID

	m.closeThresholdEpisodesForChangedBindings([]demands.RegistryChange{{
		StationID: b.stationID, CoreNodeName: b.coreNodeName, PayloadCode: b.payloadCode,
		OldThreshold: 100, NewThreshold: 150,
	}})

	got, err := db.GetDemandOrigin(original)
	if err != nil || got == nil {
		t.Fatalf("read episode: %v", err)
	}
	if got.CloseReason != protocol.CloseReasonThresholdChanged {
		t.Errorf("close_reason = %q, want %q — the need did not recover and the binding did not vanish",
			got.CloseReason, protocol.CloseReasonThresholdChanged)
	}
	// And nothing is open for the place, so the next crossing mints against
	// the NEW threshold rather than joining an episode measured on the old one.
	if held, err := db.OpenOriginForKey(placeKey(b.coreNodeName, b.payloadCode)); err != nil || held != "" {
		t.Errorf("an episode %q is still open after a threshold change (err %v)", held, err)
	}
}

// A BINDING DELETED UNDERNEATH A LIVE DEMAND, while the payload stays bound at
// another place. "The payload has no bindings left" is false here, so only a
// comparison by place catches it — and the reconciling sweep is the one closer
// of a binding that vanished with nothing announcing it.
func TestThresholdEpisode_RemovedBindingClosesEvenWhenPayloadSurvives(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)

	gone := episodeBinding(t, eng, "PANEL-EP7", 18)
	survivor := thresholdEntry{
		stationID: "PLANT.LINE2", coreNodeName: "SLN_009",
		payloadCode: gone.payloadCode, threshold: 100,
	}
	m.checkBindings([]thresholdEntry{gone, survivor}, 40, "below_threshold", false)
	open, _ := db.ListOpenThresholdEpisodes()
	if len(open) != 2 {
		t.Fatalf("two bindings below threshold should open two episodes, got %d", len(open))
	}

	// The registry holds only the survivor — the payload still HAS a binding.
	registerBinding(t, db, survivor)
	m.reconcileThresholdBindings()

	stillOpen, err := db.ListOpenThresholdEpisodes()
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(stillOpen) != 1 {
		t.Fatalf("%d episodes still open, want 1 (only the survivor)", len(stillOpen))
	}
	if stillOpen[0].CoreNodeName != survivor.coreNodeName {
		t.Errorf("the WRONG episode was closed: %s survived", stillOpen[0].CoreNodeName)
	}
	// And the closed one says why: the need did not recover, it stopped being
	// watched.
	for _, o := range open {
		if o.CoreNodeName != gone.coreNodeName {
			continue
		}
		got, _ := db.GetDemandOrigin(o.OriginID)
		if got.CloseReason != protocol.CloseReasonThresholdRemoved {
			t.Errorf("close_reason = %q, want %q", got.CloseReason, protocol.CloseReasonThresholdRemoved)
		}
	}
}

// The negative-total case, with the clamp. A -443 reading must not produce
// ceil(543/18) = 31 expected orders computed from garbage.
func TestThresholdEpisode_NegativeTotalClampsTheDenominator(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	b := episodeBinding(t, eng, "PANEL-EP5", 18)

	m.checkBindings([]thresholdEntry{b}, -443, "below_threshold", false)

	open, _ := db.ListOpenThresholdEpisodes()
	if len(open) != 1 {
		t.Fatalf("a negative total must still open a demand: %d", len(open))
	}
	got, _ := db.GetDemandOrigin(open[0].OriginID)
	// Clamped: ceil((100-0)/18) = 6, not ceil((100+443)/18) = 31.
	if got.ExpectedOrders == nil || *got.ExpectedOrders != 6 {
		t.Errorf("expected_orders = %v, want 6 — opened_total must clamp at 0 before the division",
			got.ExpectedOrders)
	}
}
