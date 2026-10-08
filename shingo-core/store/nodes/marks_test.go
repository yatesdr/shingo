//go:build docker

package nodes_test

import (
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// A lane is never without a mark: every way to write one refuses a group with no
// wait points, a group with lanes cannot lose its last wait point, and a
// database written before the rule is named by UnmarkedLanes (boot refuses it).
func TestLane_NeverWithoutAMark(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB
	laneType, err := nodes.GetTypeByCode(sdb, "LANE")
	testutil.MustNoErr(t, err, "LANE type")
	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, nodes.ErrLaneUnmarked) {
			t.Errorf("%s: err = %v, want ErrLaneUnmarked", what, err)
		}
	}

	bare, err := nodes.CreateGroup(sdb, "MARK-BARE")
	testutil.MustNoErr(t, err, "bare group")
	marked, err := nodes.CreateGroup(sdb, "MARK-OK")
	testutil.MustNoErr(t, err, "marked group")
	testdb.MarkGroup(t, db, marked)

	refused("a lane with no group", nodes.Create(sdb, &nodes.Node{Name: "MARK-L0", Enabled: true, IsSynthetic: true,
		NodeTypeID: &laneType.ID}))
	refused("a lane in an unmarked group", nodes.Create(sdb, &nodes.Node{Name: "MARK-L1", Enabled: true,
		IsSynthetic: true, NodeTypeID: &laneType.ID, ParentID: &bare}))
	_, err = nodes.AddLane(sdb, bare, "MARK-L2")
	refused("AddLane to an unmarked group", err)

	lane, err := nodes.AddLane(sdb, marked, "MARK-L3")
	testutil.MustNoErr(t, err, "a lane in a marked group is created")
	refused("moving a lane into an unmarked group", nodes.SetParent(sdb, lane, bare))
	refused("clearing a lane's group", nodes.ClearParent(sdb, lane))
	refused("blanking the wait points of a group with lanes", nodes.SetProperty(sdb, marked, nodes.PropGroupWaitPoints, " , "))
	refused("deleting the wait points of a group with lanes", nodes.DeleteProperty(sdb, marked, nodes.PropGroupWaitPoints))
	testutil.MustNoErr(t, nodes.DeleteProperty(sdb, bare, nodes.PropGroupWaitPoints),
		"a group with no lanes may have no wait points")

	// A lane written before the rule (no store door): boot names it.
	_, err = sdb.Exec(`INSERT INTO nodes (name, is_synthetic, enabled, node_type_id, parent_id) VALUES ($1, true, true, $2, $3)`,
		"MARK-LEGACY", laneType.ID, bare)
	testutil.MustNoErr(t, err, "a legacy unmarked lane")
	got, err := nodes.UnmarkedLanes(sdb)
	testutil.MustNoErr(t, err, "UnmarkedLanes")
	if len(got) != 1 || got[0] != "MARK-LEGACY (group MARK-BARE)" {
		t.Errorf("UnmarkedLanes = %v, want only the legacy lane", got)
	}
}
