package www

import (
	"net/http"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// The Copy Node Claims endpoint: one style's claims onto a selected set of
// sibling styles. These pin the HTTP contract — per-target results in the
// response, the batch never aborts, the active style is refused with a
// readable reason, and the housekeeping hooks fire once for the batch (the
// copied count, not the request count, gates them).

func TestApiCopyStyleClaims_HappyPath(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	pid, srcID := seedStyleProcess(t, h, "CopyApiProc", "SRC")
	tgtA, err := testDB.CreateStyle("TGT-A", "", pid)
	testutil.MustNoErr(t, err, "create TGT-A")
	tgtB, err := testDB.CreateStyle("TGT-B", "", pid)
	testutil.MustNoErr(t, err, "create TGT-B")

	_, err = processes.UpsertClaim(testDB.DB, processes.NodeClaimInput{
		StyleID: srcID, CoreNodeName: "N-1", Role: "produce",
		SwapMode: "sequential", PayloadCode: "P-SRC", UOPCapacity: 10,
	})
	testutil.MustNoErr(t, err, "seed source claim")

	resp := doRequest(t, router, "POST", "/api/styles/"+itoa(srcID)+"/claims/copy-to", map[string]any{
		"target_style_ids": []int64{tgtA, tgtB},
		"include_payloads": true,
	}, cookie)
	assertStatus(t, resp, http.StatusOK)

	var got struct {
		Copied  int `json:"copied"`
		Results []struct {
			StyleID int64  `json:"style_id"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
		} `json:"results"`
	}
	decodeJSON(t, resp, &got)
	if got.Copied != 2 {
		t.Fatalf("copied = %d, want 2; %+v", got.Copied, got)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results = %d rows, want 2", len(got.Results))
	}

	// Both targets now carry the source's claim.
	for _, tgt := range []int64{tgtA, tgtB} {
		claims, err := testDB.ListStyleNodeClaims(tgt)
		testutil.MustNoErr(t, err, "list claims for target")
		if len(claims) != 1 || claims[0].CoreNodeName != "N-1" || claims[0].PayloadCode != "P-SRC" {
			t.Errorf("target %d claims = %+v, want N-1/P-SRC", tgt, claims)
		}
	}
}

func TestApiCopyStyleClaims_ActiveStyleRefused(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	pid, srcID := seedStyleProcess(t, h, "CopyApiActiveProc", "SRC")
	activeID, err := testDB.CreateStyle("RUNNING", "", pid)
	testutil.MustNoErr(t, err, "create RUNNING")
	aID := activeID
	testutil.MustNoErr(t, testDB.SetActiveStyle(pid, &aID), "set active")

	resp := doRequest(t, router, "POST", "/api/styles/"+itoa(srcID)+"/claims/copy-to", map[string]any{
		"target_style_ids": []int64{activeID},
		"include_payloads": true,
	}, cookie)
	assertStatus(t, resp, http.StatusOK) // the BATCH answers; the target is the refusal

	var got struct {
		Copied  int `json:"copied"`
		Results []struct {
			StyleID int64  `json:"style_id"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
		} `json:"results"`
	}
	decodeJSON(t, resp, &got)
	if got.Copied != 0 {
		t.Errorf("copied = %d, want 0 — the active style must never receive claims", got.Copied)
	}
	if len(got.Results) != 1 || got.Results[0].Status != "failed" || got.Results[0].Reason == "" {
		t.Errorf("results = %+v, want one failed row naming the rule", got.Results)
	}
}

func TestApiCopyStyleClaims_EmptyTargets(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	_, srcID := seedStyleProcess(t, h, "CopyApiEmptyProc", "SRC")
	resp := doRequest(t, router, "POST", "/api/styles/"+itoa(srcID)+"/claims/copy-to", map[string]any{
		"target_style_ids": []int64{},
	}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
}

// seedStyleProcess is the local seed: a process and one style, returning
// both ids (www's seedProcess does not create styles).
func seedStyleProcess(t *testing.T, h *Handlers, procName, styleName string) (int64, int64) {
	t.Helper()
	pid, err := testDB.CreateProcess(procName, "", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	sid, err := testDB.CreateStyle(styleName, "", pid)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	return pid, sid
}
