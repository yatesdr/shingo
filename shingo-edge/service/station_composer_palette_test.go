package service

import (
	"context"
	"slices"
	"testing"

	"shingo/shared/scenefixtures"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
)

// station_composer_palette_test.go — the part set on the composer block.
//
// TWO RULES, AND THE SECOND IS THE BUDGET. The palette must reach both
// composer reads, because a part picker with nothing in it is the bug this
// work exists to fix; and it must NOT reach the poll, because the poll is 22 kB
// on a Pi at 500 ms a board and a part set is a list that grows with the plant.
// composer_budget_pins_test.go holds the second one as a byte number; this
// holds it as the field itself, which is the thing that would move.

func TestComposerPalette_ReachesBothComposerReadsAndNotThePoll(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")
	svc := NewStationService(db)

	if err := db.ReplaceProcessPayloads(seeded.ProcessID, []string{"TYPED-PART"}); err != nil {
		t.Fatalf("ReplaceProcessPayloads: %v", err)
	}

	station, err := svc.ComposerForStation(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ComposerForStation: %v", err)
	}
	if !slices.Contains(station.Palette, "TYPED-PART") {
		t.Errorf("the station's composer read carries palette %v, without the typed part — "+
			"the HMI's part picker offers this list and nothing else", station.Palette)
	}

	desktop, err := svc.ComposerForProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ComposerForProcess: %v", err)
	}
	if !slices.Contains(desktop.Palette, "TYPED-PART") {
		t.Errorf("the desktop's composer read carries palette %v, without the typed part", desktop.Palette)
	}

	// THE POLL, ASKED THROUGH THE POLL. The palette must sit below the picker
	// block's early return: a list of every part a cell can run, rebuilt and
	// discarded on every 500 ms poll of every board.
	//
	// BuildView rather than buildComposerData, deliberately. The internal
	// builder takes the scope as an argument, so a test that called it would
	// prove the picker SCOPE is clean and say nothing about what a board
	// actually receives — which is the fact this is about, and the one that
	// would change if a caller passed the wrong scope.
	stationID := int64(0)
	for _, id := range seeded.Stations {
		if stationID == 0 || id < stationID {
			stationID = id
		}
	}
	view, err := svc.BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if view.Composer == nil {
		t.Fatal("the poll carries no composer block at all")
	}
	if view.Composer.Palette != nil {
		t.Errorf("the poll carries a palette of %d parts; it belongs to the composer's own read",
			len(view.Composer.Palette))
	}
}

// ONE QUERY, ON A READ THAT HAPPENS ONCE. The palette is a single union read;
// a per-style or per-part read here would be the shape S8 spent the whole of
// its budget removing.
func TestComposerPalette_CostsTheStationReadOneQuery(t *testing.T) {
	fx := seedBudgetFixture(t)

	fx.counter.Reset()
	before, err := fx.svc.ComposerForStation(fx.processID)
	if err != nil {
		t.Fatalf("ComposerForStation: %v", err)
	}
	baseline := fx.counter.Count()
	if before.Palette == nil {
		t.Fatal("the station's composer read carries no palette at all")
	}

	// Ten typed rows, and the read must cost exactly what it cost with none.
	codes := make([]string, 0, 10)
	for i := range 10 {
		codes = append(codes, "PALETTE-PART-"+string(rune('A'+i)))
	}
	if err := fx.db.ReplaceProcessPayloads(fx.processID, codes); err != nil {
		t.Fatalf("ReplaceProcessPayloads: %v", err)
	}
	fx.counter.Reset()
	if _, err := fx.svc.ComposerForStation(fx.processID); err != nil {
		t.Fatalf("ComposerForStation: %v", err)
	}
	if n := fx.counter.Count(); n != baseline {
		t.Errorf("the station's composer read issues %d queries with 10 typed parts and %d with none — "+
			"the palette is constant in the size of the part set or it is not one read", n, baseline)
	}
}

// ── the duplicated style fields (owner, 2026-09-17) ──────────────────────────
//
// CARRYING THE SAME DATA TWICE IS NOT KEPT. ComposerStyle.Nodes is the
// core_node_name of every claim and .Parts is the distinct payloads with the
// first position that carries each — and on any scope that carries Claims,
// both are already in Claims, on the same object, spelled out again. Measured
// at the budget fixture: 2,290 and 13,018 bytes of a 201 kB read.
//
// THE WIDENING SEQUENCE FORKS, the way Scene and Map already do. The picker
// scope has no Claims, so it keeps both; the two composer scopes drop them and
// composer-model's styleFacts derives them from Claims — one helper, both
// surfaces. This is the pin the invariant's comment names.
func TestComposerStyleFields_TheDerivableOnesRideOnlyThePickerScope(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")
	svc := NewStationService(db)
	styles, err := db.ListStylesByProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ListStylesByProcess: %v", err)
	}
	claims := svc.liveClaimsByStyle(seeded.ProcessID)

	// THE PICKER SCOPE KEEPS THEM. It has no Claims — a board polling at
	// 500 ms must not collapse forty styles' claims into cells it will not
	// draw — so its rows would have nothing to build a flow summary from.
	picker := svc.buildComposerData(seeded.ProcessID, styles, claims, composerPicker, nil)
	withSummary := 0
	for _, s := range picker.Styles {
		if s.ClaimCount == 0 {
			continue
		}
		if len(s.Nodes) == 0 || len(s.Parts) == 0 {
			t.Errorf("picker row %q has claim_count %d and nodes=%d parts=%d; the summary and the "+
				"set-up chips are built from these and the poll carries no cells",
				s.Name, s.ClaimCount, len(s.Nodes), len(s.Parts))
		}
		withSummary++
	}
	if withSummary == 0 {
		t.Fatal("the fixture seeded no style with claims; this test proves nothing")
	}

	// AND THE COMPOSER SCOPES DROP THEM, because they carry Claims.
	for _, tc := range []struct {
		name  string
		scope composerScope
	}{{"station", composerStation}, {"desktop", composerDesktop}} {
		block := svc.buildComposerData(seeded.ProcessID, styles, claims, tc.scope,
			func() []processes.Node { return nil })
		carried := 0
		for _, s := range block.Styles {
			if len(s.Claims) == 0 {
				continue
			}
			carried++
			if s.Nodes != nil || s.Parts != nil {
				t.Errorf("%s scope: style %q carries nodes=%v parts=%v beside %d cells that already "+
					"say both", tc.name, s.Name, s.Nodes, s.Parts, len(s.Claims))
			}
		}
		if carried == 0 {
			t.Errorf("%s scope: no style carried cells, so the drop is untested", tc.name)
		}
	}
}
