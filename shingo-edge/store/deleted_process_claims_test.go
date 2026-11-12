package store

import (
	"testing"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// deleted_process_claims_test.go — a deleted process's claims stop answering.
//
// Deleting a process soft-deleted its styles and left their claims live, so a
// claim of a process that no longer exists was still read as live, and published
// to Core with the plant's claims. The delete now retires the claims.

func seedStagingClaim(t *testing.T, db *DB, name, node, payload string) (pid, sid, cid int64) {
	t.Helper()
	pid, sid = seedClaimProcess(t, db, name)
	cid, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: node, Role: "consume", SwapMode: "two_robot",
		PayloadCode: payload, InboundStaging: "STG", InboundSource: "SMN", OutboundDestination: "SMN_DST",
		Source: domain.ClaimSourceAdmin, CalledBy: "alice",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	return pid, sid, cid
}

func TestDeleteProcess_RetiresItsClaims(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, _, cid := seedStagingClaim(t, db, "P-Gone", "SLN_X", "PART-X")

	if live, err := processes.ListLiveClaimsByProcess(db.DB, pid); err != nil || len(live) != 1 {
		t.Fatalf("precondition: the live claim answers (got %d, err %v)", len(live), err)
	}
	if err := db.DeleteProcess(pid); err != nil {
		t.Fatalf("DeleteProcess: %v", err)
	}

	var retired *string
	if err := db.QueryRow(`SELECT retired_at FROM style_node_claims WHERE id=?`, cid).Scan(&retired); err != nil {
		t.Fatalf("read claim %d: %v (it must be retired, not deleted — history still names it)", cid, err)
	}
	if retired == nil {
		t.Errorf("claim %d of a deleted process is still live", cid)
	}
	if live, err := processes.ListLiveClaimsByProcess(db.DB, pid); err != nil || len(live) != 0 {
		t.Errorf("a deleted process's claims still read as live: %d (err %v)", len(live), err)
	}
}
