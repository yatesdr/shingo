package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// core_unreachable_messages_test.go — what the operator is told when the Edge
// could not look.
//
// FetchNodeBins returns (bins, reachable, err) because it used to return
// (nil, nil) for all four failure modes, so "Core says no bin is there" and
// "Core did not answer" were one value — the collapse behind the 2026-07-31
// Springfield over-ordering incident. Ten of fifteen call sites still discard
// the flag.
//
// The census found something better than expected and worse than comfortable:
// NO production site fails open. Every one refuses. What four of them do is
// tell the operator a positive falsehood about the physical world — "no bin at
// node X" — on a read that never completed, to somebody standing in front of
// the bin. LoadBin's version is the worst because it prescribes a corrective
// action ("request an empty bin first") that the request path then refuses too,
// for the same underlying reason, with a different sentence.

// unreachableCore points the engine at a server that refuses every request, so
// FetchNodeBins returns reachable=false with ErrCoreHTTPStatus.
func unreachableCore(t *testing.T, eng *Engine) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	eng.coreClient = NewCoreClient(srv.URL)
}

func seedManualSwapConsume(t *testing.T, db *store.DB, coreNode string) int64 {
	t.Helper()
	processID, err := db.CreateProcess("UNREACH-"+coreNode, "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: coreNode, Code: "U-" + coreNode,
		Name: coreNode, Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	styleID, err := db.CreateStyle("UNREACH-STYLE-"+coreNode, "", processID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &styleID), "set active style")
	_, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: coreNode, Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeManualSwap, PayloadCode: "PART-A", UOPCapacity: 100,
		OutboundDestination: "OUT",
	})
	testutil.MustNoErr(t, err, "upsert claim")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	return nodeID
}

func TestPushEmptyOut_UnreachableCoreSaysSoInsteadOfClaimingTheSlotIsEmpty(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedManualSwapConsume(t, db, "PUSH-NODE")
	unreachableCore(t, eng)

	err := eng.PushEmptyOut(nodeID)
	if err == nil {
		t.Fatal("PushEmptyOut succeeded with Core unreachable — it must fail closed")
	}
	if strings.Contains(err.Error(), "has no bin to push") {
		t.Errorf("message = %q. That is a claim about the physical world made from a read that "+
			"never completed, told to an operator looking at the carrier.", err)
	}
	if !strings.Contains(err.Error(), "http_error") {
		t.Errorf("message = %q, want the occupancy outcome named so an incident is "+
			"reconstructable from logs", err)
	}
}

func TestLoadBin_UnreachableCoreDoesNotPrescribeAWrongFix(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedManualSwapConsume(t, db, "LOAD-NODE")
	unreachableCore(t, eng)

	err := eng.LoadBin(nodeID, "PART-A", 50, []protocol.IngestManifestItem{{PartNumber: "PART-A", Quantity: 50}})
	if err == nil {
		t.Fatal("LoadBin succeeded with Core unreachable — it must fail closed")
	}
	if strings.Contains(err.Error(), "request an empty bin first") {
		t.Errorf("message = %q. The request path refuses on the same failure (claimOccupancy "+
			"assumes occupied), so following this instruction produces a second contradictory "+
			"refusal.", err)
	}
	if !strings.Contains(err.Error(), "http_error") {
		t.Errorf("message = %q, want the occupancy outcome named", err)
	}
}
