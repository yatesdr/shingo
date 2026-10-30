package service

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
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
// or style names goes without a word, and so does a save that removes nothing.
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

// TestRemovalImpact_Statements: the warning is read only on the remove path,
// and costs the same at 40 styles as at 4. A list that removes nothing reads
// the station and its positions, and stops: 2. One that removes a position
// adds the recipes, the claims, the styles and the process: 6. All reads.
func TestRemovalImpact_Statements(t *testing.T) {
	fx := seedBudgetFixture(t)
	own, err := fx.svc.GetNodeNames(fx.stationID)
	testutil.MustNoErr(t, err, "GetNodeNames")
	for _, tc := range []struct {
		name  string
		keep  []string
		total int64
	}{
		{"removes nothing", own, 2},
		{"removes one", own[1:], 6},
	} {
		fx.counter.Reset()
		if _, err := fx.svc.RemovalImpact(fx.stationID, tc.keep); err != nil {
			t.Fatalf("%s: RemovalImpact: %v", tc.name, err)
		}
		if got, reads := fx.counter.Count(), fx.counter.Reads(); got != tc.total || reads != tc.total {
			t.Errorf("%s: %d statements (%d reads), pinned at %d, all reads", tc.name, got, reads, tc.total)
		}
	}
}
