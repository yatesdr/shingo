package service

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
)

// station_position_removal_test.go — what the screen sheet is told before a
// save takes a position off a station. Plant A's first press: Screen A4 owns
// PLN_01, PLN_03, PLN_04 and PLN_06, its styles claim all four, and its
// active style is PART SYN-A-S006.

// removalFixture seeds the press, its routing set, and one recipe — the
// two-robot swap its styles already run on PLN_03 / PLN_06 — named "Swap pair".
func removalFixture(t *testing.T) (*StationService, int64) {
	t.Helper()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")
	testdb.SeedRoutingSet(t, db, seeded.ProcessID, "test")
	presets := NewProcessService(db)
	view, err := presets.PresetsFor(seeded.ProcessID)
	testutil.MustNoErr(t, err, "PresetsFor")
	named := false
	for _, c := range view.Candidates {
		if strings.Contains(c.Where, "PLN_03") {
			_, err := presets.CreateFromCandidate(seeded.ProcessID, "Swap pair", c.ShapeKey, "test")
			testutil.MustNoErr(t, err, "CreateFromCandidate")
			named = true
			break
		}
	}
	if !named {
		t.Fatal("setup: no candidate shape on PLN_03 to name")
	}
	return NewStationService(db), seeded.Stations["screen-a4"]
}

// without is the station's own list less the names given.
func without(t *testing.T, svc *StationService, stationID int64, drop ...string) []string {
	t.Helper()
	names, err := svc.GetNodeNames(stationID)
	testutil.MustNoErr(t, err, "GetNodeNames")
	gone := map[string]bool{}
	for _, d := range drop {
		gone[d] = true
	}
	var out []string
	for _, n := range names {
		if !gone[n] {
			out = append(out, n)
		}
	}
	if len(out) != len(names)-len(drop) {
		t.Fatalf("setup: the station does not own all of %v (it owns %v)", drop, names)
	}
	return out
}

// TestRemovalImpact_ARecipeThatClaimsThePositionIsNamed: taking PLN_03 off
// the screen names the recipe that claims it, and the styles.
func TestRemovalImpact_ARecipeThatClaimsThePositionIsNamed(t *testing.T) {
	t.Parallel()
	svc, station := removalFixture(t)
	got, err := svc.RemovalImpact(station, without(t, svc, station, "PLN_03"))
	testutil.MustNoErr(t, err, "RemovalImpact")
	want := `Removing PLN_03 leaves recipe "Swap pair" and 3 styles "PART SYN-A-S002", "PART SYN-A-S007" and ` +
		`"PART SYN-A-S008" naming a position this process no longer has. Save again to remove it anyway.`
	if got != want {
		t.Errorf("removing PLN_03:\n got %q\nwant %q", got, want)
	}
}

// TestLooseRemovalImpact_IsTheScreensWarningForPositionsOnNoScreen: the
// press's positions on no screen get the screen's warning. Plant A's press
// already has two (PLN_02, PLN_05); a loose list that keeps them warns about
// nothing, though it leaves out every position on Screen A4 — those are not
// the loose list's to remove. Then the screen's rows let go of theirs, as a
// press set up before its screen has them, and dropping PLN_03 from the loose
// list is told the sentence the screen's sheet is told.
func TestLooseRemovalImpact_IsTheScreensWarningForPositionsOnNoScreen(t *testing.T) {
	t.Parallel()
	svc, station := removalFixture(t)
	st, err := svc.db.GetOperatorStation(station)
	testutil.MustNoErr(t, err, "GetOperatorStation")
	loose := looseNames(t, svc, st.ProcessID)
	if len(loose) == 0 {
		t.Fatal("setup: plant A's press has no position on no screen to keep")
	}
	onScreen := without(t, svc, station, "PLN_03")

	got, err := svc.LooseRemovalImpact(st.ProcessID, loose)
	testutil.MustNoErr(t, err, "LooseRemovalImpact with the screen's positions left out")
	if got != "" {
		t.Errorf("a loose list warned about positions on a screen: %q", got)
	}

	_, err = svc.db.Exec(`UPDATE process_nodes SET operator_station_id = NULL WHERE operator_station_id = ?`, station)
	testutil.MustNoErr(t, err, "take the positions off the screen")
	got, err = svc.LooseRemovalImpact(st.ProcessID, append(append([]string{"PLN_03"}, loose...), onScreen...))
	testutil.MustNoErr(t, err, "LooseRemovalImpact keeping all")
	if got != "" {
		t.Errorf("a loose list that removes nothing warned: %q", got)
	}
	got, err = svc.LooseRemovalImpact(st.ProcessID, append(append([]string{}, loose...), onScreen...))
	testutil.MustNoErr(t, err, "LooseRemovalImpact")
	want := `Removing PLN_03 leaves recipe "Swap pair" and 3 styles "PART SYN-A-S002", "PART SYN-A-S007" and ` +
		`"PART SYN-A-S008" naming a position this process no longer has. Save again to remove it anyway.`
	if got != want {
		t.Errorf("removing PLN_03 on no screen:\n got %q\nwant %q", got, want)
	}
}

// TestRemovalImpact_TheActiveStyleIsCalledOut: the style the press is running
// is marked among the styles that claim the position.
func TestRemovalImpact_TheActiveStyleIsCalledOut(t *testing.T) {
	t.Parallel()
	svc, station := removalFixture(t)
	got, err := svc.RemovalImpact(station, without(t, svc, station, "PLN_01"))
	testutil.MustNoErr(t, err, "RemovalImpact")
	want := `Removing PLN_01 leaves 7 styles "PART SYN-A-S001", "PART SYN-A-S003", "PART SYN-A-S004", ` +
		`"PART SYN-A-S005", "PART SYN-A-S006" (the active style), "PART SYN-A-S009" and "PART SYN-A-S010" ` +
		`naming a position this process no longer has. Save again to remove it anyway.`
	if got != want {
		t.Errorf("removing PLN_01:\n got %q\nwant %q", got, want)
	}
}

// TestRemovalImpact_SeveralPositionsAreEachNamed: two positions with their
// own claimants, one sentence, each position with what names it.
func TestRemovalImpact_SeveralPositionsAreEachNamed(t *testing.T) {
	t.Parallel()
	svc, station := removalFixture(t)
	got, err := svc.RemovalImpact(station, without(t, svc, station, "PLN_03", "PLN_06"))
	testutil.MustNoErr(t, err, "RemovalImpact")
	want := `Removing PLN_03 and PLN_06 leaves these naming positions this process no longer has — ` +
		`PLN_03: recipe "Swap pair"; 3 styles "PART SYN-A-S002", "PART SYN-A-S007" and "PART SYN-A-S008". ` +
		`PLN_06: recipe "Swap pair"; 3 styles "PART SYN-A-S002", "PART SYN-A-S007" and "PART SYN-A-S008". ` +
		`Save again to remove them anyway.`
	if got != want {
		t.Errorf("removing PLN_03 and PLN_06:\n got %q\nwant %q", got, want)
	}
}

// TestRemovalImpact_ARecipesOlderVersionIsNotNamed: a recipe's enabled version is
// the one on the card, so a position only an older version stands on orphans
// nothing, and the warning names the recipe only while the card would break.
func TestRemovalImpact_ARecipesOlderVersionIsNotNamed(t *testing.T) {
	t.Parallel()
	svc, station := removalFixture(t)
	st, err := svc.db.GetOperatorStation(station)
	testutil.MustNoErr(t, err, "station")
	rows, err := svc.db.ListFlowPresets(st.ProcessID, false)
	testutil.MustNoErr(t, err, "recipes")
	cells, err := domain.ParsePresetShape(rows[0].FlowJSON)
	testutil.MustNoErr(t, err, "parse")
	for i := range cells {
		if cells[i].CoreNodeName == "PLN_03" {
			cells[i].CoreNodeName = "PLN_01"
		}
		if cells[i].PairedCoreNode == "PLN_03" {
			cells[i].PairedCoreNode = "PLN_01"
		}
	}
	_, err = NewProcessService(svc.db).CreateFromCells(st.ProcessID, "Swap pair", "test", cells, nil)
	testutil.MustNoErr(t, err, "version 2, off PLN_03")

	got, err := svc.RemovalImpact(station, without(t, svc, station, "PLN_03"))
	testutil.MustNoErr(t, err, "RemovalImpact")
	if strings.Contains(got, "Swap pair") {
		t.Errorf("removing PLN_03 names the recipe whose current version left it: %q", got)
	}
	got, err = svc.RemovalImpact(station, without(t, svc, station, "PLN_01"))
	testutil.MustNoErr(t, err, "RemovalImpact")
	if !strings.Contains(got, `recipe "Swap pair"`) {
		t.Errorf("removing PLN_01 does not name the recipe now standing on it: %q", got)
	}
}

// TestRemovalImpact_APositionNothingClaimsHasNoWarning: a position no recipe
// or style names, and that holds none of the settings a retire clears
// (TestRemovalImpact_WhatThePositionTakesWithItIsNamed), goes without a word,
// and so does a save that removes nothing.
func TestRemovalImpact_APositionNothingClaimsHasNoWarning(t *testing.T) {
	t.Parallel()
	svc, station := removalFixture(t)
	own := without(t, svc, station)
	if _, err := svc.SetNodes(station, append(append([]string{}, own...), "PLN_09")); err != nil {
		t.Fatalf("setup: SetNodes: %v", err)
	}
	got, err := svc.RemovalImpact(station, own)
	testutil.MustNoErr(t, err, "RemovalImpact")
	if got != "" {
		t.Errorf("removing PLN_09, which nothing claims, warned: %q", got)
	}
	got, err = svc.RemovalImpact(station, append(own, "PLN_09"))
	testutil.MustNoErr(t, err, "RemovalImpact")
	if got != "" {
		t.Errorf("a save that removes nothing warned: %q", got)
	}
}

// TestRemovalImpact_WhatThePositionTakesWithItIsNamed: taking a position off
// retires its row and adding the name back makes a new one, so what a person
// set on the old row and its runtime row does not come back: the light
// curtain, the station counter it is counted at, and a person's auto-reorder
// off. The warning names each, after what still names the position, and the
// last part of the test is the fact the sentence states: a take-off and a
// re-add leave a new row with none of the three.
func TestRemovalImpact_WhatThePositionTakesWithItIsNamed(t *testing.T) {
	t.Parallel()
	svc, station := removalFixture(t)
	st, err := svc.db.GetOperatorStation(station)
	testutil.MustNoErr(t, err, "GetOperatorStation")
	own := without(t, svc, station)
	if _, err := svc.SetNodes(station, append(append([]string{}, own...), "PLN_09")); err != nil {
		t.Fatalf("setup: SetNodes: %v", err)
	}
	ids := map[string]int64{}
	rows, err := svc.db.ListProcessNodesByStation(station)
	testutil.MustNoErr(t, err, "ListProcessNodesByStation")
	for _, n := range rows {
		ids[n.CoreNodeName] = n.ID
	}
	on := true
	for _, name := range []string{"PLN_03", "PLN_09"} {
		testutil.MustNoErr(t, processes.SetNodeCurtain(svc.db.DB, ids[name], true, "PLC_T", "Curtain_"+name, &on), "curtain "+name)
	}
	counter, err := svc.db.CreateLineCounter(st.ProcessID, "Station 2", "PLC_T", "Count_T", "")
	testutil.MustNoErr(t, err, "CreateLineCounter")
	testutil.MustNoErr(t, svc.db.SetNodeLineCounter(ids["PLN_09"], &counter), "count PLN_09 at Station 2")
	if held, err := processes.HoldReorder(svc.db.DB, ids["PLN_09"], domain.ReorderHoldPersonOff, time.Now(), "Pat"); err != nil || !held {
		t.Fatalf("setup: a person's off at PLN_09: held %v, %v", held, err)
	}

	got, err := svc.RemovalImpact(station, own)
	testutil.MustNoErr(t, err, "RemovalImpact")
	want := `PLN_09 has a light curtain, is counted at Station 2 and has auto-reorder switched off; taking it off ` +
		`clears all three, and adding it back does not restore them. Save again to remove it anyway.`
	if got != want {
		t.Errorf("removing PLN_09, which nothing names:\n got %q\nwant %q", got, want)
	}

	got, err = svc.RemovalImpact(station, without(t, svc, station, "PLN_03", "PLN_09"))
	testutil.MustNoErr(t, err, "RemovalImpact")
	want = `Removing PLN_03 leaves recipe "Swap pair" and 3 styles "PART SYN-A-S002", "PART SYN-A-S007" and ` +
		`"PART SYN-A-S008" naming a position this process no longer has. PLN_03 has a light curtain; taking it ` +
		`off clears that, and adding it back does not restore it. PLN_09 has a light curtain, is counted at ` +
		`Station 2 and has auto-reorder switched off; taking it off clears all three, and adding it back does ` +
		`not restore them. Save again to remove them anyway.`
	if got != want {
		t.Errorf("removing PLN_03 and PLN_09:\n got %q\nwant %q", got, want)
	}

	// The loose list is told the same: PLN_09 on no screen, beside the two
	// positions plant A's press already has there, which the list keeps.
	loose := looseNames(t, svc, st.ProcessID)
	_, err = svc.db.Exec(`UPDATE process_nodes SET operator_station_id = NULL WHERE id = ?`, ids["PLN_09"])
	testutil.MustNoErr(t, err, "take PLN_09 off the screen")
	got, err = svc.LooseRemovalImpact(st.ProcessID, loose)
	testutil.MustNoErr(t, err, "LooseRemovalImpact")
	want = `PLN_09 has a light curtain, is counted at Station 2 and has auto-reorder switched off; taking it off ` +
		`clears all three, and adding it back does not restore them. Save again to remove it anyway.`
	if got != want {
		t.Errorf("dropping PLN_09 from the positions on no screen:\n got %q\nwant %q", got, want)
	}

	// What the sentence says: off and back on, PLN_09 is a new row with none of it.
	if _, err := svc.SetLoosePositions(st.ProcessID, loose); err != nil {
		t.Fatalf("take PLN_09 off: %v", err)
	}
	if _, err := svc.SetLoosePositions(st.ProcessID, append(append([]string{}, loose...), "PLN_09")); err != nil {
		t.Fatalf("add PLN_09 back: %v", err)
	}
	nodes, err := svc.db.ListProcessNodesByProcess(st.ProcessID)
	testutil.MustNoErr(t, err, "ListProcessNodesByProcess")
	var back *processes.Node
	for i := range nodes {
		if nodes[i].CoreNodeName == "PLN_09" {
			back = &nodes[i]
		}
	}
	if back == nil {
		t.Fatal("PLN_09 is not a position after it was added back")
	}
	if back.ID == ids["PLN_09"] {
		t.Errorf("PLN_09 came back as its old row %d; the warning says a re-add starts a new one", back.ID)
	}
	if back.CurtainEnabled || back.CurtainTagName != "" || back.LineCounterID != nil {
		t.Errorf("PLN_09 came back with curtain %v %q and counter %v; the warning says they are gone",
			back.CurtainEnabled, back.CurtainTagName, back.LineCounterID)
	}
	rt, err := svc.db.GetProcessNodeRuntime(back.ID)
	testutil.MustNoErr(t, err, "GetProcessNodeRuntime")
	if rt != nil && rt.ReorderHold != domain.ReorderHoldNone {
		t.Errorf("PLN_09 came back with auto-reorder hold %q; the warning says it is gone", rt.ReorderHold)
	}
}

// TestRemovalImpact_Statements: the warning is read only on the remove path,
// and costs the same at 40 styles as at 4. A list that removes nothing reads
// the station and its positions, and stops: 2. One that removes a position
// adds the recipes, the claims, the styles, the process and the removed
// positions' runtime rows (a person's auto-reorder off): 7. One that removes
// a position counted at a station counter adds the counters, for the
// counter's name: 8. All reads.
func TestRemovalImpact_Statements(t *testing.T) {
	fx := seedBudgetFixture(t)
	own, err := fx.svc.GetNodeNames(fx.stationID)
	testutil.MustNoErr(t, err, "GetNodeNames")
	rows, err := fx.db.ListProcessNodesByStation(fx.stationID)
	testutil.MustNoErr(t, err, "ListProcessNodesByStation")
	var counted int64
	for _, n := range rows {
		if n.CoreNodeName == own[1] {
			counted = n.ID
		}
	}
	for _, tc := range []struct {
		name    string
		keep    []string
		counted bool
		total   int64
	}{
		{"removes nothing", own, false, 2},
		{"removes one", own[1:], false, 7},
		{"removes one counted at a station counter", append([]string{own[0]}, own[2:]...), true, 8},
	} {
		if tc.counted {
			lc, err := fx.db.CreateLineCounter(fx.processID, "Station 2", "PLC_T", "Count_T", "")
			testutil.MustNoErr(t, err, "CreateLineCounter")
			testutil.MustNoErr(t, fx.db.SetNodeLineCounter(counted, &lc), "SetNodeLineCounter")
		}
		fx.counter.Reset()
		if _, err := fx.svc.RemovalImpact(fx.stationID, tc.keep); err != nil {
			t.Fatalf("%s: RemovalImpact: %v", tc.name, err)
		}
		if got, reads := fx.counter.Count(), fx.counter.Reads(); got != tc.total || reads != tc.total {
			t.Errorf("%s: %d statements (%d reads), pinned at %d, all reads", tc.name, got, reads, tc.total)
		}
	}
}

// looseNames is the process's positions on no screen, as the store holds them.
func looseNames(t *testing.T, svc *StationService, processID int64) []string {
	t.Helper()
	nodes, err := svc.db.ListProcessNodesByProcess(processID)
	testutil.MustNoErr(t, err, "ListProcessNodesByProcess")
	out := []string{}
	for _, n := range onNoScreen(nodes) {
		out = append(out, n.CoreNodeName)
	}
	return out
}
