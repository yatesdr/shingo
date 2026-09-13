package store

import (
	"testing"

	"shingo/protocol/testutil"

	"shingoedge/domain"
	"shingoedge/store/catalog"
	"shingoedge/store/processes"
)

// live_reads_skip_retired_test.go — the two PROCESS-WIDE reads must not see a
// retired claim.
//
// ListLiveClaimsByProcess feeds Core's plant-claims mirror and
// ListStylePartIdentitiesByProcess feeds the CATID derivation. Both were
// written on a tree that had no retired_at, on the same day retirement landed
// on a sibling branch, so neither carried the liveClaims filter every other
// list read applies. A retired claim is one changeover history still points
// at; to everything that decides live behaviour it must read as absent,
// exactly as a deleted one did — otherwise a part the engineer removed keeps
// reaching Core's demand registry and keeps matching the PLC's CATID.

func TestRetiredClaim_IsInvisibleToTheOneQueryReads(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, sid := seedClaimProcess(t, db, "P-RetiredRead")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: "PLN_01", Name: "PLN_01", Enabled: true})
	if err != nil {
		t.Fatalf("CreateProcessNode: %v", err)
	}
	mk := func(node, payload string) int64 {
		id, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
			StyleID: sid, CoreNodeName: node, Role: "produce", SwapMode: "two_robot",
			PayloadCode: payload, InboundStaging: "STG", InboundSource: "SMN", OutboundDestination: "SMN_DST",
			Source: domain.ClaimSourceAdmin, CalledBy: "alice",
		})
		if err != nil {
			t.Fatalf("upsert %s: %v", node, err)
		}
		return id
	}
	retired := mk("PLN_01", "PART-OLD")
	mk("PLN_02", "PART-NEW")
	for _, e := range []struct {
		id    int64
		code  string
		catid string
	}{{1, "PART-OLD", "40010001"}, {2, "PART-NEW", "40010002"}} {
		if err := db.UpsertPayloadCatalog(&catalog.CatalogEntry{ID: e.id, Name: e.code, Code: e.code, CATID: e.catid}); err != nil {
			t.Fatalf("catalog %s: %v", e.code, err)
		}
	}

	// History points at PLN_01's claim, so DeleteClaim retires it instead of
	// deleting — the real path, not a raw UPDATE.
	res, err := db.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state) VALUES (?, ?, 'completed')`, pid, sid)
	if err != nil {
		t.Fatalf("insert changeover: %v", err)
	}
	coID, err := res.LastInsertId()
	testutil.MustNoErr(t, err, "res.LastInsertId")
	if _, err := db.Exec(`INSERT INTO changeover_node_tasks (process_changeover_id, process_node_id, from_claim_id, situation, state)
		VALUES (?, ?, ?, 'changed', 'completed')`, coID, nodeID, retired); err != nil {
		t.Fatalf("insert node task: %v", err)
	}
	if err := db.DeleteStyleNodeClaim(retired); err != nil {
		t.Fatalf("delete referenced: %v", err)
	}
	if c, err := db.GetStyleNodeClaim(retired); err != nil || c.RetiredAt == nil {
		t.Fatalf("precondition: claim %d should be retired and still resolvable by id (err %v)", retired, err)
	}

	claims, err := processes.ListLiveClaimsByProcess(db.DB, pid)
	if err != nil {
		t.Fatalf("ListLiveClaimsByProcess: %v", err)
	}
	if len(claims) != 1 || claims[0].CoreNodeName != "PLN_02" {
		names := make([]string, 0, len(claims))
		for _, c := range claims {
			names = append(names, c.CoreNodeName)
		}
		t.Errorf("ListLiveClaimsByProcess = %v, want [PLN_02] — a retired claim must not reach Core's mirror", names)
	}

	ids, err := processes.ListStylePartIdentitiesByProcess(db.DB, pid)
	if err != nil {
		t.Fatalf("ListStylePartIdentitiesByProcess: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("part identities = %d styles, want 1", len(ids))
	}
	if got := ids[0].CatalogCATIDs; len(got) != 1 || got[0] != "40010002" {
		t.Errorf("CatalogCATIDs = %v, want [40010002] — a retired produce claim must contribute no CATID", got)
	}
}
