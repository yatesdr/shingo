package engine

import (
	"shingoedge/orders"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
)

// Tier 1 gates — "one read-model" (collapse the dual loader-resolution layer).
// These guard the two correctness bugs the refactor closes plus the prerequisites
// the deletion depends on. See TIER1-BRIEF.md.

// Gate 1 (BUG-1, one identity for one loader): the operator path (RequestEmptyBin)
// and the L1 push (stageOperatorEmpty on the aggregate loader) must name the SAME
// loader for one physical loader. Pre-fix the operator path built its loader from
// the claim (ID = node NAME) while the push used the aggregate (ID = token), and the
// two were counted apart. The count is Core's now (the loader.retrieve ask), so the
// identity is the ask's loader_key: both paths must send the token.
func TestTier1_BUG1_OperatorAndDemandShareLoaderKey(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	core := answerEmpties(t, eng)

	// Single-window real-node loader: node "PLK_X1", aggregate token "loader:PLK_X1".
	nodeID := seedCapManualSwap(t, db, "PROC-X1", "PLK_X1", protocol.ClaimRoleProduce, []string{"PART-X"}, 2, false)
	seedCoreLoader(t, eng, sharedLoaderInfo("PLK_X1", "produce", "threshold", "PART-X", 0, 100))

	if _, err := eng.RequestEmptyBin(nodeID, "PART-X"); err != nil {
		t.Fatalf("RequestEmptyBin: %v", err)
	}
	if got := core.lastAsk().LoaderKey; got != "loader:PLK_X1" {
		t.Errorf("operator path asked for loader %q, want the loader_key token loader:PLK_X1 (not the node name)", got)
	}
	dl, err := eng.loaders().LoaderAt("PLK_X1", domain.RoleProduce)
	if err != nil || dl == nil {
		t.Fatalf("LoaderAt(PLK_X1) = %v, %v", dl, err)
	}
	if _, err := eng.stageOperatorEmpty(dl, "PART-X", 1, "", orders.NoDemand()); err != nil {
		t.Fatalf("stageOperatorEmpty: %v", err)
	}
	if got := core.lastAsk().LoaderKey; got != "loader:PLK_X1" {
		t.Errorf("push asked for loader %q, want loader:PLK_X1", got)
	}
}

// NOTE: the original Gate 2 (BUG-2, dead C-push skip via MaybeCreateLoaderEmptyIn)
// is GONE — the bin-count produce path it skipped is fully retired, so there is no
// bin-count path that could fire for a threshold-opted payload. BUG-2 is structurally
// closed (not just skipped). The threshold path is exercised by
// handle_loop_below_threshold_test.go; the operator-staging fallback by
// maybe_push_loader_test.go.

// Gate 3 (prerequisite A landed): the per-payload UOP threshold and the operator-driven
// flag must reach the *domain.Loader snapshot through projectCoreLoader and survive a
// SetCoreLoaders refresh. Without the uopThreshold projection the threshold silently
// reads 0 (and a threshold loader would fall back to operator staging).
func TestTier1_SnapshotCarriesThresholdAndTransitional(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	seedCoreLoader(t, eng,
		sharedLoaderInfo("AUTO-LDR", "produce", "threshold", "PART-A", 2, 150),
		sharedLoaderInfo("OP-LDR", "produce", "operator", "PART-B", 1, 0),
	)

	a, err := eng.loaders().LoaderForPayload("PART-A", domain.RoleProduce)
	if err != nil || a == nil {
		t.Fatalf("resolve AUTO-LDR: %v", err)
	}
	if got := a.UOPThresholdFor("PART-A"); got != 150 {
		t.Errorf("UOPThresholdFor(PART-A) = %d, want 150 (threaded off the cache)", got)
	}
	if a.UsesOperatorStaging() {
		t.Error("replenishment=auto loader must not be operator-driven")
	}

	op, err := eng.loaders().LoaderForPayload("PART-B", domain.RoleProduce)
	if err != nil || op == nil {
		t.Fatalf("resolve OP-LDR: %v", err)
	}
	if !op.UsesOperatorStaging() {
		t.Error("replenishment=operator loader must be operator-driven")
	}
	if got := op.UOPThresholdFor("PART-B"); got != 0 {
		t.Errorf("UOPThresholdFor(PART-B) = %d, want 0 (none seeded)", got)
	}

	// Survives a refresh: the field is re-threaded, not stale.
	seedCoreLoader(t, eng, sharedLoaderInfo("AUTO-LDR", "produce", "threshold", "PART-A", 2, 222))
	a2, err := eng.loaders().LoaderForPayload("PART-A", domain.RoleProduce)
	if err != nil {
		t.Fatalf("LoaderForPayload(PART-A) after refresh: %v", err)
	}
	if a2 == nil || a2.UOPThresholdFor("PART-A") != 222 {
		t.Errorf("after refresh UOPThresholdFor(PART-A) = %v, want 222", a2)
	}
}

// Gate 4 (dedicated layout preserved): the path RequestEmptyBin/the push now resolve
// through — e.loaders().LoaderAt — must keep a dedicated loader's dedicated_positions
// layout and route a member-named reservation to THAT position (O2). The deleted shim
// flattened every loader to shared_window; this pins the model the aggregate preserves.
func TestTier1_DedicatedLoaderKeepsLayoutViaAggregate(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	seedCoreLoader(t, eng, protocol.LoaderInfo{
		Name: "DECK", LoaderKey: "loader:DECK", Role: "produce", Layout: "dedicated_positions",
		Replenishment: "threshold",
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "D1", PayloadCode: "STUD"},
			{CoreNodeName: "D2", PayloadCode: "STUD"},
		},
	})

	l, err := eng.loaders().LoaderAt("D2", domain.RoleProduce)
	if err != nil || l == nil {
		t.Fatalf("LoaderAt(D2) = %v, %v", l, err)
	}
	if !l.IsDedicated() {
		t.Errorf("layout = %v, want dedicated_positions (the shim flattened to shared_window)", l.Layout())
	}
	// O2 member routing: a reservation naming D2 routes to D2, not first-match D1.
	nodes, budget := l.ReservationTarget("D2", "STUD", eng.multiWindowFor(l))
	if len(nodes) != 1 || nodes[0] != "D2" || budget != 1 {
		t.Errorf("ReservationTarget(member=D2) = %v/%d, want [D2]/1", nodes, budget)
	}
}
