package engine

import (
	"context"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
)

// flow_compose_budget_test.go — what a preview and a save are allowed to cost.
//
// The store is pinned to ONE SQLite connection, so a statement on this path is
// a statement every board's poll waits behind, and a statement inside the
// save's transaction is one they wait behind EXCLUSIVELY. These pin both.

// TestPreviewFlow_ReadsOncePerRequest: the preview reads each thing once.
//
// It read GetProcess three times (here, inside the fingerprint, and again
// inside the planner), the process nodes three times, and walked the pull
// snapshot twice — 16 to 34 statements for one pass over one press.
func TestPreviewFlow_ReadsOncePerRequest(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	processID, _, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	cells := cellsOf(t, db, toStyleID)

	counter.Reset()
	if _, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{ToStyleID: toStyleID, Cells: cells}); err != nil {
		t.Fatalf("preview: %v", err)
	}
	// A bound, not an exact number: the planner's own reads are the planner's
	// business and this test is not the place to freeze them. What it holds is
	// that the preview does not re-read its own inputs.
	const budget = 16
	if n := counter.Count(); n > budget {
		t.Errorf("a preview issues %d statements, budget %d — something is being read twice", n, budget)
	}
}

// TestPreviewFlow_StopsOnAnAbortedRequest: a preview whose caller has gone
// does not finish.
//
// THE SPRINGFIELD ORPHAN-BUILD INCIDENT IS THIS SHAPE ONE LAYER UP: aborted
// view fetches piled builds onto the single connection faster than they
// drained, and BuildView took a context for exactly this reason. The composer
// previews every 400 ms while a flow is edited and the operator closing the
// sheet abandons all of them.
func TestPreviewFlow_StopsOnAnAbortedRequest(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := eng.PreviewFlow(ctx, processID, FlowPreviewRequest{ToStyleID: toStyleID, Cells: cellsOf(t, db, toStyleID)})
	if err == nil {
		t.Fatal("a preview on a cancelled request ran to completion")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("err = %v, want the cancellation", err)
	}
}

// TestSaveFlow_TransactionHoldsNoRoutingRead: the save's exclusive hold
// contains the claims reads and the writes, and nothing else.
//
// ListRoutingNodes was inside it — the most expensive query in the feature, a
// correlated COUNT(DISTINCT) with TRIM() on both sides that no index can
// serve — because the fingerprint hashed the routing set. It left the
// fingerprint (it is an offer list, not a flow) and so it left the
// transaction, along with a second read of the to-side claims the fingerprint
// pass had already taken.
//
// It counts STATEMENTS RATHER THAN TIME on purpose: what a board waits for is
// the hold, and the hold is the statements.
func TestSaveFlow_TransactionHoldsNoRoutingRead(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	// seedFlowScenario, not seedChangeoverScenario: its claims are in a
	// configurable swap mode, which is what the composer's validator accepts.
	processID, _, toStyleID := seedFlowScenario(t, db)
	enableComposer(t, db, processID)
	eng := testEngine(t, db)
	cells := cellsOf(t, db, toStyleID)

	draft, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{ToStyleID: toStyleID, Cells: cells})
	testutil.MustNoErr(t, err, "preview")

	counter.Reset()
	res, err := eng.SaveFlow(processID, FlowSaveRequest{
		ToStyleID: toStyleID, Cells: cells, Fingerprint: draft.Fingerprint,
		Source: domain.ClaimSourceHMI, CalledBy: "Press 400",
	})
	testutil.MustNoErr(t, err, "save")
	// The whole save: the pre-transaction reads (process, changeover, style,
	// claim context) and the transaction (process, both sides' claims, the
	// writes, the read-back). A bound, so the shape is held without freezing
	// the store's internals.
	const budget = 20
	if n := counter.Count(); n > budget {
		t.Errorf("a save issues %d statements, budget %d", n, budget)
	}

	// THE FINGERPRINT IT RETURNED IS THE ONE A FRESH CHECK COMPUTES. This is
	// the property that makes returning it from inside the transaction safe:
	// the rows are read back after the writes, so ids and sequences the store
	// assigned are in the hash. A fingerprint built from the draft instead
	// would carry id 0 for every new row and turn the next save into
	// ErrFlowStale.
	fresh, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "fresh fingerprint")
	if res.Fingerprint != fresh {
		t.Errorf("the save returned %q and a fresh check computes %q", res.Fingerprint, fresh)
	}

	// AND ADOPTING A ROUTING ROW DOES NOT STALE IT. The routing set is an
	// offer list; a save that follows this still matches.
	rowID, err := db.UpsertRoutingNode(domain.RoutingNodeInput{
		ProcessID: processID, CoreNodeName: "BUF-BUDGET", Role: domain.RoutingRoleSource,
		Origin: domain.RoutingOriginEngineer,
	})
	testutil.MustNoErr(t, err, "routing row")
	testutil.MustNoErr(t, db.SetRoutingNodeEnabled(processID, rowID, true, "test"), "adopt")
	after, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "fingerprint after adopting")
	if after != fresh {
		t.Error("adopting a routing lane invalidated every open preview on the press")
	}
}
