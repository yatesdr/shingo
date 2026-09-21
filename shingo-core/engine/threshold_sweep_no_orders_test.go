//go:build docker

package engine

import (
	"strings"
	"testing"
)

// threshold_sweep_no_orders_test.go — A RECONCILING SWEEP KEEPS THE RECORD
// STRAIGHT. IT NEVER CREATES AN ORDER AND IT NEVER EVALUATES A LEVEL.
//
// The sweep gained a rebuild so it could take a withdrawn binding out of the
// monitor's memory, and it got that rebuild by calling engagePayloads whole.
// engagePayloads is two things joined: the rebuild, and an evaluation that
// reads the authoritative in-loop total and creates replenishment orders for
// whatever is below threshold. So a 60-second timer can now decide to order
// material — for bindings it was never asked about, on a pass whose only job
// was to close an episode belonging to a DIFFERENT binding.
//
// reconcileThresholdBindings' own doc already says the level is not its
// business: "THE PRECONDITION IS THE BINDING, NOT THE LEVEL", because the
// rising edge in checkBindings owns the level, runs on every delta, applies the
// hysteresis margin and says `recovered`. A sweep that also read the total
// would be a second opinion on a question that already has an answer. Ordering
// off that same total is the same mistake with material attached to it.
//
// AND IT INHERITS THE WORST CASE. engagePayloads deliberately falls through to
// total = 0 when the authoritative read fails, so a person editing a loader
// against a sick database still arms a zero-stock payload. Zero is below every
// threshold there is. On a timer, with nobody watching, that turns one Postgres
// blip into a plant-wide order for every payload the sweep happens to rebuild.
//
// WHAT THE SWEEP IS STILL FOR is unchanged: it closes the episodes of bindings
// demand_registry no longer has, and it takes those bindings out of memory so
// the next delta does not mint them again. Surviving bindings are evaluated by
// the next delta, the way they always were.

// THE SURVIVOR IS REBUILT, NOT ORDERED FOR.
//
// One payload, two loaders. The ghost on LINE1 is in the monitor's memory and
// nowhere else, so the sweep closes its episode and rebuilds the payload — and
// the rebuild is plant-wide, so it picks up the healthy binding on LINE2 that
// the pass was never asked about. That binding is below threshold and its
// debounce is expired, so nothing downstream would stop an order.
//
// The debounce is back-dated ON PURPOSE. Without it a passing test proves only
// that allow() happened to be shut, which is luck rather than a guarantee: the
// same sweep against a binding that last fired twenty seconds ago would order.
func TestThresholdSweep_RebuildsALivePayloadWithoutOrderingForIt(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	sink := &logSink{}
	eng := newLoggingEngine(t, db, sink)
	m := eng.thresholdMonitor
	fires := captureThresholdFires(t, eng)

	const payload = "PANEL-SW9"
	ghost := stationBinding(t, eng, "PLANT.LINE1", "SLN_309", payload, 18)
	live := stationBinding(t, eng, "PLANT.LINE2", "SLN_409", payload, 18)
	registerBinding(t, db, live)
	injectBinding(t, m, payload, ghost)

	// The ghost's demand, minted by an ordinary delta. This is the premise, not
	// the assertion: without it the sweep closes nothing, rebuilds nothing, and
	// every silence below would be the silence of a pass that did no work.
	m.evaluatePayload(payload, "below_threshold")
	if open := openThresholdEpisodes(t, db); len(open) != 1 {
		t.Fatalf("the delta opened %d episodes, want 1 — only the ghost is in memory", len(open))
	}
	if got := fires.count(ghost.stationID); got != 1 {
		t.Fatalf("the delta made %d replenishment decisions for the ghost, want 1 — the recorder is not watching anything", got)
	}
	if got := fires.count(live.stationID); got != 0 {
		t.Fatalf("%s was ordered for before the sweep ran (%d decisions) — it is not in memory yet", live.stationID, got)
	}

	liveKey := bindingKey(live.stationID, live.coreNodeName, live.payloadCode)
	m.mu.Lock()
	m.debounce[liveKey] = m.nowFn().Add(-2 * thresholdDebounceWindow)
	m.mu.Unlock()

	if closed := m.reconcileThresholdBindings(); closed != 1 {
		t.Fatalf("the sweep closed %d episodes, want 1 — only the ghost lost its precondition", closed)
	}

	// THE RULING. The pass closed a record and corrected memory. It did not
	// decide anything about how much material is on the floor.
	if got := fires.count(live.stationID); got != 0 {
		t.Errorf("the sweep made %d replenishment decisions for %s, want 0 — a reconciling sweep never creates an order",
			got, live.stationID)
	}
	if rows := episodesForPayload(t, db, payload); len(rows) != 1 {
		t.Errorf("%d episodes exist for %s after the sweep, want 1 (the ghost's) — the sweep minted a demand for a binding it was not asked about",
			len(rows), payload)
	}
	if held := m.currentThresholdOrigin(liveKey); held != "" {
		t.Errorf("the sweep left %s holding origin %s — it evaluated a level and stamped a falling edge", liveKey, held)
	}
	if lines := sink.linesContaining("read for " + payload); len(lines) != 0 {
		t.Errorf("the sweep issued an authoritative UOP read:\n%s", strings.Join(lines, "\n"))
	}

	// AND IT DID DO ITS OWN JOB. The ghost is gone from memory and the survivor
	// is in it — a sweep that skipped the rebuild entirely would satisfy every
	// assertion above and be a worse bug than the one this pins.
	m.mu.Lock()
	left := append([]thresholdEntry(nil), m.thresholdsByPayload[payload]...)
	m.mu.Unlock()
	if len(left) != 1 || left[0].coreNodeName != live.coreNodeName {
		t.Fatalf("the sweep left %d binding(s) in thresholdsByPayload, want only %s — the rebuild is the half it IS for",
			len(left), live.coreNodeName)
	}

	// THE NEXT DELTA IS WHAT EVALUATES THE SURVIVOR, as it always was. Once,
	// not twice: a sweep that had already fired would have stamped the debounce
	// and this would be silent.
	m.evaluatePayload(payload, "below_threshold")
	if got := fires.count(live.stationID); got != 1 {
		t.Errorf("the delta after the sweep made %d replenishment decisions for %s, want exactly 1",
			got, live.stationID)
	}
	rows := episodesForPayload(t, db, payload)
	if len(rows) != 2 {
		t.Fatalf("%d episodes for %s after the delta, want 2 — the ghost's, ended, and the survivor's, open", len(rows), payload)
	}
	minted := rows[1]
	if !minted.open || minted.stationID != live.stationID {
		t.Errorf("the delta's episode is station=%s open=%v, want %s open", minted.stationID, minted.open, live.stationID)
	}
}

// A FAILED AUTHORITATIVE READ ON A TIMER ORDERS NOTHING.
//
// The registry read succeeds, so the rebuild runs and the survivor is found;
// the UOP sum is what fails. engagePayloads answers that by evaluating against
// zero, which is below every threshold there is — a choice made for a person
// standing at a loader config page and inherited by a sweep that nobody is
// watching. One Postgres blip, every payload the pass rebuilt, orders for all
// of them.
//
// The assertion that the sweep issued no read at all is the stronger half: a
// sweep that reads and then declines to act on the answer is one edit away from
// acting on it again.
func TestThresholdSweep_UOPReadErrorDuringRebuildOrdersNothing(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	sink := &logSink{}
	eng := newLoggingEngine(t, db, sink)
	m := eng.thresholdMonitor
	fires := captureThresholdFires(t, eng)

	const payload = "PANEL-SW10"
	ghost := stationBinding(t, eng, "PLANT.LINE1", "SLN_310", payload, 18)
	live := stationBinding(t, eng, "PLANT.LINE2", "SLN_410", payload, 18)
	registerBinding(t, db, live)
	injectBinding(t, m, payload, ghost)

	m.evaluatePayload(payload, "below_threshold")
	if open := openThresholdEpisodes(t, db); len(open) != 1 {
		t.Fatalf("the delta opened %d episodes, want 1", len(open))
	}

	withTableHidden(t, db, "lineside_buckets", func() {
		if closed := m.reconcileThresholdBindings(); closed != 1 {
			t.Fatalf("the sweep closed %d episodes while the UOP read was failing, want 1 — the registry read is not the one that is broken",
				closed)
		}
	})

	if got := fires.count(live.stationID); got != 0 {
		t.Errorf("the sweep made %d replenishment decisions for %s off a failed UOP read, want 0",
			got, live.stationID)
	}
	if rows := episodesForPayload(t, db, payload); len(rows) != 1 {
		t.Errorf("%d episodes exist for %s, want 1 (the ghost's) — a failed read minted a demand against a total nobody could read",
			len(rows), payload)
	}
	if lines := sink.linesContaining("read for " + payload); len(lines) != 0 {
		t.Errorf("the sweep issued an authoritative UOP read and logged its failure:\n%s", strings.Join(lines, "\n"))
	}

	// The rebuild still happened, or the silence above is the silence of a pass
	// that gave up rather than a pass that stayed in its lane.
	m.mu.Lock()
	left := append([]thresholdEntry(nil), m.thresholdsByPayload[payload]...)
	m.mu.Unlock()
	if len(left) != 1 || left[0].coreNodeName != live.coreNodeName {
		t.Fatalf("the sweep left %d binding(s) in thresholdsByPayload, want only %s", len(left), live.coreNodeName)
	}
}
