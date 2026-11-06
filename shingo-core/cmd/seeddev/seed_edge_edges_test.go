package main

import "testing"

// seed_edge_edges_test.go — one plant file seeds each Edge with its own
// processes, and a style's pin reaches the Edge's styles row.

// TestSeedEdge_AnEdgeGetsOnlyItsOwnProcesses: the fixture's processes split
// over two Edges; each Edge's database holds its own and none of the other's.
// RED while seedEdge seeds the whole plant into every Edge it is pointed at.
func TestSeedEdge_AnEdgeGetsOnlyItsOwnProcesses(t *testing.T) {
	t.Parallel()
	plant := loadFixture(t)
	if len(plant.Processes) < 2 {
		t.Fatalf("the fixture has %d processes; this needs two", len(plant.Processes))
	}
	moved := plant.Processes[len(plant.Processes)-1].Name
	plant.Processes[len(plant.Processes)-1].Edge = "edge2.line1"
	if err := plant.Validate(); err != nil {
		t.Fatalf("the split fixture: %v", err)
	}

	for _, station := range plant.Edges() {
		db := freshEdgeDB(t)
		if err := seedEdgeDB(db, plant.ForEdge(station), map[string]int64{}); err != nil {
			t.Fatalf("seed %s: %v", station, err)
		}
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM processes WHERE name = ?`, moved).Scan(&n); err != nil {
			t.Fatal(err)
		}
		want := 0
		if station == "edge2.line1" {
			want = 1
		}
		if n != want {
			t.Errorf("%s holds %d row(s) of %s, want %d", station, n, moved, want)
		}
		var total int
		if err := db.QueryRow(`SELECT count(*) FROM processes`).Scan(&total); err != nil {
			t.Fatal(err)
		}
		if exp := len(plant.ForEdge(station).Processes); total != exp {
			t.Errorf("%s holds %d processes, want its own %d", station, total, exp)
		}
	}
}

// TestSeedEdge_AStylesPinIsSeeded: expected_catid on a style lands on the
// Edge's styles row. RED while the seeder writes no pin.
func TestSeedEdge_AStylesPinIsSeeded(t *testing.T) {
	t.Parallel()
	plant := loadFixture(t)
	plant.Styles[0].ExpectedCATID = "1001"
	db := freshEdgeDB(t)
	if err := seedEdgeDB(db, plant, map[string]int64{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var pin string
	if err := db.QueryRow(`SELECT expected_catid FROM styles WHERE name = ?`, plant.Styles[0].Name).Scan(&pin); err != nil {
		t.Fatal(err)
	}
	if pin != "1001" {
		t.Errorf("%s expected_catid = %q, want 1001", plant.Styles[0].Name, pin)
	}
}
