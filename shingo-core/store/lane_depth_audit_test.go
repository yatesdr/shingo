//go:build docker

package store_test

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// realLaneWithDepthlessChild builds a lane of the REAL LANE class (the audit
// keys on node_types.code, so the prefix-scoped types laneFixture invents would
// not match) holding one depth-1 slot and one child with no depth at all.
// occupyDepthless decides whether a bin sits in the depth-less one.
func realLaneWithDepthlessChild(t *testing.T, db *store.DB, prefix string, occupyDepthless bool) (lane, slot, depthless *nodes.Node) {
	t.Helper()
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	if err != nil {
		t.Fatalf("get LANE node type: %v", err)
	}

	lane = &nodes.Node{Name: prefix + "-LANE", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	d1 := 1
	slot = &nodes.Node{Name: prefix + "-SLOT-01", Enabled: true, ParentID: &lane.ID, Depth: &d1}
	if err := db.CreateNode(slot); err != nil {
		t.Fatalf("create depth-1 slot: %v", err)
	}
	depthless = &nodes.Node{Name: prefix + "-NODEPTH", Enabled: true, ParentID: &lane.ID}
	if err := db.CreateNode(depthless); err != nil {
		t.Fatalf("create depth-less child: %v", err)
	}
	if occupyDepthless {
		testdb.CreateBinAtNode(t, db, "PART-A", depthless.ID, prefix+"-BIN")
	}
	return
}

// TestAuditLaneDepths_FlagsOccupiedDepthlessChild is the loud half of the D1
// ruling.
//
// Reachability IGNORES a depth-less sibling — it is not a depth-ordered slot, so
// it cannot be in front of anything. That is the majority reading and it is now
// the only one. It is also unfalsifiable from inside the code: if the geometry
// never occurs the ruling costs nothing, and if it does occur a bin is sitting
// in a lane that NOTHING in the system will treat as in the way.
//
// So the ruling ships with an alarm rather than a guard. The two spellings used
// to disagree about this case silently; a boot warning is what turns the silence
// into a question someone can answer.
func TestAuditLaneDepths_FlagsOccupiedDepthlessChild(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	_, _, depthless := realLaneWithDepthlessChild(t, db, "AUDIT-BAD", true)

	warnings, err := db.AuditLaneDepths()
	if err != nil {
		t.Fatalf("AuditLaneDepths: %v", err)
	}
	var saw bool
	for _, w := range warnings {
		if strings.Contains(w, depthless.Name) {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("%s holds a bin and has no depth, but the audit said nothing.\n"+
			"Reachability will never treat that bin as being in the way; the point of the audit is that this "+
			"is loud at boot rather than a silent disagreement between two readers.\ngot: %v",
			depthless.Name, warnings)
	}
}

// TestAuditLaneDepths_QuietWhenNothingIsWrong: a depth-less lane child that
// holds NOTHING is not a problem — reachability ignoring an empty node costs
// nobody anything — and neither is a clean depth-ordered slot. An audit that
// fires on healthy scenes is an audit operators learn to ignore.
func TestAuditLaneDepths_QuietWhenNothingIsWrong(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	_, slot, depthless := realLaneWithDepthlessChild(t, db, "AUDIT-OK", false)
	// The BIN goes in the properly-tiered slot, where it is visible to everyone.
	testdb.CreateBinAtNode(t, db, "PART-A", slot.ID, "AUDIT-OK-BIN")

	warnings, err := db.AuditLaneDepths()
	if err != nil {
		t.Fatalf("AuditLaneDepths: %v", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, depthless.Name) || strings.Contains(w, slot.Name) {
			t.Fatalf("healthy lane flagged by the depth audit: %q", w)
		}
	}
}
