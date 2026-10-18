package www

import (
	"encoding/json"
	"net/http"
	"testing"

	"shingo/protocol/testutil"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// decodeBody reads a response body into a generic map so a test can assert on
// keys that are ADDITIVE — present sometimes, absent otherwise.
func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

// ── Attribution ──────────────────────────────────────────────────────────
//
// Every claim row says who wrote it and from where, stamped by the server.
// The admin editor's write path is here: 'admin' plus the session user, and a
// client that tries to say otherwise is refused rather than ignored — a
// silently-dropped field is a field somebody will believe they set.

// TestChangeoverView_ResolvesRetiredFromClaim: deleting a claim that a
// changeover's node task came FROM retires it, and the changeover view still
// renders the from-part label from the retired row — never blank.
func TestChangeoverView_ResolvesRetiredFromClaim(t *testing.T) {
	h, _ := newAdminRouter(t)
	pid := seedProcess(t, "RetireLabelLine")
	sid := seedStyle(t, "RetireLabelStyle", pid)
	nodeID := seedProcessNode(t, pid, 0, "RETIRE-NODE")
	claimID, err := testDB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "RETIRE-NODE", Role: "consume", SwapMode: "two_robot",
		PayloadCode: "PART-OLD", InboundStaging: "RETIRE-STG", OutboundDestination: "RETIRE-DST",
	})
	if err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	res, err := testDB.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state) VALUES (?, ?, 'active')`, pid, sid)
	if err != nil {
		t.Fatalf("insert changeover: %v", err)
	}
	coID, err := res.LastInsertId()
	testutil.MustNoErr(t, err, "res.LastInsertId")
	if _, err := testDB.Exec(`INSERT INTO changeover_node_tasks (process_changeover_id, process_node_id, from_claim_id, situation, state)
		VALUES (?, ?, ?, 'changed', 'pending')`, coID, nodeID, claimID); err != nil {
		t.Fatalf("insert node task: %v", err)
	}

	// The claim is retired through the store's delete — the one a flow save
	// that drops the position calls (the single-claim DELETE endpoint went in
	// pick-a-part U6).
	testutil.MustNoErr(t, testDB.DeleteStyleNodeClaim(claimID), "delete claim")
	claims, err := testDB.ListStyleNodeClaims(sid)
	testutil.MustNoErr(t, err, "testDB.ListStyleNodeClaims")
	if len(claims) != 0 {
		t.Fatalf("retired claim still listed: %+v", claims)
	}

	process, err := testDB.GetProcess(pid)
	if err != nil {
		t.Fatalf("get process: %v", err)
	}
	d := h.buildChangeoverViewData(process)
	if d.ActiveChangeover == nil {
		t.Fatal("the seeded changeover is not active in the view")
	}
	var seen bool
	for _, v := range d.CentralNodeTasks {
		if v.ProcessNodeID == nodeID {
			seen = true
			if v.FromPayload != "PART-OLD" {
				t.Fatalf("from-part label = %q after the claim was retired, want PART-OLD — the history label rendered blank", v.FromPayload)
			}
		}
	}
	if !seen {
		t.Fatalf("node task for %d not in the view: %+v", nodeID, d.CentralNodeTasks)
	}
}
