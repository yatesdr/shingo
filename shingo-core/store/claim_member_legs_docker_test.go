//go:build docker

package store_test

import (
	"reflect"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// TestClaimPickLegsNamingMembers pins Core's start report of saved setups: a
// claim leg where Core picks that names a member of a node group (a lane slot,
// or a group's direct child) is listed with its group, a member that is a
// loader home says so, and a leg naming the group itself, a node in no group,
// or no node is not listed.
func TestClaimPickLegsNamingMembers(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	grp := *testdb.MarkedGroup(t, db, "ML-AREA")
	lane, err := db.AddLane(grp, "ML-AREA-L1")
	testutil.MustNoErr(t, err, "lane")
	depth := 1
	slot := &nodes.Node{Name: "ML-AREA-L1-S1", Enabled: true, ParentID: &lane, Depth: &depth}
	testutil.MustNoErr(t, db.CreateNode(slot), "lane slot")
	child := &nodes.Node{Name: "ML-AREA-W1", Enabled: true, ParentID: &grp}
	testutil.MustNoErr(t, db.CreateNode(child), "group child")
	loose := &nodes.Node{Name: "ML-LOOSE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(loose), "node in no group")
	lid, err := db.CreateLoader(loaders.Loader{Name: "ML-UNL", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator})
	testutil.MustNoErr(t, err, "loader")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: lid, PositionNodeID: child.ID}), "its home")

	_, err = db.DB.Exec(`INSERT INTO style_claims (process_id, style_id, core_node_name, role, swap_mode,
		inbound_source, outbound_destination, containment_destination, partial_destination) VALUES
		('P', 'S', 'ML-CELL', 'produce', 'auto', 'ML-AREA', 'ML-AREA-L1-S1', 'ML-AREA-W1', 'ML-LOOSE'),
		('P', 'T', 'ML-CELL', 'produce', 'auto', 'NO-SUCH-NODE', '', '', '')`)
	testutil.MustNoErr(t, err, "claims")

	got, err := db.ClaimPickLegsNamingMembers()
	testutil.MustNoErr(t, err, "report")
	want := []store.MemberLeg{
		{Process: "P", Style: "S", Node: "ML-CELL", Leg: "containment_destination", Name: "ML-AREA-W1",
			Group: "ML-AREA", LoaderHome: true},
		{Process: "P", Style: "S", Node: "ML-CELL", Leg: "outbound_destination", Name: "ML-AREA-L1-S1",
			Group: "ML-AREA"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the report lists %+v\nwant              %+v", got, want)
	}
}
