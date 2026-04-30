package dispatch

import (
	"testing"

	"shingocore/domain"
	"shingocore/store/bins"
)

// availBin is a tiny constructor for an unclaimed, available bin matching
// the given payload — the happy-path candidate. Tests can mutate the
// returned pointer to exercise reject reasons.
func availBin(id int64, label, payload string) *bins.Bin {
	return &domain.Bin{
		ID:          id,
		Label:       label,
		PayloadCode: payload,
		Status:      domain.BinStatusAvailable,
	}
}

func TestBuildComplexPlan_SimpleRetrieve(t *testing.T) {
	t.Parallel()
	steps := []resolvedStep{
		{Action: "pickup", Node: "storage.A1"},
		{Action: "dropoff", Node: "line.L1"},
	}
	candidates := map[string][]*bins.Bin{
		"storage.A1": {availBin(100, "B100", "PART-X")},
	}

	plan := BuildComplexPlan(steps, candidates, "PART-X", "line.L1")

	if plan.SourceNode != "storage.A1" {
		t.Errorf("SourceNode = %q, want storage.A1", plan.SourceNode)
	}
	if plan.DeliveryNode != "line.L1" {
		t.Errorf("DeliveryNode = %q, want line.L1", plan.DeliveryNode)
	}
	if len(plan.BinClaims) != 1 || plan.BinClaims[0].BinID != 100 {
		t.Fatalf("BinClaims = %+v, want one entry for bin 100", plan.BinClaims)
	}
	if plan.BinClaims[0].IsProcessNode {
		t.Errorf("storage pickup should not be IsProcessNode")
	}
	if plan.HasWait {
		t.Errorf("HasWait = true, want false (no wait in steps)")
	}
	if len(plan.PreWaitSteps) != 2 {
		t.Errorf("PreWaitSteps len = %d, want 2", len(plan.PreWaitSteps))
	}
	if len(plan.Skips) != 0 {
		t.Errorf("Skips = %+v, want empty", plan.Skips)
	}
	if plan.PerBinDestinations != nil {
		t.Errorf("PerBinDestinations = %+v, want nil for single-bin plan", plan.PerBinDestinations)
	}
}

func TestBuildComplexPlan_ProcessNodePickupFlagged(t *testing.T) {
	t.Parallel()
	// The pickup at the order's source node (the process node) should be
	// flagged so apply-time logic knows to attach the operator's
	// RemainingUOP signal there and not at storage hops.
	steps := []resolvedStep{
		{Action: "pickup", Node: "line.L1"}, // process node
		{Action: "dropoff", Node: "storage.A1"},
	}
	candidates := map[string][]*bins.Bin{
		"line.L1": {availBin(200, "B200", "PART-Y")},
	}

	plan := BuildComplexPlan(steps, candidates, "PART-Y", "line.L1")

	if len(plan.BinClaims) != 1 {
		t.Fatalf("BinClaims = %+v, want one entry", plan.BinClaims)
	}
	if !plan.BinClaims[0].IsProcessNode {
		t.Errorf("process-node pickup should be IsProcessNode=true")
	}
}

func TestBuildComplexPlan_WaitSplit(t *testing.T) {
	t.Parallel()
	steps := []resolvedStep{
		{Action: "pickup", Node: "storage"},
		{Action: "dropoff", Node: "staging"},
		{Action: "wait"},
		{Action: "pickup", Node: "staging"},
		{Action: "dropoff", Node: "line"},
	}
	candidates := map[string][]*bins.Bin{
		"storage": {availBin(300, "B300", "PART-Z")},
		"staging": {availBin(301, "B301", "PART-Z")}, // not actually picked at apply (in-flight bin), but test still records selection
	}

	plan := BuildComplexPlan(steps, candidates, "PART-Z", "")

	if !plan.HasWait {
		t.Errorf("HasWait = false, want true")
	}
	if len(plan.PreWaitSteps) != 2 {
		t.Errorf("PreWaitSteps len = %d, want 2 (steps before bare wait)", len(plan.PreWaitSteps))
	}
}

func TestBuildComplexPlan_NoBinsAtNode(t *testing.T) {
	t.Parallel()
	steps := []resolvedStep{
		{Action: "pickup", Node: "storage.A1"},
		{Action: "dropoff", Node: "line.L1"},
	}
	candidates := map[string][]*bins.Bin{
		"storage.A1": {}, // resolved fine, just empty
	}

	plan := BuildComplexPlan(steps, candidates, "PART-X", "line.L1")

	if len(plan.BinClaims) != 0 {
		t.Errorf("BinClaims = %+v, want empty", plan.BinClaims)
	}
	if len(plan.Skips) != 1 || plan.Skips[0].reason != "no bins at node" {
		t.Errorf("Skips = %+v, want one entry with reason 'no bins at node'", plan.Skips)
	}
}

func TestBuildComplexPlan_AllCandidatesRejected(t *testing.T) {
	t.Parallel()
	// One bin available but claimed by another order, one with the wrong
	// payload — both rejects are enumerated in the skip reason so operators
	// can see why no bin was selected.
	otherOrder := int64(99)
	wrongPayload := availBin(401, "WRONG", "OTHER-PART")
	claimed := availBin(402, "BUSY", "PART-X")
	claimed.ClaimedBy = &otherOrder

	steps := []resolvedStep{
		{Action: "pickup", Node: "storage.A1"},
		{Action: "dropoff", Node: "line.L1"},
	}
	candidates := map[string][]*bins.Bin{
		"storage.A1": {wrongPayload, claimed},
	}

	plan := BuildComplexPlan(steps, candidates, "PART-X", "line.L1")

	if len(plan.BinClaims) != 0 {
		t.Errorf("BinClaims = %+v, want empty", plan.BinClaims)
	}
	if len(plan.Skips) != 1 {
		t.Fatalf("Skips len = %d, want 1", len(plan.Skips))
	}
	got := plan.Skips[0].reason
	if !contains(got, "no candidate among 2 bin(s)") || !contains(got, "WRONG") || !contains(got, "BUSY") {
		t.Errorf("skip reason missing expected detail: %q", got)
	}
}

func TestBuildComplexPlan_MultiPickupFillsDestinations(t *testing.T) {
	t.Parallel()
	// The 9-step swap pattern from complex_test.go's resolvePerBinDestinations
	// coverage. Plan should record per-bin destinations so apply can write
	// the order_bins junction rows.
	steps := []resolvedStep{
		{Action: "pickup", Node: "storage"},
		{Action: "dropoff", Node: "inStaging"},
		{Action: "wait", Node: "line"},
		{Action: "pickup", Node: "line"},
		{Action: "dropoff", Node: "outStaging"},
		{Action: "pickup", Node: "inStaging"},
		{Action: "dropoff", Node: "line"},
		{Action: "pickup", Node: "outStaging"},
		{Action: "dropoff", Node: "outDest"},
	}
	candidates := map[string][]*bins.Bin{
		"storage":    {availBin(101, "newBin", "PART-X")},
		"line":       {availBin(102, "oldBin", "PART-X")},
		"inStaging":  {availBin(101, "newBin", "PART-X")}, // re-pickup of in-flight bin
		"outStaging": {availBin(102, "oldBin", "PART-X")},
	}

	plan := BuildComplexPlan(steps, candidates, "PART-X", "line")

	// Two distinct bins should be recorded as primary claims (storage @ step 0,
	// line @ step 3); the re-pickups at step 5 / 7 select bins again but the
	// destination simulator treats them as the same bin moving through the plan.
	if len(plan.BinClaims) < 2 {
		t.Fatalf("expected at least 2 bin claims, got %+v", plan.BinClaims)
	}
	if plan.PerBinDestinations[101] != "line" {
		t.Errorf("bin 101 final dest = %q, want line", plan.PerBinDestinations[101])
	}
	if plan.PerBinDestinations[102] != "outDest" {
		t.Errorf("bin 102 final dest = %q, want outDest", plan.PerBinDestinations[102])
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
