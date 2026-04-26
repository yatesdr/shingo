package engine

import (
	"testing"

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
