package engine

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// TestLoaderOnlyOperations_AdmitASynthesizedClaim is census 16: the loader
// operations on the population the claim quarantine leaves behind — a Core-owned
// loader window with NO stored claim, whose board runs on the claim
// domain.Loader.SynthClaim builds.
//
// TestLoaderOnlyOperations_AdmitLoaderNodes admits a STORED manual_swap row,
// which is exactly the shape the first node-list sync after deploy moves into
// style_node_claims_quarantine. After that sync every loader window is this
// test's shape, and nothing asserted its operations are still admitted.
//
// It asserts the loader gate does not fire (the engine points at an unreachable
// Core, so each call then fails for a visibly different reason), and that the
// synthesized claim's ID — zero, by construction — is never written to the
// runtime's active_claim_id, which is an FK into style_node_claims.
//
// COVERAGE PIN. Expected to pass at bcbde0d2. MUTATION: resolve the claim with
// the package-level loadActiveNode (no synth fallback) in requireLoaderClaim's
// callers — every operation then refuses "has no active claim".
func TestLoaderOnlyOperations_AdmitASynthesizedClaim(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	unreachableCore(t, eng)

	processID, err := db.CreateProcess("SYN16-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "SYN16-W1", Code: "S16", Name: "loader window", Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	seedCoreLoader(t, eng, sharedLoaderInfo("SYN16-W1", "produce", "operator", "PART-X", 0, 0))

	for _, op := range loaderOnlyOps(eng) {
		err := op.call(nodeID)
		if err != nil && (strings.Contains(err.Error(), notALoaderMessage) ||
			strings.Contains(err.Error(), "has no active claim")) {
			t.Errorf("%s on a Core-owned loader window with no stored claim was refused by the loader "+
				"gate (%v). After the quarantine this is EVERY loader window, so this is a dead board", op.name, err)
		}
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.ActiveClaimID != nil && *rt.ActiveClaimID == 0 {
		t.Errorf("active_claim_id was written as 0 — the synthesized claim's ID, which names no row")
	}
}
