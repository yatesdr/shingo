//go:build docker

package nodes_test

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// R19-1: a mid-transaction statement failure must surface a specific, actionable
// error and leave the group intact. Postgres already aborts the tx on the first
// error (so a partial delete can't commit), but the fix names the failing
// statement instead of returning a generic "commit unexpectedly resulted in
// rollback".
func TestDeleteGroup_MidTxError_RollsBackWithSpecificError(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	grpID, err := nodes.CreateGroup(sdb, "GRP-ROLLBACK")
	testutil.MustNoErr(t, err, "CreateGroup")
	testutil.MustNoErr(t, nodes.SetProperty(sdb, grpID, "keep", "yes"), "SetProperty")

	// Induce a mid-transaction failure on one of DeleteGroup's statements.
	_, err = sdb.Exec(`DROP TABLE node_payloads`)
	testutil.MustNoErr(t, err, "drop node_payloads")

	err = nodes.DeleteGroup(sdb, grpID)
	if err == nil {
		t.Fatal("DeleteGroup: expected an error on mid-transaction failure, got nil")
	}
	if !strings.Contains(err.Error(), "payload") {
		t.Errorf("DeleteGroup error = %q, want it to name the failing statement (payloads)", err)
	}

	// No partial commit: the group node and its property survive the rollback.
	var nodeCnt, propCnt int
	testutil.MustNoErr(t, sdb.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id=$1`, grpID).Scan(&nodeCnt), "count node")
	testutil.MustNoErr(t, sdb.QueryRow(`SELECT COUNT(*) FROM node_properties WHERE node_id=$1`, grpID).Scan(&propCnt), "count prop")
	if nodeCnt != 1 || propCnt != 1 {
		t.Errorf("after rollback: node count=%d prop count=%d, want 1/1 (no partial delete)", nodeCnt, propCnt)
	}
}

// R19-2 (the real correctness bug): when ListChildren errors, the descendant set
// would silently come back empty, so DeleteGroup deleted only the group root and
// committed — orphaning the physical children and synthetic descendants. That
// partial outcome is NOT caught by the transaction (no statement errors), so the
// fix must surface the ListChildren error and delete nothing.
func TestDeleteGroup_ListChildrenError_NoPartialDelete(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	grpID, err := nodes.CreateGroup(sdb, "GRP-LCERR")
	testutil.MustNoErr(t, err, "CreateGroup")
	laneID, err := nodes.AddLane(sdb, grpID, "LAN-LCERR")
	testutil.MustNoErr(t, err, "AddLane")
	physical := &nodes.Node{Name: "PHYS-LCERR", Enabled: true, ParentID: &laneID}
	testutil.MustNoErr(t, nodes.Create(sdb, physical), "Create physical child")

	// Break ListChildren, which queries nodes.parent_id.
	_, err = sdb.Exec(`ALTER TABLE nodes RENAME COLUMN parent_id TO parent_id_x`)
	testutil.MustNoErr(t, err, "rename parent_id")

	err = nodes.DeleteGroup(sdb, grpID)
	if err == nil {
		t.Fatal("DeleteGroup: expected the ListChildren error to be surfaced, got nil (would have partial-deleted)")
	}
	if !strings.Contains(err.Error(), "list children") {
		t.Errorf("DeleteGroup error = %q, want it to mention list children", err)
	}

	// Nothing was deleted — the group root still exists (no partial delete).
	// The COUNT keys on id, so it is unaffected by the renamed column.
	var nodeCnt int
	testutil.MustNoErr(t, sdb.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id=$1`, grpID).Scan(&nodeCnt), "count group")
	if nodeCnt != 1 {
		t.Errorf("group node count = %d, want 1 (DeleteGroup must not partial-delete on a ListChildren error)", nodeCnt)
	}
}
