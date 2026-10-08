package testdb

import (
	"testing"

	"shingocore/store"
	"shingocore/store/nodes"
)

// MarkGroup gives node group groupID a waiting spot named after it, unless it
// already has one. A lane cannot be created in a group without wait points
// (nodes.ErrLaneUnmarked), so a fixture marks a group before its first lane.
func MarkGroup(t testing.TB, db *store.DB, groupID int64) {
	t.Helper()
	if len(nodes.ParseWaitPoints(db.GetNodeProperty(groupID, nodes.PropGroupWaitPoints))) > 0 {
		return
	}
	g, err := db.GetNode(groupID)
	if err != nil || g == nil {
		t.Fatalf("mark group %d: %v", groupID, err)
	}
	if err := db.SetNodeProperty(groupID, nodes.PropGroupWaitPoints, g.Name+"-WAIT"); err != nil {
		t.Fatalf("mark group %s: %v", g.Name, err)
	}
}

// MarkedGroup creates a node group named name with a waiting spot and returns
// a pointer to its id, for a fixture's lane to take as ParentID: a lane is never
// without a group, and never in a group without wait points.
func MarkedGroup(t testing.TB, db *store.DB, name string) *int64 {
	t.Helper()
	ngrp, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("NGRP type: %v", err)
	}
	g := &nodes.Node{Name: name, IsSynthetic: true, Enabled: true, NodeTypeID: &ngrp.ID}
	if err := db.CreateNode(g); err != nil {
		t.Fatalf("create group %s: %v", name, err)
	}
	MarkGroup(t, db, g.ID)
	return &g.ID
}
