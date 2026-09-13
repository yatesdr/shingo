package service

import (
	"context"
	"encoding/json"
	"testing"
)

// composer_budget_pins_test.go — the S0 budgets as assertions.
//
// WHY A NUMBER AND NOT A DURATION. The Edge is a Raspberry Pi with ONE SQLite
// connection, and every station polls the full view at 500 ms while events
// flow (operator.js:34-35 — 3 s is only the idle floor). What an operator
// feels as a hang is the number of statements queued on that connection and
// the CPU spent marshalling JSON, and neither is a wall clock a loaded CI
// runner can hold. A test that asserts on milliseconds is noise; a test that
// counts statements is the property.
//
// The fixture is seedBudgetFixture: 40 styles x 6 positions, 3 presets, 600
// changeovers, 250 catalog rows, 12 routing rows, and a scene in the geometry
// cache. See composer_budget_test.go for why each of those matters.

// composerBlockQueryBudget is what the composer is allowed to add to a poll
// the base already paid for.
//
// THREE, AND THEY ARE NAMED: the process's live claims (the picker's flow
// summaries and part chips), the changeover history (last-run and RECENT),
// and the styles list — except the styles list is handed in by the caller
// that just read it, so the real number is two. The budget is three because
// a fourth would be a per-style read coming back.
const composerBlockQueryBudget = 3

// composerBlockByteBudget is the serialised size of the block the poll
// carries, PRE-GZIP. Gzip takes 40 near-identical style blocks down about
// 30x, so the compressed size hides exactly the growth that costs the Pi its
// CPU; what the box pays is the marshalling.
const composerBlockByteBudget = 22 * 1024

// composerReadByteBudget is the DESKTOP composer read's serialised size,
// pre-gzip, with the plant Map in it.
//
// 200 kB BY OWNER RULING 2026-09-13, up from the 150 kB S0 asked for. The
// original number was set from a composition that measurement contradicts:
// it read as though Map were the bulk of this payload, and at the pre-simplify
// tip Map was 94.0 kB of a 324.4 kB read while the styles array was 192.3 kB.
//
// What closed the gap was duplication, and it is all gone: a map edge no
// longer carries its endpoints' coordinates (Map 108.6 -> 77.2 kB) and an
// Advanced block no longer spells out fourteen zero-valued fields (86.0 ->
// 33.0 kB). 324.4 kB -> 193.6 kB, measured.
//
// What is left is not duplication — it is the map, and 240 cells. Getting
// under 150 would take a columnar encoding of the map or a round trip per
// style click, and this brief's standard is one copy, not minification, and
// calls a round trip a stop. So the number moved and the payload did not.
const composerReadByteBudget = 200 * 1024

// TestComposerRead_DesktopByteBudget holds it, and says what the headroom is
// rather than only whether it passed: a budget that reports nothing until the
// day it fails gives no warning that the margin has gone.
func TestComposerRead_DesktopByteBudget(t *testing.T) {
	t.Parallel()
	fx := seedBudgetFixture(t)

	data, err := fx.svc.ComposerForProcess(fx.processID)
	if err != nil {
		t.Fatalf("ComposerForProcess: %v", err)
	}
	n := len(mustJSON(t, data))
	if n > composerReadByteBudget {
		t.Errorf("the desktop composer read is %d bytes at %d styles, budget %d (%d over).\n"+
			"  Pre-gzip, because what the Pi pays is the marshalling and what the browser "+
			"pays is the parse; gzip hides exactly the growth that costs them.",
			n, budgetStyles, composerReadByteBudget, n-composerReadByteBudget)
	}
	t.Logf("desktop composer read: %d bytes at %d styles, budget %d, headroom %d (%.1f%%)",
		n, budgetStyles, composerReadByteBudget, composerReadByteBudget-n,
		100*float64(composerReadByteBudget-n)/float64(composerReadByteBudget))

	// THE STATION'S READ CARRIES NO MAP, so it is not on this budget and must
	// not quietly acquire one — that is S2.5's rule, and it is the reason the
	// two reads can have different budgets at all.
	station, err := fx.svc.ComposerForStation(fx.processID)
	if err != nil {
		t.Fatalf("ComposerForStation: %v", err)
	}
	if station.Map != nil {
		t.Error("the station's composer read carries the plant map; no screen on the station draws one")
	}
}

// TestStationView_QueryCountAndBytes pins what the composer costs a station
// poll: three queries and 22 kB on top of what the board already read.
//
// Before S8 this block was 86 queries and 142 kB, on every poll of every
// board, and the station read its composer half ONCE — on a tap. The rest
// was built, marshalled, gzipped and discarded.
func TestStationView_QueryCountAndBytes(t *testing.T) {
	fx := seedBudgetFixture(t)

	styles, err := fx.db.ListStylesByProcess(fx.processID)
	if err != nil {
		t.Fatalf("list styles: %v", err)
	}
	fx.counter.Reset()
	block := fx.svc.buildComposerData(fx.processID, styles, fx.svc.liveClaimsByStyle(fx.processID), composerPicker)
	queries := fx.counter.Count()
	if queries > composerBlockQueryBudget {
		t.Errorf("the composer block costs the poll %d queries, budget %d — a per-style read has come back",
			queries, composerBlockQueryBudget)
	}

	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(raw) > composerBlockByteBudget {
		t.Errorf("the composer block serialises to %d bytes, budget %d — at 500 ms per board that is "+
			"what the Pi marshals and the HMI parses", len(raw), composerBlockByteBudget)
	}

	// AND THE BLOCK IS THE PICKER'S, not the composer's. These four are the
	// payload S8 moved to its own fetch; a nil check is what stops one of them
	// drifting back onto the poll.
	if block.Routing != nil || block.Presets != nil || block.Scene != nil || block.Map != nil {
		t.Errorf("the poll carries routing=%v presets=%v scene=%v map=%v; all four belong to the "+
			"composer's own read", block.Routing != nil, block.Presets != nil, block.Scene != nil, block.Map != nil)
	}
	for _, s := range block.Styles {
		if len(s.Claims) != 0 || s.Advanced != nil {
			t.Fatalf("style %q rides the poll with %d cells and advanced=%v", s.Name, len(s.Claims), s.Advanced != nil)
		}
	}
}

// TestComposerRead_QueryCount pins both composer reads as CONSTANT IN N: the
// number of styles on the press does not change how many statements either
// one issues.
//
// It was 130 queries — a read per style, three times over (the builder, the
// Advanced blocks, the preset member counts) — which at Springfield's 40-90
// styles is the whole feature queued on one connection.
func TestComposerRead_QueryCount(t *testing.T) {
	const budget = 12
	fx := seedBudgetFixture(t)

	fx.counter.Reset()
	if _, err := fx.svc.ComposerForStation(fx.processID); err != nil {
		t.Fatalf("ComposerForStation: %v", err)
	}
	if n := fx.counter.Count(); n > budget {
		t.Errorf("the station's composer read issues %d queries, budget %d", n, budget)
	}

	fx.counter.Reset()
	if _, err := fx.svc.ComposerForProcess(fx.processID); err != nil {
		t.Fatalf("ComposerForProcess: %v", err)
	}
	if n := fx.counter.Count(); n > budget {
		t.Errorf("the desktop's composer read issues %d queries, budget %d", n, budget)
	}
}

// TestPresetsFor_QueryCount pins the Presets tab as constant in N. It was
// N+2: one claims read per style, for a page that is a list of shapes.
func TestPresetsFor_QueryCount(t *testing.T) {
	const budget = 6
	fx := seedBudgetFixture(t)
	svc := NewProcessService(fx.db)

	fx.counter.Reset()
	if _, err := svc.PresetsFor(fx.processID); err != nil {
		t.Fatalf("PresetsFor: %v", err)
	}
	if n := fx.counter.Count(); n > budget {
		t.Errorf("PresetsFor issues %d queries at %d styles, budget %d — it is reading claims per style again",
			n, budgetStyles, budget)
	}
}

// TestComposerBlock_PresetMemberCountsShortCircuit: a process with NO presets
// issues no member-count work at all.
//
// Every process starts here and most stay here, and the count was N+1 queries
// per poll to build a map that was then indexed zero times — 41 of the poll's
// 86 queries at launch.
func TestComposerBlock_PresetMemberCountsShortCircuit(t *testing.T) {
	fx := seedBudgetFixture(t)
	presets, err := fx.db.ListFlowPresets(fx.processID, true)
	if err != nil {
		t.Fatalf("list presets: %v", err)
	}
	for _, p := range presets {
		if err := fx.db.ArchiveFlowPreset(p.ID); err != nil {
			t.Fatalf("archive %d: %v", p.ID, err)
		}
	}

	claims := fx.svc.liveClaimsByStyle(fx.processID)
	// Read before the count: the press order is the CALLER's, read once per
	// composer build and handed in, not part of what the strip costs.
	order := fx.svc.pressOrder(fx.processID)

	fx.counter.Reset()
	withPresets := fx.svc.composerPresets(fx.processID, claims, order)
	if n := fx.counter.Count(); n > 1 {
		t.Errorf("a process with no presets issues %d queries building its (empty) strip, want 1 — "+
			"the list read itself and nothing else", n)
	}
	if withPresets != nil {
		t.Errorf("a process with no live presets offered %d cards", len(withPresets))
	}
}

// TestStationView_BuildsInOnePass is the whole-view number, so a regression
// anywhere on the poll path shows up even if the composer block itself is
// still inside its budget.
func TestStationView_BuildsInOnePass(t *testing.T) {
	// main built this view in 31 queries at this fixture's size; the composer
	// is allowed three more.
	const budget = 31 + composerBlockQueryBudget
	fx := seedBudgetFixture(t)

	fx.counter.Reset()
	if _, err := fx.svc.BuildView(context.Background(), fx.stationID); err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if n := fx.counter.Count(); n > budget {
		t.Errorf("a station poll issues %d queries, budget %d", n, budget)
	}
}
