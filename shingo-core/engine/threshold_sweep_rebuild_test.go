//go:build docker

package engine

import (
	"context"
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/fleet/simulator"
	"shingocore/store"
	"shingocore/store/demands"
)

// threshold_sweep_rebuild_test.go — what the reconciling sweep does to the
// monitor's MEMORY, as opposed to what it does to demand_origins.
//
// threshold_station_reap_test.go pins the station grain through the production
// door that Lane A wired (the stale-edge reaper's Resync). These tests are about
// the sweep standing alone, because Springfield 2026-08-19 says it has to:
// `marked stale` is 0 across the burst window and a 48h control, so the reaper
// did not drive it, and after six eliminations the writer that emptied
// demand_registry is still unnamed. An unnamed entry point cannot be fixed at
// its door.
//
// The two directions pinned here are the ones a floor must not break. A read
// error must not be read as an empty binding set — and specifically must not
// trigger a rebuild off one. And a binding that comes BACK must re-engage
// through every door that engages one, minting exactly once if the place is
// still hungry: a floor that quietly un-monitors a loader is the same outage as
// the one it was built to prevent, just harder to see.

// sweptAwayBinding engages a binding the way production does, takes its
// demand_registry rows away with nothing firing, and lets the SWEEP be the thing
// that notices. Returns nothing: every test that uses it then reopens the
// question from the database.
//
// The reap is deliberately NOT followed by a Resync here. Resync is Lane A's
// door and threshold_station_reap_test.go owns it; what these tests need is the
// state a plant is in when the writer that emptied the registry is one nobody
// has identified — rows gone, no notification, the sweep running underneath.
func sweptAwayBinding(t *testing.T, db *store.DB, m *ThresholdMonitor, b thresholdEntry) {
	t.Helper()
	registerBinding(t, db, b)
	m.Resync(b.stationID)
	if open := openThresholdEpisodes(t, db); len(open) != 1 {
		t.Fatalf("Resync opened %d episodes for one below-threshold binding, want 1", len(open))
	}
	emptyStationRegistry(t, db, b.stationID)
	if closed := m.reconcileThresholdBindings(); closed != 1 {
		t.Fatalf("the sweep closed %d episodes whose binding is gone, want 1", closed)
	}
}

// assertOneClosedOneOpen is the shape every "the binding came back" test ends
// in: the withdrawn config ended exactly one demand, and the return opened
// exactly one new one. Two rows, never three, and never one.
func assertOneClosedOneOpen(t *testing.T, db *store.DB, payload string) {
	t.Helper()
	rows := episodesForPayload(t, db, payload)
	if len(rows) != 2 {
		t.Fatalf("withdraw then return produced %d episodes, want 2 (one ended by the withdrawal, one opened by the return)", len(rows))
	}
	open, closed := 0, 0
	for _, r := range rows {
		if r.open {
			open++
			continue
		}
		closed++
		if r.closeReason != protocol.CloseReasonThresholdRemoved {
			t.Errorf("the withdrawn binding's episode closed %q, want %q — the need did not recover, it stopped being watched",
				r.closeReason, protocol.CloseReasonThresholdRemoved)
		}
	}
	if open != 1 || closed != 1 {
		t.Fatalf("%d open and %d closed episodes after the binding returned, want exactly 1 and 1", open, closed)
	}
}

// A READ FAILURE IS NOT AN EMPTY BINDING SET, and it must not be an empty one
// for the monitor's MEMORY either.
//
// The sweep already refuses to close on a failed demand_registry read
// (TestThresholdEpisode_SweepReadErrorIsNotAnEmptyBindingSet pins that half).
// The half with nothing on it is the cache: a sweep that reacted to a read error
// by rebuilding thresholdsByPayload from the same unreadable table would drop
// every binding it touched, and the station would simply stop being replenished
// — no error on the replenishment path, no episode, nothing to read afterwards
// except a quiet loader. That is strictly worse than the stranded episode the
// sweep exists to prevent, because a stranded episode is at least visible.
func TestThresholdSweep_ReadErrorLeavesTheBindingCacheAlone(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	const payload = "PANEL-SW1"
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_301", payload, 18)
	registerBinding(t, db, b)

	m.Resync(b.stationID)
	open := openThresholdEpisodes(t, db)
	if len(open) != 1 {
		t.Fatalf("Resync opened %d episodes, want 1", len(open))
	}
	originID := open[0].OriginID

	hideDemandRegistry(t, db)
	if closed := m.reconcileThresholdBindings(); closed != 0 {
		t.Errorf("the sweep closed %d episodes on a demand_registry read error, want 0", closed)
	}

	if got := mustGetOrigin(t, db, originID); got.ClosedAt != nil {
		t.Errorf("a read error closed a live episode (reason=%q, by=%q)", got.CloseReason, got.ClosedBy)
	}
	m.mu.Lock()
	bindings, monitored := m.thresholdsByPayload[payload]
	m.mu.Unlock()
	if !monitored || len(bindings) != 1 {
		t.Fatalf("a read error left %d binding(s) in thresholdsByPayload (monitored=%v), want the 1 that was there — a blip must not un-monitor a loader",
			len(bindings), monitored)
	}
	// And the cache is still LIVE, not merely present: the next delta has to
	// reach the binding. A cache entry nothing evaluates is the same outage.
	if held := m.currentThresholdOrigin(bindingKey(b.stationID, b.coreNodeName, b.payloadCode)); held != originID {
		t.Errorf("after a read error the monitor holds %q, want %s", held, originID)
	}
}

// THE RETURN, THROUGH RESYNC. An Edge reconnects (or seeddev writes the rows and
// something resyncs the station) and the binding is back. The place is still
// hungry, so that is a NEW demand: re-joining the closed one would make a single
// row span an outage the plant knows nothing about.
func TestThresholdSweep_ReturnedBindingReEngagesViaResync(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	const payload = "PANEL-SW2"
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_302", payload, 18)
	sweptAwayBinding(t, db, m, b)

	registerBinding(t, db, b)
	m.Resync(b.stationID)

	assertOneClosedOneOpen(t, db, payload)
	if held := m.currentThresholdOrigin(bindingKey(b.stationID, b.coreNodeName, b.payloadCode)); held == "" {
		t.Error("the monitor holds no origin after the binding returned — its signals would fire with no demand attached")
	}
}

// THE RETURN, THROUGH A CONFIG EDIT. OnThresholdChanges is the loader UI's door:
// an engineer re-adds the payload to the loader and SyncDemandRegistry reports
// the threshold moving off 0.
func TestThresholdSweep_ReturnedBindingReEngagesViaThresholdChange(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	const payload = "PANEL-SW3"
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_303", payload, 18)
	sweptAwayBinding(t, db, m, b)

	registerBinding(t, db, b)
	m.OnThresholdChanges([]demands.RegistryChange{{
		StationID: b.stationID, CoreNodeName: b.coreNodeName, PayloadCode: b.payloadCode,
		OldThreshold: 0, NewThreshold: b.threshold,
	}})

	assertOneClosedOneOpen(t, db, payload)
}

// THE RETURN, THROUGH A RESTART. startupSweep is the door that needs no wire at
// all, which is why it is the one an operator reaches for — and the sweep must
// not have left anything behind that makes a restart mint twice or not at all.
//
// A WHOLE NEW MONITOR, because that is what a restart is. Reusing the old one
// would test a cache that a real restart never has.
func TestThresholdSweep_ReturnedBindingReEngagesViaStartupSweep(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	const payload = "PANEL-SW4"
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_304", payload, 18)
	sweptAwayBinding(t, db, m, b)

	registerBinding(t, db, b)
	restarted := NewThresholdMonitor(eng)
	restarted.startupSweep(context.Background())

	assertOneClosedOneOpen(t, db, payload)
	if held := restarted.currentThresholdOrigin(bindingKey(b.stationID, b.coreNodeName, b.payloadCode)); held == "" {
		t.Error("the restarted monitor holds no origin for a binding that is back and still below threshold")
	}
}

// staleBindingTag is the grep handle on the line this lane adds. Named once so
// a test cannot drift from the string an operator is told to search for.
const staleBindingTag = "STALE BINDING IN MEMORY"

// staleBindingLineBudget is the ceiling on that line's length, in characters.
//
// A real constraint rather than a style rule. The line fires once per affected
// payload per sweep for the whole life of a mismatch, into a plant journal that
// is already too big to grep quickly — Springfield's Core journal is 3.5 GB and
// an unbounded scan of it costs about forty minutes. The budget is loose enough
// that four fields and a clause fit with room for a long payload code and
// several absent bindings, and tight enough that a paragraph cannot come back.
const staleBindingLineBudget = 400

// loggingMonitor builds a monitor over an UNSTARTED engine whose log is
// captured, so a test can assert on the PROVER itself and not only on what the
// database ended up holding.
//
// It reuses stranded_fixround_docker_test.go's logSink rather than standing up a
// second one: a package with two capture helpers is a package where the next
// person picks whichever they find first, and they drift.
func loggingMonitor(t *testing.T, db *store.DB) (*ThresholdMonitor, *logSink) {
	t.Helper()
	sink := &logSink{}
	return NewThresholdMonitor(newLoggingEngine(t, db, sink)), sink
}

// injectBinding puts a binding into the monitor's memory THROUGH NO PRODUCTION
// DOOR.
//
// This is the whole point of the lane and it is worth being explicit about why
// the test is written the wrong-looking way. Six candidates for the writer that
// emptied Springfield's demand_registry on 2026-08-19 have been eliminated and
// none named: `marked stale` is 0 across the burst window and a 48h control, so
// it was not the stale-edge reaper, and the onset is a whole-station absence
// appearing between two sweeps rather than any per-binding edit. A test that
// reached this state through Resync, OnThresholdChanges or startupSweep would be
// asserting that THAT door is clean, which is a claim about a door nobody has
// shown is the one. Assigning the map directly asserts the only thing that can
// be asserted without naming the writer: whatever put it there, the floor ends
// it.
func injectBinding(t *testing.T, m *ThresholdMonitor, payload string, bindings ...thresholdEntry) {
	t.Helper()
	m.mu.Lock()
	m.thresholdsByPayload[payload] = bindings
	m.mu.Unlock()
}

// THE VERIFY-RED, AND IT NAMES NO ENTRY POINT.
//
// A binding sits in the monitor's memory that demand_registry does not have. The
// level is below threshold and deltas keep arriving, so every evaluation mints;
// the sweep closes each mint `threshold_removed` because the binding is absent,
// and closeThresholdEpisodeRef clears belowThresholdSince on the way out, which
// re-arms the falling edge for the next delta. The sweep is supplying the other
// half of an oscillator.
//
// ONE ABSENCE IS ONE ENDING. The assertion is a ROW COUNT over demand_origins,
// not a state: Springfield's 411 opens were followed by 405 closes, so a query
// for open episodes reports a perfectly healthy plant and always would have.
func TestThresholdSweep_GhostBindingClosesOnceAndStopsMinting(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	m, _ := loggingMonitor(t, db)
	const payload = "PANEL-SW5"
	b := stationBinding(t, m.eng, "PLANT.LINE1", "SLN_305", payload, 18)
	injectBinding(t, m, payload, b)

	// Four alternating passes. The oscillator adds a row per pass, so the count
	// is the measurement.
	for i := 0; i < 4; i++ {
		m.evaluatePayload(payload, "below_threshold")
		m.reconcileThresholdBindings()
	}

	rows := episodesForPayload(t, db, payload)
	if len(rows) != 1 {
		t.Fatalf("a binding the database does not have minted %d episodes across 4 sweep passes, want 1 — the sweep closes each mint and re-arms the next, which is the Springfield 2026-08-19 shape (411 opens, 411 distinct origins, 405 closes)",
			len(rows))
	}
	got := rows[0]
	if got.open {
		t.Fatal("the episode is still open — its binding does not exist, so nothing will ever close it")
	}
	if got.closeReason != protocol.CloseReasonThresholdRemoved {
		t.Errorf("close_reason = %q, want %q — the need did not recover, it stopped being watched",
			got.closeReason, protocol.CloseReasonThresholdRemoved)
	}
	// BY THE SWEEP. The rebuild has to run AFTER the closes, or
	// rebuildPayloadBindings' own comparison closes the same row
	// `by=notification` and the sweep's share of the closing stops being
	// measurable.
	if got.closedBy != protocol.ClosedBySweep {
		t.Errorf("closed_by = %q, want %q — the sweep is what noticed", got.closedBy, protocol.ClosedBySweep)
	}
	m.mu.Lock()
	_, monitored := m.thresholdsByPayload[payload]
	m.mu.Unlock()
	if monitored {
		t.Error("the absent binding is still in thresholdsByPayload — the next delta mints again")
	}
}

// ONE LINE PER PAYLOAD, NAMING EVERY ABSENT BINDING, AND THE REGISTRY COUNT.
//
// The whole-station absence at the onset of the 2026-08-19 burst closed two
// long-open episodes on two different nodes in the same instant, so the
// several-bindings case is the one that actually happened, not a hypothetical.
// The line is per PAYLOAD because the rebuild is per payload — one
// LookupDemandThresholdsByPayload, one announcement — and it carries the absent
// bindings as a LIST rather than a scalar station field, because a field whose
// meaning depends on how many there are is exactly the ambiguity that made the
// journal unreadable.
func TestThresholdSweep_StaleLineNamesEveryAbsentBindingOnce(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	m, sink := loggingMonitor(t, db)
	const payload = "PANEL-SW6"
	first := stationBinding(t, m.eng, "PLANT.LINE1", "SMN_022", payload, 18)
	second := stationBinding(t, m.eng, "PLANT.LINE1", "SMN_030", payload, 18)
	injectBinding(t, m, payload, first, second)

	m.evaluatePayload(payload, "below_threshold")
	if open := openThresholdEpisodes(t, db); len(open) != 2 {
		t.Fatalf("two below-threshold bindings opened %d episodes, want 2", len(open))
	}
	if closed := m.reconcileThresholdBindings(); closed != 2 {
		t.Fatalf("the sweep closed %d episodes, want 2", closed)
	}

	lines := sink.linesContaining(staleBindingTag)
	if len(lines) != 1 {
		t.Fatalf("%d %q lines for one payload, want exactly 1 — one rebuild, one announcement:\n%s",
			len(lines), staleBindingTag, strings.Join(lines, "\n"))
	}
	for _, want := range []string{
		"payload=" + payload,
		"registry_rows=0",
		"closed=2",
		"PLANT.LINE1/SMN_022",
		"PLANT.LINE1/SMN_030",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the line does not carry %q — an operator cannot tell which binding went or whether the rows are there:\n%s",
				want, lines[0])
		}
	}
	// AND IT STAYS A LINE, NOT A PARAGRAPH. This fires once per affected payload
	// per sweep for as long as the mismatch lasts, and the 2026-08-19 burst ran
	// over nineteen hours — into a Core journal already at 3.5 GB, where an
	// unbounded scan costs about forty minutes. Prose here degrades the tool the
	// next diagnosis depends on, and it does not skim beside its key=value
	// neighbours. The explanation lives in dropAbsentBindingsFromMemory's doc
	// comment, where it costs nothing at runtime.
	if len(lines[0]) > staleBindingLineBudget {
		t.Errorf("the line is %d characters, over the %d budget — put the explanation in the doc comment, not in the journal:\n%s",
			len(lines[0]), staleBindingLineBudget, lines[0])
	}
}

// REGISTRY_ROWS IS THE FACT THE JOURNAL DID NOT HAVE.
//
// Zero means the payload's bindings are gone plant-wide; a non-zero count means
// the payload is still bound at another loader and only the listed binding went.
// Those are different plants, and the 2026-08-19 journal could not tell them
// apart — which is most of why the writer is still unnamed. The count comes out
// of the same ListDemandThresholds result the comparison decided on, so it
// cannot disagree with the decision it explains.
func TestThresholdSweep_StaleLineCountsTheRegistryRowsThatRemain(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	m, sink := loggingMonitor(t, db)
	const payload = "PANEL-SW7"
	ghost := stationBinding(t, m.eng, "PLANT.LINE1", "SLN_307", payload, 18)
	survivor := stationBinding(t, m.eng, "PLANT.LINE2", "SLN_407", payload, 18)
	registerBinding(t, db, survivor)
	injectBinding(t, m, payload, ghost, survivor)

	m.evaluatePayload(payload, "below_threshold")
	if open := openThresholdEpisodes(t, db); len(open) != 2 {
		t.Fatalf("two below-threshold bindings opened %d episodes, want 2", len(open))
	}
	if closed := m.reconcileThresholdBindings(); closed != 1 {
		t.Fatalf("the sweep closed %d episodes, want 1 — only the absent binding lost its precondition", closed)
	}

	lines := sink.linesContaining(staleBindingTag)
	if len(lines) != 1 {
		t.Fatalf("%d %q lines, want 1:\n%s", len(lines), staleBindingTag, strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "closed=1") {
		t.Errorf("the line must say it closed exactly 1 episode — the survivor did not lose its precondition:\n%s", lines[0])
	}
	if !strings.Contains(lines[0], "registry_rows=1") {
		t.Errorf("the line must say the payload still holds 1 registry row, or it reads as a payload that vanished plant-wide:\n%s", lines[0])
	}
	if strings.Contains(lines[0], survivor.coreNodeName) {
		t.Errorf("the line names %s, whose binding is present — the absent list must only carry what went:\n%s",
			survivor.coreNodeName, lines[0])
	}
	// And the survivor is untouched: still bound, still held, still open.
	m.mu.Lock()
	left := append([]thresholdEntry(nil), m.thresholdsByPayload[payload]...)
	m.mu.Unlock()
	if len(left) != 1 || left[0].coreNodeName != survivor.coreNodeName {
		t.Fatalf("the rebuild left %d binding(s) in thresholdsByPayload, want only %s — a plant-wide lookup must not un-monitor a healthy loader",
			len(left), survivor.coreNodeName)
	}
	if held := m.currentThresholdOrigin(bindingKey(survivor.stationID, survivor.coreNodeName, payload)); held == "" {
		t.Error("the rebuild dropped the monitor's hold on the survivor's episode")
	}
}

// A CLEAN PASS REBUILDS NOTHING AND SAYS NOTHING.
//
// The rebuild is reachable only from an actual close, so a plant whose
// notification paths all work pays nothing for the floor — no extra query, no
// line. That matters twice: the alternative shape (rebuild every payload every
// pass) is a plant-wide registry re-read per minute, and it would also make the
// loud line meaningless by printing it when nothing is wrong.
func TestThresholdSweep_CleanPassDoesNotRebuildOrAnnounce(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	m, sink := loggingMonitor(t, db)
	const payload = "PANEL-SW8"
	b := stationBinding(t, m.eng, "PLANT.LINE1", "SLN_308", payload, 18)
	registerBinding(t, db, b)
	m.Resync(b.stationID)
	open := openThresholdEpisodes(t, db)
	if len(open) != 1 {
		t.Fatalf("Resync opened %d episodes, want 1", len(open))
	}

	if closed := m.reconcileThresholdBindings(); closed != 0 {
		t.Fatalf("the sweep closed %d episodes whose binding is present, want 0", closed)
	}

	if lines := sink.linesContaining(staleBindingTag); len(lines) != 0 {
		t.Errorf("a clean pass emitted %d %q line(s):\n%s", len(lines), staleBindingTag, strings.Join(lines, "\n"))
	}
	if got := mustGetOrigin(t, db, open[0].OriginID); got.ClosedAt != nil {
		t.Errorf("a clean pass closed a live episode (reason=%q)", got.CloseReason)
	}
	m.mu.Lock()
	left := len(m.thresholdsByPayload[payload])
	m.mu.Unlock()
	if left != 1 {
		t.Errorf("a clean pass left %d binding(s) in thresholdsByPayload, want 1", left)
	}
}
