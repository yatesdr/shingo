package engine

import (
	"context"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// reorder_hold_test.go — the position's auto-reorder hold (domain/reorder_hold.go):
// the one write and its rank, the sweep's gate, and the tile's chip.

// holdAt writes kind at the position through the one write, set at at.
func holdAt(t *testing.T, eng *Engine, nodeID int64, kind domain.ReorderHold, at time.Time, note string) bool {
	t.Helper()
	wrote, err := processes.HoldReorder(eng.db.DB, nodeID, kind, at, note)
	testutil.MustNoErr(t, err, "hold reorder")
	return wrote
}

// TestHoldReorder_TheRankNeverStepsDown: for every pair of kinds, a write over
// a held kind lands only when it ranks at or above it, and both runtime reads
// (the per-node one and the sweep's batched one) read the kind, the stamp and
// the note back. Equal kinds restamp: a burst of pauses is one pause from the
// last.
func TestHoldReorder_TheRankNeverStepsDown(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	second := first.Add(3 * time.Minute)
	for _, held := range domain.ReorderHolds {
		for _, next := range domain.ReorderHolds {
			t.Run(string(held)+" then "+string(next), func(t *testing.T) {
				t.Parallel()
				eng, _, nodeID, _ := keeperFixture(t)
				if !holdAt(t, eng, nodeID, held, first, "first") {
					t.Fatalf("%q over no hold did not write", held)
				}
				lands := next.Rank() >= held.Rank()
				if got := holdAt(t, eng, nodeID, next, second, "second"); got != lands {
					t.Fatalf("%q over %q wrote = %v, want %v", next, held, got, lands)
				}
				want := struct {
					kind domain.ReorderHold
					at   time.Time
					note string
				}{held, first, "first"}
				if lands {
					want.kind, want.at, want.note = next, second, "second"
				}
				one, err := eng.db.GetProcessNodeRuntime(nodeID)
				testutil.MustNoErr(t, err, "read the runtime row")
				batch, err := eng.db.ProcessNodeRuntimes([]int64{nodeID})
				testutil.MustNoErr(t, err, "read the runtime rows")
				for name, rt := range map[string]*processes.RuntimeState{"GetRuntime": one, "RuntimesForNodes": batch[nodeID]} {
					if rt.ReorderHold != want.kind || !rt.ReorderHoldAt.Equal(want.at) || rt.ReorderHoldNote != want.note {
						t.Errorf("%s reads (%q, %v, %q), want (%q, %v, %q)", name,
							rt.ReorderHold, rt.ReorderHoldAt, rt.ReorderHoldNote, want.kind, want.at, want.note)
					}
				}
			})
		}
	}
}

// TestHoldReorder_RefusesAKindThatDoesNotHold: none and an unknown word are not
// holds; clearing is a go's, by its own rule.
func TestHoldReorder_RefusesAKindThatDoesNotHold(t *testing.T) {
	t.Parallel()
	eng, _, nodeID, _ := keeperFixture(t)
	for _, k := range []domain.ReorderHold{domain.ReorderHoldNone, "snoozed"} {
		if _, err := processes.HoldReorder(eng.db.DB, nodeID, k, time.Now(), ""); err == nil {
			t.Errorf("HoldReorder(%q) wrote; it is not a hold", k)
		}
	}
}

// TestLevelSweep_AHeldPositionIsNotAskedFor: a breached, armed cell with
// nothing coming is asked for every pass (TestLevelSweep_ReasksAfterTerminalButNotOnTopOfTheReask)
// unless its position holds. A pause past ReorderPause no longer holds, and the
// sweep asks.
func TestLevelSweep_AHeldPositionIsNotAskedFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		kind domain.ReorderHold
		ago  time.Duration
		asks bool
	}{
		{name: "paused a minute ago", kind: domain.ReorderHoldPaused, ago: time.Minute},
		{name: "paused past its pause", kind: domain.ReorderHoldPaused, ago: domain.ReorderPause + time.Minute, asks: true},
		{name: "off", kind: domain.ReorderHoldOff, ago: 48 * time.Hour},
		{name: "a person's off", kind: domain.ReorderHoldPersonOff, ago: 48 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID, claimID := keeperFixture(t)
			setLevel(t, db, nodeID, claimID, 0)
			holdAt(t, eng, nodeID, tc.kind, eng.reconcilerClock().Now().Add(-tc.ago), "fixture")

			eng.sweepCellLevels()
			want := 0
			if tc.asks {
				want = 1
			}
			if got := countOrders(t, db); got != want {
				t.Fatalf("a breached, armed cell held %s %v ago: %d orders after a sweep, want %d", tc.kind, tc.ago, got, want)
			}
		})
	}
}

// TestBuildView_AHeldPositionShowsItsChip: the tile of an armed position
// carries the kind and the decider's sentence; it carries neither when nothing
// holds, or when the pause has run out.
func TestBuildView_AHeldPositionShowsItsChip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		kind domain.ReorderHold
		ago  time.Duration
		want domain.ReorderHold
	}{
		{name: "nothing held"},
		{name: "paused", kind: domain.ReorderHoldPaused, ago: time.Minute, want: domain.ReorderHoldPaused},
		{name: "a pause run out", kind: domain.ReorderHoldPaused, ago: domain.ReorderPause},
		{name: "off", kind: domain.ReorderHoldOff, want: domain.ReorderHoldOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID, _ := keeperFixture(t)
			node, err := db.GetProcessNode(nodeID)
			testutil.MustNoErr(t, err, "read the node")
			sid, err := db.CreateOperatorStation(stations.Input{ProcessID: node.ProcessID, Name: "KEEPER-STATION"})
			testutil.MustNoErr(t, err, "create station")
			_, err = db.DB.Exec(`UPDATE process_nodes SET operator_station_id=? WHERE id=?`, sid, nodeID)
			testutil.MustNoErr(t, err, "seat the node on the station")
			processes.BumpNodeGeneration()
			if tc.kind != "" {
				holdAt(t, eng, nodeID, tc.kind, time.Now().Add(-tc.ago), "order 7 was cancelled")
			}

			view, err := eng.stationService.BuildView(context.Background(), sid)
			testutil.MustNoErr(t, err, "build the view")
			if len(view.Nodes) != 1 {
				t.Fatalf("nodes = %d, want 1", len(view.Nodes))
			}
			tile := view.Nodes[0]
			if tile.AutoReorderHold != tc.want || (tile.AutoReorderNote != "") != (tc.want != "") {
				t.Fatalf("tile hold = %q, note %q; want hold %q", tile.AutoReorderHold, tile.AutoReorderNote, tc.want)
			}
			wantNote := domain.AutoReorderHeld(tile.ActiveClaim, tile.Runtime, time.Now()).Sentence
			if tile.AutoReorderNote != wantNote {
				t.Errorf("tile note %q, want the decider's %q", tile.AutoReorderNote, wantNote)
			}
		})
	}
}
