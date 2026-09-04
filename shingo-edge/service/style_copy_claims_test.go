package service

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
)

// CopyClaims is the service-level gate over the mechanical store replace:
// same-process targets only, the process's ACTIVE style refused outright
// (copying claims under a style production is running would change live
// behavior mid-part), the source skipped, duplicates collapsed â€” and the
// batch never aborts on the first bad target.
func TestStyleCopyClaims_RulesAndResults(t *testing.T) {
	db := testdb.Open(t)
	svc := NewStyleService(db)

	pid, srcID := seedProcessStyle(t, db, "CopyRulesProc", "SRC")
	tgtID, err := db.CreateStyle("TGT", "", pid)
	testutil.MustNoErr(t, err, "create TGT")
	activeID, err := db.CreateStyle("RUNNING", "", pid)
	testutil.MustNoErr(t, err, "create RUNNING")
	_, otherProcStyle := seedProcessStyle(t, db, "CopyRulesOther", "OTHER-STYLE")

	// Make RUNNING the production-active style.
	var aID = activeID
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &aID), "set active")

	// Give the source one claim so a successful copy has something to move.
	_, err = processes.UpsertClaim(db.DB, processes.NodeClaimInput{
		StyleID: srcID, CoreNodeName: "N-1", Role: "produce",
		SwapMode: protocol.SwapModeSequential, PayloadCode: "P-SRC", UOPCapacity: 10,
	})
	testutil.MustNoErr(t, err, "seed source claim")

	results := svc.CopyClaims(srcID, []int64{
		tgtID,          // legal target
		activeID,       // refused: active style
		srcID,          // skipped: is the source
		otherProcStyle, // failed: different process
		999999,         // failed: missing
		tgtID,          // duplicate target â€” collapsed
	}, true)

	mustCopyStatus(t, results, tgtID, "copied", "")
	mustCopyStatus(t, results, activeID, "failed", "active style")
	mustCopyStatus(t, results, srcID, "skipped", "source")
	mustCopyStatus(t, results, otherProcStyle, "failed", "different process")
	mustCopyStatus(t, results, 999999, "failed", "not found")

	// Six requested, five reported â€” the duplicate collapsed.
	if len(results) != 5 {
		t.Errorf("results = %d rows, want 5 (duplicate target collapsed): %+v", len(results), results)
	}

	// The legal target actually received the claims.
	claims, err := db.ListStyleNodeClaims(tgtID)
	testutil.MustNoErr(t, err, "list TGT claims")
	if len(claims) != 1 || claims[0].CoreNodeName != "N-1" || claims[0].PayloadCode != "P-SRC" {
		t.Errorf("TGT claims = %+v, want N-1/P-SRC copied from SRC", claims)
	}
}

func mustCopyStatus(t *testing.T, results []CopyClaimsResult, styleID int64, wantStatus, wantReasonContains string) {
	t.Helper()
	for _, r := range results {
		if r.StyleID == styleID {
			if r.Status != wantStatus {
				t.Errorf("style %d: status = %q (%s), want %q", styleID, r.Status, r.Reason, wantStatus)
			}
			if wantReasonContains != "" && !strings.Contains(r.Reason, wantReasonContains) {
				t.Errorf("style %d: reason = %q, want it to contain %q", styleID, r.Reason, wantReasonContains)
			}
			return
		}
	}
	t.Errorf("style %d missing from results: %+v", styleID, results)
}
