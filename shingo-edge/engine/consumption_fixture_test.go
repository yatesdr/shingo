package engine

import (
	"testing"
	"time"
)

// consumption_fixture_test.go — production-rate counter-delta fixture.
//
// The fixture drives `Engine.handleCounterDelta` with the SAME code path
// production uses (the EventCounterDelta subscription dispatches to
// handleCounterDelta directly). Tests that want to simulate "the line
// consumed 25 parts in 15 minutes" use processFixture.RunFor instead of
// mutating runtime.RemainingUOP directly — without this fixture, every
// "did the value change?" test reduces to itself, hiding predicate bugs
// like the #11 handleNormalReplenishment regression.
//
// The clock is virtual. Real time doesn't advance; the fixture computes
// how many tick events RunFor(d) is worth and emits them. Each tick is
// Delta=1 by default, matching production cell-counter behavior, so
// auto-reorder thresholds and lineside drain ordering are exercised
// faithfully.
//
// Reading state: all assertions go back through the test DB
// (db.GetProcessNodeRuntime). Fixture caches nothing. If we can't
// observe a value through the same query production uses, the test is
// lying — that's the rule.

// consumptionRate describes a production cadence as parts-per-period.
// Stored as integers to avoid float drift over long sim windows.
type consumptionRate struct {
	parts  int
	period time.Duration
}

// rate25Per15Min is a production-realistic consume rate (Springfield
// 2026-04 data). Used as the default for #11 choreography tests. If a
// future plant ships a faster line, add a sibling rate constant rather
// than mutating this one — existing tests' expected values are tied to
// the rate they were written against.
var rate25Per15Min = consumptionRate{parts: 25, period: 15 * time.Minute}

// processFixture drives counter-delta events at a configured rate
// against a seeded (process, style) pair. Engine state is read back
// through the test DB so assertions see what production would see.
type processFixture struct {
	t         *testing.T
	eng       *Engine
	processID int64
	styleID   int64
	rate      consumptionRate
	emitted   int
	elapsed   time.Duration
}

// newProcessFixture creates a consumption fixture for a (process, style)
// already seeded in the test DB. The caller owns the engine; the fixture
// only reads from it. Pass the engine after seedConsumeNode (or the
// produce equivalent) so the runtime row exists.
func newProcessFixture(t *testing.T, eng *Engine, processID, styleID int64, rate consumptionRate) *processFixture {
	t.Helper()
	if processID == 0 {
		t.Fatal("processFixture: processID must be non-zero")
	}
	if styleID == 0 {
		t.Fatal("processFixture: styleID must be non-zero")
	}
	if rate.parts <= 0 || rate.period <= 0 {
		t.Fatal("processFixture: rate.parts and rate.period must be positive")
	}
	return &processFixture{
		t:         t,
		eng:       eng,
		processID: processID,
		styleID:   styleID,
		rate:      rate,
	}
}

// RunFor advances the virtual clock by d and emits the equivalent number
// of Delta=1 counter events through eng.handleCounterDelta — the same
// path EventCounterDelta dispatches to. The integer-truncating math
// (d * parts / period) under-reports rather than over-reports when d
// lands mid-tick, which matches what a real cell counter does.
//
// Returns the number of ticks emitted so callers can assert on it
// directly without re-running the math.
func (f *processFixture) RunFor(d time.Duration) int {
	f.t.Helper()
	if d <= 0 {
		return 0
	}
	f.elapsed += d
	// Integer math: avoid float drift across long sim windows.
	parts := int(int64(d) * int64(f.rate.parts) / int64(f.rate.period))
	for i := 0; i < parts; i++ {
		f.eng.handleCounterDelta(CounterDeltaEvent{
			ProcessID: f.processID,
			StyleID:   f.styleID,
			Delta:     1,
		})
	}
	f.emitted += parts
	return parts
}

// EmitTicks fires n raw Delta=1 events without advancing the virtual
// clock. Useful for tests that want a specific tick count rather than a
// duration ("after 10 parts consumed, fire completion") — keeps the
// assertion expectation arithmetic-free.
func (f *processFixture) EmitTicks(n int) {
	f.t.Helper()
	if n <= 0 {
		return
	}
	for i := 0; i < n; i++ {
		f.eng.handleCounterDelta(CounterDeltaEvent{
			ProcessID: f.processID,
			StyleID:   f.styleID,
			Delta:     1,
		})
	}
	f.emitted += n
}

// LineUOP reads the current RemainingUOP for a node directly from the
// test DB. Assertions go through this helper so they see the actual
// stored value, not a fixture-cached one — that's the property that
// keeps the fixture honest against future refactors of where UOP is
// persisted.
//
// Returns -1 if the runtime row is missing, so a forgotten seed shows
// up as a clear failure rather than a zero-value confusion.
func (f *processFixture) LineUOP(nodeID int64) int {
	f.t.Helper()
	rt, err := f.eng.db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		f.t.Fatalf("processFixture.LineUOP: GetProcessNodeRuntime(%d): %v", nodeID, err)
	}
	if rt == nil {
		return -1
	}
	return rt.RemainingUOP
}

// Emitted reports how many ticks the fixture has driven so far. Useful
// for tests that want to log or assert on emission count without poking
// fixture internals.
func (f *processFixture) Emitted() int { return f.emitted }

// Elapsed reports the total virtual time the fixture has run for. Use
// this in test logs to tie assertion failures back to the simulated
// timeline ("at t=12m line UOP was 80, expected 75").
func (f *processFixture) Elapsed() time.Duration { return f.elapsed }

// =============================================================================
// Self-tests — exercise the fixture itself against a real seeded node so a
// future change to handleCounterDelta or the rate math doesn't silently
// break every test that depends on the fixture.
// =============================================================================

// TestProcessFixture_RunFor_DrivesRealCounterDeltaPath verifies that the
// fixture decrements runtime.RemainingUOP through handleCounterDelta —
// not via direct mutation. Asserts the fixture is correctly wired into
// the production code path; if a future refactor moves UOP arithmetic
// elsewhere, this test goes red and the fixture follows the production
// path before any choreography test gets re-written.
func TestProcessFixture_RunFor_DrivesRealCounterDeltaPath(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "FIX-RUNFOR", PayloadCode: "PART-FIX", UOPCapacity: 200, InitialUOP: 200,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 200); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	fix := newProcessFixture(t, eng, processID, styleID, rate25Per15Min)

	// 15 minutes at 25 parts/15min should drive exactly 25 ticks.
	emitted := fix.RunFor(15 * time.Minute)
	if emitted != 25 {
		t.Errorf("emitted = %d, want 25 (rate is exactly 25 parts/15min)", emitted)
	}

	// Each tick is Delta=1, applied through handleConsumeTick → UpdateProcessNodeUOP.
	// Started at 200, consumed 25, expect 175.
	if got := fix.LineUOP(nodeID); got != 175 {
		t.Errorf("LineUOP = %d, want 175 (200 - 25 ticks)", got)
	}
}

// TestProcessFixture_RunFor_PartialPeriodTruncates verifies the integer
// math under-reports on a partial period rather than over-reports. A
// real cell counter at 25 parts/15min has emitted 0 parts at t=1min,
// not 1.66; the fixture must match.
func TestProcessFixture_RunFor_PartialPeriodTruncates(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "FIX-PARTIAL", PayloadCode: "PART-PRT", UOPCapacity: 100, InitialUOP: 100,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 100); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	fix := newProcessFixture(t, eng, processID, styleID, rate25Per15Min)

	// 1 minute = 1.67 parts at this rate; integer math should produce 1.
	emitted := fix.RunFor(time.Minute)
	if emitted != 1 {
		t.Errorf("emitted at 1min = %d, want 1 (1*25/15 = 1, truncated)", emitted)
	}
	if got := fix.LineUOP(nodeID); got != 99 {
		t.Errorf("LineUOP = %d, want 99 (100 - 1)", got)
	}
}

// TestProcessFixture_EmitTicks_DirectCount verifies the tick-count entry
// point — the variant tests use when they want "after 10 consumed parts"
// rather than "after t minutes."
func TestProcessFixture_EmitTicks_DirectCount(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "FIX-TICKS", PayloadCode: "PART-TKS", UOPCapacity: 50, InitialUOP: 50,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 50); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	fix := newProcessFixture(t, eng, processID, styleID, rate25Per15Min)

	fix.EmitTicks(10)
	if got := fix.LineUOP(nodeID); got != 40 {
		t.Errorf("LineUOP = %d, want 40 (50 - 10)", got)
	}
	if fix.Elapsed() != 0 {
		t.Errorf("Elapsed = %v, want 0 (EmitTicks does not advance virtual clock)", fix.Elapsed())
	}
	if fix.Emitted() != 10 {
		t.Errorf("Emitted = %d, want 10", fix.Emitted())
	}
}

// TestProcessFixture_LineUOP_ReadsThroughDB verifies the read-back goes
// through GetProcessNodeRuntime — the same query production uses. Lock
// down: the fixture must NOT cache or shortcut the read, otherwise a
// future bug where UOP gets persisted to the wrong row would be invisible
// to fixture-driven tests.
func TestProcessFixture_LineUOP_ReadsThroughDB(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "FIX-READ", PayloadCode: "PART-RD", UOPCapacity: 300, InitialUOP: 300,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 300); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	fix := newProcessFixture(t, eng, processID, styleID, rate25Per15Min)

	// Mutate runtime via the production API (not the fixture) — fixture
	// must observe the change.
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 137); err != nil {
		t.Fatalf("manual mutation: %v", err)
	}
	if got := fix.LineUOP(nodeID); got != 137 {
		t.Errorf("LineUOP = %d, want 137 (must read through DB, not from fixture cache)", got)
	}
}
