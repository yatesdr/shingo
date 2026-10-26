package service

import (
	"testing"

	"shingo/protocol/testutil"
	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
)

// composer_routing_gaps_test.go — the desktop composer's word for a routing
// set that cannot fill a new row. "+ Add a position" fills a row's inbound
// source and outbound destination from the set's lowest enabled row of each
// role (composer-model's defaultRouting); a set with none leaves the field
// empty, and a recipe draft says why beside it, in the server's words.

func TestComposer_RoutingGapsNameWhatTheSetLacks(t *testing.T) {
	t.Parallel()
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	testdb.SeedRoutingSet(t, db, seeded.ProcessID, "test")
	svc := NewStationService(db)
	svc.SetBinTypeResolver(testdb.BinTypeOf(fx))
	svc.SetCoreNodeGroupResolver(func() map[string][]string { return testdb.GroupsOf(fx) })
	geom := testdb.SceneGeometryOf(fx)
	svc.SetSceneGeometryResolver(func() *domain.SceneGeometry { return geom })

	gaps := func() map[string]string {
		data, err := svc.ComposerForProcess(seeded.ProcessID)
		testutil.MustNoErr(t, err, "ComposerForProcess")
		return data.RoutingGaps
	}
	if g := gaps(); len(g) != 0 {
		t.Fatalf("a set with a source and a destination names gaps: %v", g)
	}
	rows, err := db.ListRoutingNodes(seeded.ProcessID)
	testutil.MustNoErr(t, err, "ListRoutingNodes")
	drop := func(role string) {
		t.Helper()
		n := 0
		for _, r := range rows {
			if r.Role == role {
				testutil.MustNoErr(t, db.SetRoutingNodeEnabled(domain.CoreNodeKinds{}, seeded.ProcessID, r.ID, false, "test"), "disable")
				n++
			}
		}
		if n == 0 {
			t.Fatalf("setup: the routing set has no %s row to take away", role)
		}
	}

	drop(domain.RoutingRoleDestination)
	dest := "Press A1's routing set names no outbound destination — add one under Routing set"
	if g := gaps(); len(g) != 1 || g["outbound_destination"] != dest {
		t.Errorf("no destination: gaps %v, want outbound_destination %q", g, dest)
	}

	drop(domain.RoutingRoleSource)
	source := "Press A1's routing set names no inbound source — add one under Routing set"
	if g := gaps(); len(g) != 2 || g["outbound_destination"] != dest || g["inbound_source"] != source {
		t.Errorf("no source and no destination: gaps %v, want both sentences", g)
	}
}
