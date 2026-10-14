package store

import (
	"testing"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// deleted_process_claims_test.go — a deleted process's claims stop answering.
//
// Deleting a process soft-deleted its styles and left their claims live, so a
// claim of a process that no longer exists was still a candidate for "where
// does the carrier on this node belong" (ClaimForLinesidePayload, read by the
// evacuation aim). SPR kept one at a staging spot. The delete now retires the
// claims, and the lookup ignores claims of deleted styles, which covers rows a
// delete left behind before this.

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

	if got, err := db.ClaimForLinesidePayload("SLN_X", "PART-X"); err != nil || got == nil {
		t.Fatalf("precondition: the live claim answers (got %v, err %v)", got, err)
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
	if got, err := db.ClaimForLinesidePayload("SLN_X", "PART-X"); err != nil || got != nil {
		t.Errorf("a deleted process's claim still answers where the carrier belongs: %+v (err %v)", got, err)
	}
}

func TestClaimForLinesidePayload_IgnoresAClaimOfADeletedStyle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, sid, _ := seedStagingClaim(t, db, "P-Orphan", "SLN_Y", "PART-Y")
	// A row a delete left behind before claims were retired with their style.
	if _, err := db.Exec(`UPDATE styles SET deleted_at = datetime('now') WHERE id=?`, sid); err != nil {
		t.Fatalf("soft-delete style: %v", err)
	}
	if got, err := db.ClaimForLinesidePayload("SLN_Y", "PART-Y"); err != nil || got != nil {
		t.Errorf("a claim of a deleted style answers where the carrier belongs: %+v (err %v)", got, err)
	}
}
