//go:build docker

package nodes_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// depthOf reads a node's depth, nil when unset.
func depthOf(t *testing.T, db *store.DB, id int64) *int {
	t.Helper()
	n, err := nodes.Get(db.DB, id)
	testutil.MustNoErr(t, err, "read node")
	return n.Depth
}

func wantDepth(t *testing.T, step string, got *int, want int) {
	t.Helper()
	if want == 0 {
		if got != nil {
			t.Errorf("%s: depth = %d, want none", step, *got)
		}
		return
	}
	if got == nil || *got != want {
		t.Errorf("%s: depth = %v, want %d", step, got, want)
	}
}

// TestNodeLeavingALaneIsAFlatPosition: a depth is a slot's place in a LANE. A
// slot moved out of its lane into the group's direct section stands at depth 1,
// as a flat position (the Nodes page sends no position there, and the old
// depth used to stay: SPR's AMR Supermarket kept 1-9 for five months). Moved
// back into a lane at a position it takes that position; loose it has none.
func TestNodeLeavingALaneIsAFlatPosition(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB
	grp := testdb.MarkedGroup(t, db, "DEPTH-GRP")
	lane, err := nodes.AddLane(sdb, *grp, "DEPTH-LANE")
	testutil.MustNoErr(t, err, "add lane")
	slot := &nodes.Node{Name: "DEPTH-SLOT", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, slot), "create slot")

	testutil.MustNoErr(t, nodes.Reparent(sdb, slot.ID, &lane, 3), "into the lane at 3")
	wantDepth(t, "in the lane", depthOf(t, db, slot.ID), 3)

	testutil.MustNoErr(t, nodes.Reparent(sdb, slot.ID, grp, 0), "to the group's direct section")
	wantDepth(t, "out to the group", depthOf(t, db, slot.ID), 1)

	testutil.MustNoErr(t, nodes.Reparent(sdb, slot.ID, &lane, 2), "back into the lane at 2")
	testutil.MustNoErr(t, nodes.Reparent(sdb, slot.ID, grp, 5), "to the group at a position")
	wantDepth(t, "to the group at a position", depthOf(t, db, slot.ID), 1)

	testutil.MustNoErr(t, nodes.Reparent(sdb, slot.ID, &lane, 2), "back into the lane at 2")
	testutil.MustNoErr(t, nodes.ClearParent(sdb, slot.ID), "loose")
	wantDepth(t, "loose", depthOf(t, db, slot.ID), 0)
}

// TestNodeFormOutOfALaneIsAFlatPosition: the node form posts every column,
// depth included, so a slot whose parent the form changes from its lane to the
// group keeps the posted depth unless the write flattens it.
func TestNodeFormOutOfALaneIsAFlatPosition(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB
	grp := testdb.MarkedGroup(t, db, "FORM-GRP")
	lane, err := nodes.AddLane(sdb, *grp, "FORM-LANE")
	testutil.MustNoErr(t, err, "add lane")
	four := 4
	slot := &nodes.Node{Name: "FORM-SLOT", Enabled: true, ParentID: &lane, Depth: &four}
	testutil.MustNoErr(t, nodes.Create(sdb, slot), "create slot in the lane")
	wantDepth(t, "created in the lane", depthOf(t, db, slot.ID), 4)

	slot.ParentID = grp
	testutil.MustNoErr(t, nodes.Update(sdb, slot), "form: parent to the group")
	wantDepth(t, "form moved it to the group", depthOf(t, db, slot.ID), 1)
}

// TestDeleteGroupLeavesNoDepth: a group deleted with its lane unparents the
// lane's slots, and a loose node has no depth (the delete cleared the old
// depth property and left the column).
func TestDeleteGroupLeavesNoDepth(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB
	grp := testdb.MarkedGroup(t, db, "DEL-DEPTH-GRP")
	lane, err := nodes.AddLane(sdb, *grp, "DEL-DEPTH-LANE")
	testutil.MustNoErr(t, err, "add lane")
	two := 2
	slot := &nodes.Node{Name: "DEL-DEPTH-SLOT", Enabled: true, ParentID: &lane, Depth: &two}
	testutil.MustNoErr(t, nodes.Create(sdb, slot), "create slot in the lane")

	testutil.MustNoErr(t, nodes.DeleteGroup(sdb, *grp), "delete the group")
	wantDepth(t, "after the group's delete", depthOf(t, db, slot.ID), 0)
}
