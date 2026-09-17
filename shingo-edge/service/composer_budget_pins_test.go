package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"shingoedge/store/processes"
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
// 192 KiB, RE-RULED 2026-09-17 FROM THE MEASUREMENT, down from the 200 kB
// owner ruling of 09-13. The history is worth keeping because every move of
// this number has been duplication leaving, never minification:
//
//	324.4 kB  the pre-simplify tip
//	193.6 kB  a map edge stopped carrying its endpoints' coordinates
//	          (Map 108.6 -> 77.2) and an Advanced block stopped spelling out
//	          fourteen zero-valued fields (86.0 -> 33.0)
//	200.2 kB  the part set arrived (+4.5 kB at this fixture's 240 distinct
//	          payloads) and the cell gained its staging band
//	184.0 kB  ComposerStyle.Nodes and .Parts left the scopes that carry
//	          Claims — 15.3 kB of the same strings twice on one object — and
//	          the LM coordinate list went with the drawing that needed it
//
// WHY 192 AND NOT 180. The ceiling has to be tight enough that putting the
// duplication back FAILS: restoring Nodes and Parts here is +16.3 kB, which
// 192 catches and 200 did not. Above that it has to leave room for the two
// things that legitimately grow — the palette with the plant's part set, and
// the map with the plant — so it is not pinned to the measurement. 8.5 KiB of
// headroom (6.4%) is the trade, and TestComposerRead_DesktopByteBudget prints
// it on every run so the margin cannot quietly go.
//
// AND THE FIXTURE IS PESSIMISTIC ABOUT THE PALETTE, deliberately: 240 distinct
// payload codes on one process, where Springfield's biggest runs 40-90. The
// palette's real cost at a plant is nearer 0.8 kB than 4.5.
const composerReadByteBudget = 192 * 1024

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
	block := fx.svc.buildComposerData(fx.processID, styles, fx.svc.liveClaimsByStyle(fx.processID), composerPicker, nil)
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
	// SAYS THE NUMBER, for the same reason the byte budget does: a budget that
	// reports nothing until the day it fails gives no warning that the margin
	// has gone — and this one's comment was wrong about its own cost for
	// exactly as long as nobody could read the count off a passing run.
	n := fx.counter.Count()
	if n > budget {
		t.Errorf("PresetsFor issues %d queries at %d styles, budget %d — it is reading claims per style again",
			n, budgetStyles, budget)
	} else {
		t.Logf("PresetsFor: %d queries at %d styles, budget %d", n, budgetStyles, budget)
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

	// THE NODE LIST IS INSIDE THE COUNT NOW, and that is the point of P2. It
	// used to be read by this test BEFORE the counter was reset and handed in
	// as a value — so a cell with zero presets paid a node-list query for an
	// order nothing would print, and the pin could not see it. The lazy form
	// is passed in here exactly as the service passes it, and the budget is
	// the one read that is genuinely needed: the preset list itself.
	fx.counter.Reset()
	withPresets := fx.svc.composerPresets(fx.processID, claims,
		func() []processes.Node { return fx.svc.processNodes(fx.processID) })
	if n := fx.counter.Count(); n > 1 {
		t.Errorf("a process with no presets issues %d queries building its (empty) strip, want 1 — "+
			"the list read itself and nothing else. A node list read for the card ORDER, on a "+
			"process with no cards, is the one this pin exists to catch", n)
	}
	if withPresets != nil {
		t.Errorf("a process with no live presets offered %d cards", len(withPresets))
	}
}

// TestStationView_BuildsInOnePass is the whole-view number, so a regression
// anywhere on the poll path shows up even if the composer block itself is
// still inside its budget.
func TestStationView_BuildsInOnePass(t *testing.T) {
	const budget = stationPollQueryBudget
	fx := seedBudgetFixture(t)

	fx.counter.Reset()
	if _, err := fx.svc.BuildView(context.Background(), fx.stationID); err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if n := fx.counter.Count(); n > budget {
		t.Errorf("a station poll issues %d queries, budget %d", n, budget)
	} else {
		t.Logf("station poll: %d queries, budget %d", n, budget)
	}
}

// stationPollQueryBudget is what one station poll costs at the budget
// fixture's plant: the board's own reads, plus the composer PICKER block's
// three, and no cell picture.
//
// 33 BEFORE, 31 NOW. The two that left are the cell picture's — the process's
// whole node list, and the partner-slot scan over every live claim
// (ListBackPositionNames) — which rode every poll of every board at 500 ms on
// a Pi with one SQLite connection, for a drawing that changes when an engineer
// edits a cell (owner ruling 4, 2026-09-17). They are not saved somewhere
// else: the picture is read by its own endpoint, when its version moves.
//
// The ceiling here was 34 while the actual was 33, which is one query of slack
// nobody meant to leave. The exact-equality pin below is what closes that.
const stationPollQueryBudget = 31

// stationViewByteBudget is the WHOLE view's serialised size, pre-gzip, at the
// budget fixture's plant.
//
// P4 — SYNTH-round3 §2.10 asked for this and it never landed, so the only byte
// pin on the poll path covered the composer BLOCK. Everything else on the view
// could grow unobserved, and the cell picture — which is what did grow — was
// not in the block.
//
// The number is a CEILING with headroom reported, like the desktop read's, not
// a golden: the view carries a tile per position and the fixture's board is
// what it is. What it catches is a field arriving on the poll.
const stationViewByteBudget = 40 * 1024

// TestStationView_ByteBudget is P4, and TestStationView_CarriesNoCellPicture
// below is the specific thing it exists to keep off.
func TestStationView_ByteBudget(t *testing.T) {
	fx := seedBudgetFixture(t)

	view, err := fx.svc.BuildView(context.Background(), fx.stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	n := len(mustJSON(t, view))
	if n > stationViewByteBudget {
		t.Errorf("the station view serialises to %d bytes, budget %d (%d over).\n"+
			"  Pre-gzip, and per board every 500 ms while events flow: what the Pi pays is "+
			"the marshalling and what the HMI pays is the parse.", n, stationViewByteBudget, n-stationViewByteBudget)
	}
	t.Logf("station view: %d bytes, budget %d, headroom %d (%.1f%%)",
		n, stationViewByteBudget, stationViewByteBudget-n,
		100*float64(stationViewByteBudget-n)/float64(stationViewByteBudget))
}

// TestStationView_CarriesAVersionAndNoPicture is the budget's other half, and
// the one that says what the rule IS rather than what the number is.
//
// AN IDLE POLL CARRIES A PICTURE VERSION AND NO PICTURE, and runs neither of
// the two queries the picture used to cost. A byte budget alone would pass on
// a picture that merely got smaller.
func TestStationView_CarriesAVersionAndNoPicture(t *testing.T) {
	fx := seedBudgetFixture(t)

	fx.counter.Reset()
	view, err := fx.svc.BuildView(context.Background(), fx.stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if view.CellVersion == "" {
		t.Error("the poll carries no cell-picture version; the page has nothing to compare and " +
			"would either never fetch the picture or fetch it on every poll")
	}
	raw := string(mustJSON(t, view))
	if strings.Contains(raw, `"cell":`) {
		t.Error(`the view carries a "cell" object again — the picture is back on the poll`)
	}
	// AND NEITHER QUERY, held as an EXACT count rather than a ceiling.
	//
	// The counter counts statements; it does not name them, and a second
	// instrument that did would be the "two ways to count" store/query_count.go
	// says not to add. Exact equality is the strong form available: the two
	// reads the picture cost are gone, so one coming back fails here even if
	// something else was removed in the same diff to keep a ceiling satisfied.
	if n := fx.counter.Count(); n != stationPollQueryBudget {
		t.Errorf("a station poll issues %d queries, want exactly %d.\n"+
			"  Two left this path with the cell picture: the process's node list, and the "+
			"partner-slot scan over every live claim (ListBackPositionNames).\n"+
			"  If you REMOVED one, lower stationPollQueryBudget and say so. If you added "+
			"one, it is on every poll of every board at 500 ms.", n, stationPollQueryBudget)
	}
}
