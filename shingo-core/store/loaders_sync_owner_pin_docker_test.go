//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/loaders"
	"shingocore/store/plantclaims"
)

// A loader's registry rows belong to the station that owns its position
// (RULINGS-D2-exchange-at-empty §3.3; TRACE-line2-empties §7.1). Every station
// used to derive every loader, so on the two-Edge plant line 2's loaders had
// rows under edge1.line1 too, and all 13 of its PART-W loader orders went out
// addressed to the Edge that does not own the node. A position no station owns
// yet (an older Edge that reports no positions, or before its first plant.claims)
// stays with every station, as before, so no plant loses a threshold. RED at
// 7a341f92: edge1 derives the loader edge2 owns, and edge2 the one edge1 owns.
func TestDemandRegistryPin_ALoaderIsDerivedForTheStationThatOwnsItsPosition(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	mk := func(name, node, payload string) {
		t.Helper()
		n := seedRegistryNode(t, db, "NT-OWN", node)
		id, err := db.CreateLoader(loaders.Loader{Name: name, Role: loaders.RoleProduce,
			Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold})
		if err != nil {
			t.Fatalf("CreateLoader %s: %v", name, err)
		}
		if err := db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: n, PayloadCode: payload, UOPThreshold: 10}); err != nil {
			t.Fatalf("UpsertLoaderHome %s: %v", name, err)
		}
	}
	mk("OWN-1", "OWN-W1", "P-1")
	mk("OWN-2", "OWN-W2", "P-2")
	mk("OWN-X", "OWN-WX", "P-X")
	for _, p := range []struct{ process, station, node string }{
		{"OWN-LOADER-1", "edge1.line1", "OWN-W1"},
		{"OWN-LOADER-2", "edge2.line1", "OWN-W2"},
	} {
		if _, err := db.ReplacePlantClaimsFromStation(p.process, p.station,
			[]plantclaims.StyleRow{{ProcessID: p.process, StyleID: "RUN", IsActive: true}}, nil, []string{p.node}, 0); err != nil {
			t.Fatalf("plant.claims %s: %v", p.process, err)
		}
	}

	for station, want := range map[string]map[string]bool{
		"edge1.line1": {"OWN-W1": true, "OWN-WX": true},
		"edge2.line1": {"OWN-W2": true, "OWN-WX": true},
	} {
		entries, _ := deriveAndSync(t, db, station)
		got := map[string]bool{}
		for _, e := range entries {
			got[e.CoreNodeName] = true
		}
		if len(got) != len(want) {
			t.Errorf("%s derived %v; want %v", station, got, want)
			continue
		}
		for n := range want {
			if !got[n] {
				t.Errorf("%s derived %v; want %v", station, got, want)
				break
			}
		}
	}
}
