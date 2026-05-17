package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// findDiff returns the diff for the named node, or nil if not found.
func findDiff(diffs []ChangeoverNodeDiff, nodeName string) *ChangeoverNodeDiff {
	for i := range diffs {
		if diffs[i].CoreNodeName == nodeName {
			return &diffs[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// DiffStyleClaims — single-node situation tests
// ---------------------------------------------------------------------------

func TestDiffStyleClaims_Swap(t *testing.T) {
	t.Parallel()
	diffs := DiffStyleClaims(
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"}},
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-B", Role: "consume"}},
	)
	d := findDiff(diffs, "N1")
	if d == nil {
		t.Fatal("missing diff for N1")
	}
	if d.Situation != SituationSwap {
		t.Errorf("situation = %q, want %q", d.Situation, SituationSwap)
	}
	if d.FromClaim == nil || d.ToClaim == nil {
		t.Error("expected both FromClaim and ToClaim to be non-nil for swap")
	}
}

func TestDiffStyleClaims_Evacuate(t *testing.T) {
	t.Parallel()
	diffs := DiffStyleClaims(
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"}},
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume", EvacuateOnChangeover: true}},
	)
	d := findDiff(diffs, "N1")
	if d == nil {
		t.Fatal("missing diff for N1")
	}
	if d.Situation != SituationEvacuate {
		t.Errorf("situation = %q, want %q", d.Situation, SituationEvacuate)
	}
}

func TestDiffStyleClaims_Unchanged(t *testing.T) {
	t.Parallel()
	diffs := DiffStyleClaims(
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"}},
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"}},
	)
	d := findDiff(diffs, "N1")
	if d == nil {
		t.Fatal("missing diff for N1")
	}
	if d.Situation != SituationUnchanged {
		t.Errorf("situation = %q, want %q", d.Situation, SituationUnchanged)
	}
}

func TestDiffStyleClaims_Add(t *testing.T) {
	t.Parallel()
	diffs := DiffStyleClaims(
		nil, // no from-style claims
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"}},
	)
	d := findDiff(diffs, "N1")
	if d == nil {
		t.Fatal("missing diff for N1")
	}
	if d.Situation != SituationAdd {
		t.Errorf("situation = %q, want %q", d.Situation, SituationAdd)
	}
	if d.FromClaim != nil {
		t.Error("FromClaim should be nil for add")
	}
	if d.ToClaim == nil {
		t.Error("ToClaim should be non-nil for add")
	}
}

func TestDiffStyleClaims_Drop(t *testing.T) {
	t.Parallel()
	diffs := DiffStyleClaims(
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"}},
		nil,
	)
	d := findDiff(diffs, "N1")
	if d == nil {
		t.Fatal("missing diff for N1")
	}
	if d.Situation != SituationDrop {
		t.Errorf("situation = %q, want %q", d.Situation, SituationDrop)
	}
	if d.FromClaim == nil {
		t.Error("FromClaim should be non-nil for drop")
	}
	if d.ToClaim != nil {
		t.Error("ToClaim should be nil for drop")
	}
}

// ---------------------------------------------------------------------------
// __empty__ payload handling
// ---------------------------------------------------------------------------

// To-claim with __empty__ → explicitly clear the node (SituationDrop).
func TestDiffStyleClaims_ToEmpty(t *testing.T) {
	t.Parallel()
	diffs := DiffStyleClaims(
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"}},
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "__empty__", Role: "consume"}},
	)
	d := findDiff(diffs, "N1")
	if d == nil {
		t.Fatal("missing diff for N1")
	}
	if d.Situation != SituationDrop {
		t.Errorf("situation = %q, want %q (explicitly clear node)", d.Situation, SituationDrop)
	}
}

// From-claim with __empty__ → node was empty, now needs material (SituationAdd).
func TestDiffStyleClaims_FromEmpty(t *testing.T) {
	t.Parallel()
	diffs := DiffStyleClaims(
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "__empty__", Role: "consume"}},
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"}},
	)
	d := findDiff(diffs, "N1")
	if d == nil {
		t.Fatal("missing diff for N1")
	}
	if d.Situation != SituationAdd {
		t.Errorf("situation = %q, want %q (was empty, needs material)", d.Situation, SituationAdd)
	}
}

// New node with __empty__ → node was empty, stays empty (SituationUnchanged).
func TestDiffStyleClaims_ToEmptyNewNode(t *testing.T) {
	t.Parallel()
	diffs := DiffStyleClaims(
		nil,
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "__empty__", Role: "consume"}},
	)
	d := findDiff(diffs, "N1")
	if d == nil {
		t.Fatal("missing diff for N1")
	}
	if d.Situation != SituationUnchanged {
		t.Errorf("situation = %q, want %q (new node with __empty__ stays unchanged)", d.Situation, SituationUnchanged)
	}
}

// Both from and to have __empty__ → nothing changes (SituationUnchanged).
func TestDiffStyleClaims_BothEmpty(t *testing.T) {
	t.Parallel()
	diffs := DiffStyleClaims(
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "__empty__", Role: "consume"}},
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "__empty__", Role: "consume"}},
	)
	d := findDiff(diffs, "N1")
	if d == nil {
		t.Fatal("missing diff for N1")
	}
	if d.Situation != SituationUnchanged {
		t.Errorf("situation = %q, want %q (both empty → nothing to do)", d.Situation, SituationUnchanged)
	}
}

// ---------------------------------------------------------------------------
// Changeover-only role
// ---------------------------------------------------------------------------

func TestDiffStyleClaims_ChangeoverRole_FromSide(t *testing.T) {
	t.Parallel()
	diffs := DiffStyleClaims(
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "changeover"}},
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"}},
	)
	d := findDiff(diffs, "N1")
	if d == nil {
		t.Fatal("missing diff for N1")
	}
	if d.Situation != SituationEvacuate {
		t.Errorf("situation = %q, want %q (from-side changeover role forces evacuate)", d.Situation, SituationEvacuate)
	}
}

func TestDiffStyleClaims_ChangeoverRole_ToSide(t *testing.T) {
	t.Parallel()
	diffs := DiffStyleClaims(
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"}},
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "changeover"}},
	)
	d := findDiff(diffs, "N1")
	if d == nil {
		t.Fatal("missing diff for N1")
	}
	if d.Situation != SituationEvacuate {
		t.Errorf("situation = %q, want %q (to-side changeover role forces evacuate)", d.Situation, SituationEvacuate)
	}
}

// Changeover role overrides EvacuateOnChangeover=false — evacuate is forced.
func TestDiffStyleClaims_ChangeoverRoleOverridesNoEvacuate(t *testing.T) {
	t.Parallel()
	diffs := DiffStyleClaims(
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "changeover"}},
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "changeover"}},
	)
	d := findDiff(diffs, "N1")
	if d == nil {
		t.Fatal("missing diff for N1")
	}
	if d.Situation != SituationEvacuate {
		t.Errorf("situation = %q, want %q (changeover role always evacuates)", d.Situation, SituationEvacuate)
	}
}

// ---------------------------------------------------------------------------
// Multi-node diff — mix of situations in one call
// ---------------------------------------------------------------------------

func TestDiffStyleClaims_MultiNode(t *testing.T) {
	t.Parallel()
	from := []processes.NodeClaim{
		{CoreNodeName: "SWAP-NODE", PayloadCode: "OLD", Role: "consume"},
		{CoreNodeName: "UNCHANGED-NODE", PayloadCode: "SAME", Role: "consume"},
		{CoreNodeName: "EVAC-NODE", PayloadCode: "SAME-E", Role: "consume"},
		{CoreNodeName: "DROP-NODE", PayloadCode: "DROP-PART", Role: "consume"},
		{CoreNodeName: "CO-NODE", PayloadCode: "CO-PART", Role: "changeover"},
	}
	to := []processes.NodeClaim{
		{CoreNodeName: "SWAP-NODE", PayloadCode: "NEW", Role: "consume"},
		{CoreNodeName: "UNCHANGED-NODE", PayloadCode: "SAME", Role: "consume"},
		{CoreNodeName: "EVAC-NODE", PayloadCode: "SAME-E", Role: "consume", EvacuateOnChangeover: true},
		{CoreNodeName: "ADD-NODE", PayloadCode: "NEW-PART", Role: "consume"},
		{CoreNodeName: "CO-NODE", PayloadCode: "CO-PART", Role: "consume"},
	}

	diffs := DiffStyleClaims(from, to)
	if len(diffs) != 6 { // SWAP, UNCHANGED, EVAC, DROP, CO, ADD
		t.Errorf("expected 6 diffs, got %d", len(diffs))
	}

	cases := []struct {
		node      string
		situation ChangeoverSituation
	}{
		{"SWAP-NODE", SituationSwap},
		{"UNCHANGED-NODE", SituationUnchanged},
		{"EVAC-NODE", SituationEvacuate},
		{"DROP-NODE", SituationDrop},
		{"ADD-NODE", SituationAdd},
		{"CO-NODE", SituationEvacuate},
	}
	for _, tc := range cases {
		d := findDiff(diffs, tc.node)
		if d == nil {
			t.Errorf("missing diff for %s", tc.node)
			continue
		}
		if d.Situation != tc.situation {
			t.Errorf("%s: situation = %q, want %q", tc.node, d.Situation, tc.situation)
		}
	}
}

// ---------------------------------------------------------------------------
// Nil from-claims (first-ever changeover, no active style)
// ---------------------------------------------------------------------------

func TestDiffStyleClaims_NilFromClaims(t *testing.T) {
	t.Parallel()
	to := []processes.NodeClaim{
		{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"},
		{CoreNodeName: "N2", PayloadCode: "PART-B", Role: "produce"},
	}
	diffs := DiffStyleClaims(nil, to)
	if len(diffs) != 2 {
		t.Fatalf("expected 2 diffs, got %d", len(diffs))
	}
	for _, name := range []string{"N1", "N2"} {
		d := findDiff(diffs, name)
		if d == nil {
			t.Fatalf("missing diff for %s", name)
		}
		if d.Situation != SituationAdd {
			t.Errorf("%s: situation = %q, want %q (first-ever changeover)", name, d.Situation, SituationAdd)
		}
		if d.FromClaim != nil {
			t.Errorf("%s: FromClaim should be nil when from-claims is empty", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Role change with same payload → Swap (not Unchanged)
// ---------------------------------------------------------------------------

func TestDiffStyleClaims_RoleChange(t *testing.T) {
	t.Parallel()
	diffs := DiffStyleClaims(
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"}},
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "produce"}},
	)
	d := findDiff(diffs, "N1")
	if d == nil {
		t.Fatal("missing diff for N1")
	}
	if d.Situation != SituationSwap {
		t.Errorf("situation = %q, want %q (same payload, different role = swap)", d.Situation, SituationSwap)
	}
}

// ---------------------------------------------------------------------------
// EvacuateOnChangeover ignored when payload differs (still Swap)
// ---------------------------------------------------------------------------

func TestDiffStyleClaims_EvacuateFlagIgnoredOnPayloadChange(t *testing.T) {
	t.Parallel()
	diffs := DiffStyleClaims(
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"}},
		[]processes.NodeClaim{{CoreNodeName: "N1", PayloadCode: "PART-B", Role: "consume", EvacuateOnChangeover: true}},
	)
	d := findDiff(diffs, "N1")
	if d == nil {
		t.Fatal("missing diff for N1")
	}
	if d.Situation != SituationSwap {
		t.Errorf("situation = %q, want %q (payload change overrides evacuate flag)", d.Situation, SituationSwap)
	}
}

// ---------------------------------------------------------------------------
// ApplyReuseCompatibleBinsShortcut
// ---------------------------------------------------------------------------

// Press-index Swap with matching payload + reuse flag set + empty bin →
// Unchanged (skip the swap entirely).
func TestApplyReuseCompatibleBinsShortcut_SkipsWhenAllConditionsMet(t *testing.T) {
	t.Parallel()
	from := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume", SwapMode: "two_robot_press_index", ReuseCompatibleBins: true}
	to := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume", SwapMode: "two_robot_press_index"}
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "N1", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	out := ApplyReuseCompatibleBinsShortcut(diffs, func(string) bool { return true })

	if out[0].Situation != SituationUnchanged {
		t.Errorf("situation = %q, want Unchanged", out[0].Situation)
	}
}

// Reuse flag false → no shortcut, Swap stays Swap.
func TestApplyReuseCompatibleBinsShortcut_FlagFalseStillSwaps(t *testing.T) {
	t.Parallel()
	from := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume", SwapMode: "two_robot_press_index", ReuseCompatibleBins: false}
	to := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume", SwapMode: "two_robot_press_index"}
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "N1", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	out := ApplyReuseCompatibleBinsShortcut(diffs, func(string) bool { return true })

	if out[0].Situation != SituationSwap {
		t.Errorf("situation = %q, want Swap (reuse flag off)", out[0].Situation)
	}
}

// Bin not empty → no shortcut.
func TestApplyReuseCompatibleBinsShortcut_BinNotEmptyStillSwaps(t *testing.T) {
	t.Parallel()
	from := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume", SwapMode: "two_robot_press_index", ReuseCompatibleBins: true}
	to := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume", SwapMode: "two_robot_press_index"}
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "N1", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	out := ApplyReuseCompatibleBinsShortcut(diffs, func(string) bool { return false })

	if out[0].Situation != SituationSwap {
		t.Errorf("situation = %q, want Swap (bin not empty)", out[0].Situation)
	}
}

// Non press-index mode → never shortcuts.
func TestApplyReuseCompatibleBinsShortcut_NonPressIndexIgnored(t *testing.T) {
	t.Parallel()
	from := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume", SwapMode: "two_robot", ReuseCompatibleBins: true}
	to := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume", SwapMode: "two_robot"}
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "N1", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	out := ApplyReuseCompatibleBinsShortcut(diffs, func(string) bool { return true })

	if out[0].Situation != SituationSwap {
		t.Errorf("situation = %q, want Swap (non press-index ignored)", out[0].Situation)
	}
}

// Different payloads → never shortcuts even with flag set + empty bin.
func TestApplyReuseCompatibleBinsShortcut_DifferentPayloadStillSwaps(t *testing.T) {
	t.Parallel()
	from := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume", SwapMode: "two_robot_press_index", ReuseCompatibleBins: true}
	to := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-B", Role: "consume", SwapMode: "two_robot_press_index"}
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "N1", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	out := ApplyReuseCompatibleBinsShortcut(diffs, func(string) bool { return true })

	if out[0].Situation != SituationSwap {
		t.Errorf("situation = %q, want Swap (payload differs)", out[0].Situation)
	}
}

// nil isEmpty accessor → defensive default, no shortcut applied.
func TestApplyReuseCompatibleBinsShortcut_NilAccessorPreservesSwap(t *testing.T) {
	t.Parallel()
	from := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume", SwapMode: "two_robot_press_index", ReuseCompatibleBins: true}
	to := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume", SwapMode: "two_robot_press_index"}
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "N1", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	out := ApplyReuseCompatibleBinsShortcut(diffs, nil)

	if out[0].Situation != SituationSwap {
		t.Errorf("situation = %q, want Swap (nil accessor must not shortcut)", out[0].Situation)
	}
}

// ---------------------------------------------------------------------------
// FanOutPressIndexDifferentBinType (per-position fan-out post-processor)
// ---------------------------------------------------------------------------

// pressIndexFanOutFromClaim builds a press-index claim with the given
// position layout for the fan-out tests. CoreNodeName / PairedCoreNode /
// SecondPairedCoreNode are passed in so position-count change cases
// can be tested.
func pressIndexFanOutClaim(payload, core, paired, second string) processes.NodeClaim {
	return processes.NodeClaim{
		CoreNodeName:         core,
		PairedCoreNode:       paired,
		SecondPairedCoreNode: second,
		PayloadCode:          payload,
		Role:                 "consume",
		SwapMode:             "two_robot_press_index",
		InboundSource:        "MARKET",
		OutboundDestination:  "DEST",
	}
}

// 3-pos to 3-pos with different bin types: parent diff replaced by 3
// per-position diffs, all SituationSwap with synthesized claims at
// pressPositionSwapMode.
func TestFanOutPressIndexDifferentBinType_3PosTo3Pos_Emits3Diffs(t *testing.T) {
	t.Parallel()
	from := pressIndexFanOutClaim("PART-A", "FRONT", "MID", "BACK")
	to := pressIndexFanOutClaim("PART-B", "FRONT", "MID", "BACK")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	binTypes := map[string]string{"PART-A": "T1", "PART-B": "T2"}
	out := FanOutPressIndexDifferentBinType(diffs, binTypes)

	if len(out) != 3 {
		t.Fatalf("expected 3 per-position diffs, got %d", len(out))
	}
	wantNodes := []string{"FRONT", "MID", "BACK"}
	for i, d := range out {
		if d.CoreNodeName != wantNodes[i] {
			t.Errorf("diff %d: CoreNodeName = %q, want %q", i, d.CoreNodeName, wantNodes[i])
		}
		if d.Situation != SituationSwap {
			t.Errorf("diff %d: Situation = %q, want SituationSwap", i, d.Situation)
		}
		if d.FromClaim == nil || d.ToClaim == nil {
			t.Fatalf("diff %d: both claims must be non-nil for full per-position swap", i)
		}
		if d.FromClaim.SwapMode != pressPositionSwapMode {
			t.Errorf("diff %d: FromClaim.SwapMode = %q, want pressPositionSwapMode", i, d.FromClaim.SwapMode)
		}
		if d.FromClaim.CoreNodeName != wantNodes[i] {
			t.Errorf("diff %d: FromClaim.CoreNodeName = %q, want %q", i, d.FromClaim.CoreNodeName, wantNodes[i])
		}
		// Synthesized claim must NOT carry parent's PairedCoreNode /
		// SecondPairedCoreNode fields — per-position is a single slot.
		if d.FromClaim.PairedCoreNode != "" || d.FromClaim.SecondPairedCoreNode != "" {
			t.Errorf("diff %d: synthesized claim must clear paired/second-paired fields, got paired=%q second=%q",
				i, d.FromClaim.PairedCoreNode, d.FromClaim.SecondPairedCoreNode)
		}
		// Parent's PayloadCode propagates (per the SME assumption check).
		if d.FromClaim.PayloadCode != "PART-A" {
			t.Errorf("diff %d: FromClaim.PayloadCode = %q, want parent PART-A", i, d.FromClaim.PayloadCode)
		}
		if d.ToClaim.PayloadCode != "PART-B" {
			t.Errorf("diff %d: ToClaim.PayloadCode = %q, want parent PART-B", i, d.ToClaim.PayloadCode)
		}
	}
}

// 3-pos to 2-pos: third position evacs but doesn't refill — emitted as
// a SituationDrop diff so the existing planner Drop branch handles it.
func TestFanOutPressIndexDifferentBinType_3PosTo2Pos_ThirdPositionDropOnly(t *testing.T) {
	t.Parallel()
	from := pressIndexFanOutClaim("PART-A", "FRONT", "MID", "BACK")
	to := pressIndexFanOutClaim("PART-B", "FRONT", "MID", "")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	binTypes := map[string]string{"PART-A": "T1", "PART-B": "T2"}
	out := FanOutPressIndexDifferentBinType(diffs, binTypes)

	if len(out) != 3 {
		t.Fatalf("expected 3 diffs (front+mid full swap, back drop-only), got %d", len(out))
	}
	for _, d := range out {
		if d.CoreNodeName == "BACK" {
			if d.Situation != SituationDrop {
				t.Errorf("BACK position: Situation = %q, want SituationDrop", d.Situation)
			}
			if d.ToClaim != nil {
				t.Error("BACK position: ToClaim must be nil for drop-only")
			}
			if d.FromClaim == nil {
				t.Error("BACK position: FromClaim must be non-nil for drop-only")
			}
		} else if d.Situation != SituationSwap {
			t.Errorf("%s position: Situation = %q, want SituationSwap", d.CoreNodeName, d.Situation)
		}
	}
}

// 2-pos to 3-pos: third position refills but didn't evac — emitted as
// SituationAdd so the existing planner Add branch handles it.
func TestFanOutPressIndexDifferentBinType_2PosTo3Pos_ThirdPositionAddOnly(t *testing.T) {
	t.Parallel()
	from := pressIndexFanOutClaim("PART-A", "FRONT", "MID", "")
	to := pressIndexFanOutClaim("PART-B", "FRONT", "MID", "BACK")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	binTypes := map[string]string{"PART-A": "T1", "PART-B": "T2"}
	out := FanOutPressIndexDifferentBinType(diffs, binTypes)

	if len(out) != 3 {
		t.Fatalf("expected 3 diffs (front+mid full swap, back add-only), got %d", len(out))
	}
	for _, d := range out {
		if d.CoreNodeName == "BACK" {
			if d.Situation != SituationAdd {
				t.Errorf("BACK position: Situation = %q, want SituationAdd", d.Situation)
			}
			if d.FromClaim != nil {
				t.Error("BACK position: FromClaim must be nil for add-only")
			}
			if d.ToClaim == nil {
				t.Error("BACK position: ToClaim must be non-nil for add-only")
			}
		} else if d.Situation != SituationSwap {
			t.Errorf("%s position: Situation = %q, want SituationSwap", d.CoreNodeName, d.Situation)
		}
	}
}

// Same bin type → no fan-out, parent diff stays untouched.
func TestFanOutPressIndexDifferentBinType_SameBinType_NoFanOut(t *testing.T) {
	t.Parallel()
	from := pressIndexFanOutClaim("PART-A", "FRONT", "MID", "BACK")
	to := pressIndexFanOutClaim("PART-B", "FRONT", "MID", "BACK")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	binTypes := map[string]string{"PART-A": "T1", "PART-B": "T1"} // same bin type
	out := FanOutPressIndexDifferentBinType(diffs, binTypes)

	if len(out) != 1 {
		t.Fatalf("same bin type → no fan-out, expected 1 diff, got %d", len(out))
	}
	if out[0].FromClaim.SwapMode != "two_robot_press_index" {
		t.Errorf("expected parent diff untouched, got SwapMode = %q", out[0].FromClaim.SwapMode)
	}
}

// Missing bin-type entries (catalog wasn't pre-resolved, or one payload
// has no rule) → fall through to no-fan-out (the conservative
// "treat as same-bin-type" fallback).
func TestFanOutPressIndexDifferentBinType_MissingEntries_NoFanOut(t *testing.T) {
	t.Parallel()
	from := pressIndexFanOutClaim("PART-A", "FRONT", "MID", "BACK")
	to := pressIndexFanOutClaim("PART-B", "FRONT", "MID", "BACK")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	cases := []map[string]string{
		nil,
		{},
		{"PART-A": "T1"},
		{"PART-A": "T1", "PART-B": ""},
	}
	for i, m := range cases {
		out := FanOutPressIndexDifferentBinType(diffs, m)
		if len(out) != 1 {
			t.Errorf("case %d: missing entries should not fan out, got %d diffs", i, len(out))
		}
	}
}

// Non-press-index diffs are passed through unchanged regardless of bin
// type signal — the post-processor only fans out two_robot_press_index.
func TestFanOutPressIndexDifferentBinType_NonPressIndexUnchanged(t *testing.T) {
	t.Parallel()
	from := processes.NodeClaim{
		CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume", SwapMode: "single_robot",
	}
	to := processes.NodeClaim{
		CoreNodeName: "N1", PayloadCode: "PART-B", Role: "consume", SwapMode: "single_robot",
	}
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "N1", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	binTypes := map[string]string{"PART-A": "T1", "PART-B": "T2"}
	out := FanOutPressIndexDifferentBinType(diffs, binTypes)

	if len(out) != 1 || out[0].FromClaim.SwapMode != "single_robot" {
		t.Errorf("non-press-index must pass through unchanged; got %d diffs, first SwapMode=%q",
			len(out), out[0].FromClaim.SwapMode)
	}
}

// Synthesized claim ID matches parent so node-task creation can persist
// references that resolve back to the real persisted claim.
func TestFanOutPressIndexDifferentBinType_SynthesizedClaimRetainsParentID(t *testing.T) {
	t.Parallel()
	from := pressIndexFanOutClaim("PART-A", "FRONT", "MID", "BACK")
	from.ID = 42
	to := pressIndexFanOutClaim("PART-B", "FRONT", "MID", "BACK")
	to.ID = 43
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	binTypes := map[string]string{"PART-A": "T1", "PART-B": "T2"}
	out := FanOutPressIndexDifferentBinType(diffs, binTypes)

	for _, d := range out {
		if d.FromClaim != nil && d.FromClaim.ID != 42 {
			t.Errorf("%s: synthesized FromClaim.ID = %d, want parent's 42", d.CoreNodeName, d.FromClaim.ID)
		}
		if d.ToClaim != nil && d.ToClaim.ID != 43 {
			t.Errorf("%s: synthesized ToClaim.ID = %d, want parent's 43", d.CoreNodeName, d.ToClaim.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// FanOutPressIndexCrossMode (cross-mode + extension-position fan-out)
// ---------------------------------------------------------------------------

// crossModeFromPressIndex builds a press-index from-claim with the
// specified position layout. Used by cross-mode tests to set up
// scenarios where the to-side has a different SwapMode.
func crossModeFromPressIndex(payload, core, paired, second string) processes.NodeClaim {
	return processes.NodeClaim{
		CoreNodeName:         core,
		PairedCoreNode:       paired,
		SecondPairedCoreNode: second,
		PayloadCode:          payload,
		Role:                 "consume",
		SwapMode:             "two_robot_press_index",
		InboundSource:        "MARKET",
		OutboundDestination:  "DEST",
	}
}

// crossModeNonPressIndex builds a non-press-index claim (single_robot,
// two_robot, or sequential) at a given CoreNodeName.
func crossModeNonPressIndex(core, payload string, swapMode protocol.SwapMode) processes.NodeClaim {
	return processes.NodeClaim{
		CoreNodeName:        core,
		PayloadCode:         payload,
		Role:                "consume",
		SwapMode:            swapMode,
		InboundSource:       "MARKET",
		OutboundDestination: "DEST",
		// Staging fields populated where the SwapMode normally needs
		// them; doesn't affect the cross-mode fan-out (which only looks
		// at the press-index claim's extension fields).
		InboundStaging:  "ISTG_" + core,
		OutboundStaging: "OSTG_" + core,
	}
}

// Direction A: from is press-index 3-pos, to is single_robot. The front
// position has a normal Swap diff (handled by single_robot's branch).
// Middle and back positions only exist on the from-claim's extension
// fields, so DiffStyleClaims doesn't emit diffs for them — the
// cross-mode fan-out synthesizes Drops so those bins evac to
// OutboundDestination.
func TestFanOutCrossMode_PressIndex3PosToSingleRobot_SynthesizesBackMiddleDrops(t *testing.T) {
	t.Parallel()
	from := crossModeFromPressIndex("PART-A", "FRONT", "MIDDLE", "BACK")
	to := crossModeNonPressIndex("FRONT", "PART-B", "single_robot")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	out := FanOutPressIndexCrossMode(diffs)

	// FRONT (already covered) + MIDDLE + BACK = 3 diffs total.
	if len(out) != 3 {
		t.Fatalf("expected 3 diffs (front + 2 synthesized drops), got %d", len(out))
	}
	frontSeen, middleSeen, backSeen := false, false, false
	for _, d := range out {
		switch d.CoreNodeName {
		case "FRONT":
			if d.Situation != SituationSwap {
				t.Errorf("FRONT: situation = %q, want SituationSwap (cross-mode fan-out must not touch front)", d.Situation)
			}
			frontSeen = true
		case "MIDDLE", "BACK":
			if d.Situation != SituationDrop {
				t.Errorf("%s: situation = %q, want SituationDrop", d.CoreNodeName, d.Situation)
			}
			if d.FromClaim == nil {
				t.Errorf("%s: FromClaim must be non-nil for synthesized drop", d.CoreNodeName)
			}
			if d.ToClaim != nil {
				t.Errorf("%s: ToClaim must be nil for drop-only", d.CoreNodeName)
			}
			if d.FromClaim != nil && d.FromClaim.OutboundDestination != "DEST" {
				t.Errorf("%s: synthesized FromClaim.OutboundDestination = %q, want parent's DEST",
					d.CoreNodeName, d.FromClaim.OutboundDestination)
			}
			if d.FromClaim != nil && d.FromClaim.PairedCoreNode != "" {
				t.Errorf("%s: synthesized FromClaim must clear PairedCoreNode, got %q",
					d.CoreNodeName, d.FromClaim.PairedCoreNode)
			}
			if d.CoreNodeName == "MIDDLE" {
				middleSeen = true
			} else {
				backSeen = true
			}
		}
	}
	if !frontSeen || !middleSeen || !backSeen {
		t.Errorf("expected front/middle/back diffs, got front=%v middle=%v back=%v",
			frontSeen, middleSeen, backSeen)
	}
}

// Direction B mirror: from is single_robot, to is press-index 3-pos.
// Front position has a normal Swap diff. Middle and back only exist on
// the to-claim's extension fields; the cross-mode fan-out synthesizes
// Adds so new bins get delivered to those positions from InboundSource.
func TestFanOutCrossMode_SingleRobotToPressIndex3Pos_SynthesizesBackMiddleAdds(t *testing.T) {
	t.Parallel()
	from := crossModeNonPressIndex("FRONT", "PART-A", "single_robot")
	to := crossModeFromPressIndex("PART-B", "FRONT", "MIDDLE", "BACK")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	out := FanOutPressIndexCrossMode(diffs)

	if len(out) != 3 {
		t.Fatalf("expected 3 diffs (front + 2 synthesized adds), got %d", len(out))
	}
	for _, d := range out {
		switch d.CoreNodeName {
		case "FRONT":
			if d.Situation != SituationSwap {
				t.Errorf("FRONT: situation = %q, want SituationSwap untouched", d.Situation)
			}
		case "MIDDLE", "BACK":
			if d.Situation != SituationAdd {
				t.Errorf("%s: situation = %q, want SituationAdd", d.CoreNodeName, d.Situation)
			}
			if d.FromClaim != nil {
				t.Errorf("%s: FromClaim must be nil for add-only", d.CoreNodeName)
			}
			if d.ToClaim == nil {
				t.Errorf("%s: ToClaim must be non-nil for synthesized add", d.CoreNodeName)
			}
			if d.ToClaim != nil && d.ToClaim.InboundSource != "MARKET" {
				t.Errorf("%s: synthesized ToClaim.InboundSource = %q, want parent's MARKET",
					d.CoreNodeName, d.ToClaim.InboundSource)
			}
			// Synthesized Add claim must clear InboundStaging so the
			// SituationAdd planner branch routes through the
			// direct-Retrieve fallback (delivery to position node)
			// rather than building a stage order.
			if d.ToClaim != nil && d.ToClaim.InboundStaging != "" {
				t.Errorf("%s: synthesized ToClaim must clear InboundStaging, got %q (would route to staging instead of position)",
					d.CoreNodeName, d.ToClaim.InboundStaging)
			}
		}
	}
}

// Cross-mode rule applies regardless of which non-press-index mode the
// other side uses. Pinning two_robot specifically because the
// implementation switches on "two_robot_press_index" only — any other
// SwapMode triggers the cross-mode fan-out the same way.
func TestFanOutCrossMode_PressIndex3PosToTwoRobot_SynthesizesBackMiddleDrops(t *testing.T) {
	t.Parallel()
	from := crossModeFromPressIndex("PART-A", "FRONT", "MIDDLE", "BACK")
	to := crossModeNonPressIndex("FRONT", "PART-B", "two_robot")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	out := FanOutPressIndexCrossMode(diffs)

	if len(out) != 3 {
		t.Fatalf("expected 3 diffs, got %d", len(out))
	}
	dropCount := 0
	for _, d := range out {
		if d.Situation == SituationDrop {
			dropCount++
		}
	}
	if dropCount != 2 {
		t.Errorf("expected 2 synthesized Drop diffs (middle + back), got %d", dropCount)
	}
}

// Direction C: both press-index, position-count differs, same bin type.
// The same-mode fan-out doesn't fire (bin types match); the cross-mode
// fan-out picks up the position-count delta. Here the from-style is
// 2-pos, to-style is 3-pos: BACK only appears in to-claim's extension,
// so the cross-mode fan-out synthesizes Add for it.
func TestFanOutCrossMode_PressIndex2PosToPressIndex3Pos_SynthesizesAddForNewPosition(t *testing.T) {
	t.Parallel()
	from := crossModeFromPressIndex("PART-A", "FRONT", "MIDDLE", "")
	to := crossModeFromPressIndex("PART-B", "FRONT", "MIDDLE", "BACK")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	out := FanOutPressIndexCrossMode(diffs)

	if len(out) != 2 {
		t.Fatalf("expected 2 diffs (front untouched + back add), got %d", len(out))
	}
	var backDiff *ChangeoverNodeDiff
	for i := range out {
		if out[i].CoreNodeName == "BACK" {
			backDiff = &out[i]
		}
	}
	if backDiff == nil {
		t.Fatal("expected synthesized BACK diff")
	}
	if backDiff.Situation != SituationAdd {
		t.Errorf("BACK situation = %q, want SituationAdd", backDiff.Situation)
	}
	if backDiff.FromClaim != nil {
		t.Error("BACK FromClaim must be nil (from-style didn't have this position)")
	}
	if backDiff.ToClaim == nil {
		t.Error("BACK ToClaim must be non-nil")
	}
	// MIDDLE is shared by both press-index extensions (paired on both),
	// so the cross-mode fan-out leaves it alone — same-bin-type, position
	// stays via implicit no-op or index motion at FRONT.
	for _, d := range out {
		if d.CoreNodeName == "MIDDLE" {
			t.Error("MIDDLE: cross-mode fan-out must NOT synthesize a diff for a position both sides reference")
		}
	}
}

// Direction C reverse: 3-pos to 2-pos same bin type. BACK is on
// from-claim's extension only; cross-mode fan-out emits Drop.
func TestFanOutCrossMode_PressIndex3PosToPressIndex2Pos_SynthesizesDropForRetiredPosition(t *testing.T) {
	t.Parallel()
	from := crossModeFromPressIndex("PART-A", "FRONT", "MIDDLE", "BACK")
	to := crossModeFromPressIndex("PART-B", "FRONT", "MIDDLE", "")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	out := FanOutPressIndexCrossMode(diffs)

	if len(out) != 2 {
		t.Fatalf("expected 2 diffs (front + back drop), got %d", len(out))
	}
	var backDiff *ChangeoverNodeDiff
	for i := range out {
		if out[i].CoreNodeName == "BACK" {
			backDiff = &out[i]
		}
	}
	if backDiff == nil {
		t.Fatal("expected synthesized BACK drop")
	}
	if backDiff.Situation != SituationDrop {
		t.Errorf("BACK situation = %q, want SituationDrop", backDiff.Situation)
	}
}

// Regression: same-mode same-bin-type same-position-count must NOT
// trigger the cross-mode fan-out. Both sides claim FRONT/MIDDLE/BACK;
// everything matches — no synthesized diffs.
func TestFanOutCrossMode_NotCrossMode_NoExtraDiffs(t *testing.T) {
	t.Parallel()
	from := crossModeFromPressIndex("PART-A", "FRONT", "MIDDLE", "BACK")
	to := crossModeFromPressIndex("PART-A", "FRONT", "MIDDLE", "BACK") // same payload
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationUnchanged, FromClaim: &from, ToClaim: &to},
	}
	out := FanOutPressIndexCrossMode(diffs)

	if len(out) != 1 {
		t.Errorf("expected diffs unchanged, got %d (cross-mode fan-out must skip when both sides reference each position)", len(out))
	}
}

// Same-mode different-bin-type case: the per-position fan-out has
// already expanded the front diff into per-position diffs. The cross-
// mode fan-out then sees those positions in the covered set and adds
// nothing — no double-fan-out.
func TestFanOutCrossMode_AfterSameModeFanOut_NoOverlap(t *testing.T) {
	t.Parallel()
	from := crossModeFromPressIndex("PART-A", "FRONT", "MIDDLE", "BACK")
	to := crossModeFromPressIndex("PART-B", "FRONT", "MIDDLE", "BACK")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	binTypes := map[string]string{"PART-A": "T1", "PART-B": "T2"}
	// Same-mode fan-out expands into 3 per-position diffs.
	diffs = FanOutPressIndexDifferentBinType(diffs, binTypes)
	if len(diffs) != 3 {
		t.Fatalf("same-mode fan-out setup: expected 3 per-position diffs, got %d", len(diffs))
	}
	// Cross-mode fan-out then runs and should add nothing.
	out := FanOutPressIndexCrossMode(diffs)
	if len(out) != 3 {
		t.Errorf("expected cross-mode fan-out to add nothing on top of same-mode fan-out, got %d diffs", len(out))
	}
}

// Cross-mode where the press-index claim's CoreNodeName is itself
// dropped (to-claim doesn't have that name): DiffStyleClaims would have
// emitted a Drop for CoreNodeName. The cross-mode fan-out still
// expands the extension-only positions.
func TestFanOutCrossMode_PressIndexFullDrop_FansOutExtensions(t *testing.T) {
	t.Parallel()
	from := crossModeFromPressIndex("PART-A", "FRONT", "MIDDLE", "BACK")
	// Diff list represents "front gets dropped" with no to-claim.
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationDrop, FromClaim: &from, ToClaim: nil},
	}
	out := FanOutPressIndexCrossMode(diffs)
	if len(out) != 3 {
		t.Fatalf("expected 3 diffs (front drop + middle drop + back drop), got %d", len(out))
	}
	dropCount := 0
	for _, d := range out {
		if d.Situation == SituationDrop {
			dropCount++
		}
	}
	if dropCount != 3 {
		t.Errorf("expected all 3 diffs to be Drops, got %d", dropCount)
	}
}

// Synthesized claim's ID matches parent so wiring lookups still resolve
// to the persisted parent claim (same property the same-mode fan-out
// ensures).
func TestFanOutCrossMode_SynthesizedClaimRetainsParentID(t *testing.T) {
	t.Parallel()
	from := crossModeFromPressIndex("PART-A", "FRONT", "MIDDLE", "BACK")
	from.ID = 91
	to := crossModeNonPressIndex("FRONT", "PART-B", "single_robot")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	out := FanOutPressIndexCrossMode(diffs)
	for _, d := range out {
		if d.CoreNodeName == "FRONT" {
			continue // unchanged
		}
		if d.FromClaim == nil || d.FromClaim.ID != 91 {
			t.Errorf("%s: synthesized FromClaim.ID = %d, want parent's 91", d.CoreNodeName,
				func() int64 {
					if d.FromClaim == nil {
						return 0
					}
					return d.FromClaim.ID
				}())
		}
	}
}
