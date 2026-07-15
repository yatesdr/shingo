package dispatch

import (
	"errors"
	"fmt"
	"testing"

	"shingocore/dispatch/binresolver"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// Unit tests for resolvePerBinDestinations — the bin flow simulation that
// computes per-bin final destinations from a step sequence. This function
// is the core risk surface of the order_bins junction table feature.

func TestResolvePerBinDestinations_SinglePickupDropoff(t *testing.T) {
	t.Parallel()
	// Simplest case: one bin, pickup then dropoff.
	steps := []resolvedStep{
		{Action: "pickup", Node: "storage.A1"},
		{Action: "dropoff", Node: "line.L1"},
	}
	claimed := map[string]int64{"storage.A1": 100}

	dest := resolvePerBinDestinations(steps, claimed)

	if dest[100] != "line.L1" {
		t.Errorf("bin 100 dest = %q, want %q", dest[100], "line.L1")
	}
}

func TestResolvePerBinDestinations_SwapPattern(t *testing.T) {
	t.Parallel()
	// Full 9-step swap: two bins, multiple pickups and dropoffs.
	// This is the TC-60 production pattern.
	//
	// newBin (101): storage → inStaging → (wait) → inStaging → line (final)
	// oldBin (102): line → outStaging → (wait) → outStaging → outDest (final)
	steps := []resolvedStep{
		{Action: "pickup", Node: "storage"},     // 1: pick newBin
		{Action: "dropoff", Node: "inStaging"},  // 2: stage newBin
		{Action: "wait", Node: "line"},          // 3: drive to node + hold (BinTask=Wait)
		{Action: "pickup", Node: "line"},        // 4: pick oldBin
		{Action: "dropoff", Node: "outStaging"}, // 5: park oldBin
		{Action: "pickup", Node: "inStaging"},   // 6: re-pick newBin
		{Action: "dropoff", Node: "line"},       // 7: deliver newBin (FINAL for newBin)
		{Action: "pickup", Node: "outStaging"},  // 8: re-pick oldBin
		{Action: "dropoff", Node: "outDest"},    // 9: deliver oldBin (FINAL for oldBin)
	}
	claimed := map[string]int64{
		"storage": 101, // newBin claimed at step 1
		"line":    102, // oldBin claimed at step 4
	}

	dest := resolvePerBinDestinations(steps, claimed)

	if dest[101] != "line" {
		t.Errorf("newBin (101) dest = %q, want %q", dest[101], "line")
	}
	if dest[102] != "outDest" {
		t.Errorf("oldBin (102) dest = %q, want %q", dest[102], "outDest")
	}
}

func TestResolvePerBinDestinations_ReStaging(t *testing.T) {
	t.Parallel()
	// Bin is picked up, dropped at staging, picked up again, dropped at final dest.
	// The dest should update to the final dropoff, not the intermediate staging.
	steps := []resolvedStep{
		{Action: "pickup", Node: "storage.A1"},
		{Action: "dropoff", Node: "staging.S1"}, // intermediate
		{Action: "pickup", Node: "staging.S1"},  // re-pick
		{Action: "dropoff", Node: "line.L1"},    // final
	}
	claimed := map[string]int64{"storage.A1": 200}

	dest := resolvePerBinDestinations(steps, claimed)

	if dest[200] != "line.L1" {
		t.Errorf("bin 200 dest = %q, want %q (should be final dropoff, not staging)", dest[200], "line.L1")
	}
}

func TestResolvePerBinDestinations_EmptyDropoff(t *testing.T) {
	t.Parallel()
	// Robot drops bin at staging, then drives empty to another node (dropoff with no bin).
	// The empty dropoff should not create a destination entry for a non-existent bin.
	steps := []resolvedStep{
		{Action: "pickup", Node: "storage.A1"},
		{Action: "dropoff", Node: "staging.S1"},
		{Action: "dropoff", Node: "line.L1"}, // robot is empty — no-op for bin tracking
	}
	claimed := map[string]int64{"storage.A1": 300}

	dest := resolvePerBinDestinations(steps, claimed)

	if dest[300] != "staging.S1" {
		t.Errorf("bin 300 dest = %q, want %q (empty dropoff should not change dest)", dest[300], "staging.S1")
	}
}

func TestResolvePerBinDestinations_GhostPickup(t *testing.T) {
	t.Parallel()
	// Pickup at a node with no bin (ghost robot dispatch). The robot arrives
	// but there's nothing to pick up. Algorithm handles this via the binAtNode
	// lookup — no entry means carrying stays 0.
	steps := []resolvedStep{
		{Action: "pickup", Node: "empty.node"}, // nothing here
		{Action: "dropoff", Node: "line.L1"},   // robot empty, no-op
		{Action: "pickup", Node: "storage.A1"}, // actual bin
		{Action: "dropoff", Node: "line.L2"},
	}
	claimed := map[string]int64{"storage.A1": 400}
	// Note: no bin claimed at "empty.node" — ghost pickup

	dest := resolvePerBinDestinations(steps, claimed)

	if dest[400] != "line.L2" {
		t.Errorf("bin 400 dest = %q, want %q", dest[400], "line.L2")
	}
	// The ghost node should produce no destination entry
	if len(dest) != 1 {
		t.Errorf("expected 1 destination entry, got %d", len(dest))
	}
}

// makeBuriedError synthesizes a *BuriedError for the classifier tests
// below. Tests do not exercise the resolver itself — they only need a
// typed error that wraps the ErrBuried sentinel.
func makeBuriedError(binID, laneID int64, slotName string) *binresolver.BuriedError {
	return &binresolver.BuriedError{
		Bin:    &bins.Bin{ID: binID},
		Slot:   &nodes.Node{ID: 7, Name: slotName},
		LaneID: laneID,
	}
}

// TestClassifyResolutionError_AllShapes is a table-driven exercise of
// every error shape in §3's resolver-error taxonomy. Each shape must
// map to the right ResolutionErrorClass — the load-bearing contract
// that lets both complex-intake and simple-retrieve route through a
// single classifier.
func TestClassifyResolutionError_AllShapes(t *testing.T) {
	t.Parallel()
	be := makeBuriedError(6, 27, "Cell_9")
	se := &binresolver.StructuralError{
		Group:   "ASRS_Lane_Test",
		Payload: "P1",
		Reason:  "group has no enabled child nodes",
	}

	cases := []struct {
		name      string
		err       error
		wantClass ResolutionErrorClass
		wantTyped bool // does this class carry a typed payload?
	}{
		{"nil", nil, ResolutionOK, false},
		{"buried direct", be, ResolutionBuried, true},
		{"buried wrapped", fmt.Errorf("step 0: %w", fmt.Errorf("cannot resolve group X: %w", be)), ResolutionBuried, true},
		{"structural", se, ResolutionStructural, true},
		{"structural wrapped", fmt.Errorf("step 1: %w", se), ResolutionStructural, true},
		{"capacity slot", errors.New("no available slot in node group ASRS"), ResolutionCapacity, false},
		{"capacity payload", errors.New("no bin of requested payload in node group ASRS"), ResolutionCapacity, false},
		{"capacity wrapped", fmt.Errorf("step 0: cannot resolve group ASRS: %w", errors.New("no available slot in node group ASRS")), ResolutionCapacity, false},
		// Produce-side empty-fetch duals: a dry empty pool is sourceable-eventually
		// (an empty returns), so it must QUEUE like the full pair, not terminal-reject.
		// The first wraps the resolver's sql.ErrNoRows (resolveStepNode's step.Empty
		// branch); the second is the clean nil-bin shape.
		{"capacity empty dry pool", fmt.Errorf("cannot resolve empty in group %s: %w", "SYN_MARKET", errors.New("sql: no rows in result set")), ResolutionCapacity, false},
		{"capacity empty none", errors.New("no empty carrier in group SYN_MARKET"), ResolutionCapacity, false},
		{"capacity empty wrapped", fmt.Errorf("step 3: %w", fmt.Errorf("cannot resolve empty in group %s: %w", "SYN_MARKET", errors.New("sql: no rows in result set"))), ResolutionCapacity, false},
		{"transient list children", errors.New("list children of NGRP: connection refused"), ResolutionTransient, false},
		{"transient get depth", errors.New("get target depth: connection refused"), ResolutionTransient, false},
		{"fatal unknown", errors.New("something else entirely"), ResolutionFatal, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			class, payload := classifyResolutionError(c.err)
			if class != c.wantClass {
				t.Errorf("class = %v, want %v (err=%v)", class, c.wantClass, c.err)
			}
			if c.wantTyped && payload == nil {
				t.Errorf("class %v must carry typed payload; got nil", class)
			}
			if !c.wantTyped && payload != nil {
				t.Errorf("class %v should have nil payload; got %T", class, payload)
			}
		})
	}
}

// TestClassifyResolutionError_ChainWalking replaces the v5-era
// TestErrorsIs_ErrBuried_ThroughWrapping. Locks in that the
// classifier walks any number of fmt.Errorf wraps (resolveComplexSteps
// adds "step N:" and resolveStepNode adds "cannot resolve group X:").
// The buried payload extracted from the wrapped error must carry the
// original bin/lane fields.
func TestClassifyResolutionError_ChainWalking(t *testing.T) {
	t.Parallel()
	be := makeBuriedError(6, 27, "Cell_9")
	wrapped := fmt.Errorf("step 0: %w", fmt.Errorf("cannot resolve group ASRS_Lane_Test: %w", be))

	class, payload := classifyResolutionError(wrapped)
	if class != ResolutionBuried {
		t.Fatalf("class = %v, want ResolutionBuried", class)
	}
	got, ok := payload.(*binresolver.BuriedError)
	if !ok {
		t.Fatalf("payload type = %T, want *BuriedError", payload)
	}
	if got.Bin.ID != be.Bin.ID || got.LaneID != be.LaneID {
		t.Errorf("extracted BuriedError mismatch: got bin=%d lane=%d, want bin=%d lane=%d",
			got.Bin.ID, got.LaneID, be.Bin.ID, be.LaneID)
	}
}

// TestEmptyBinsOnly pins the empty-carrier filter used by ApplyComplexPlan on a
// produce node's empty pickup leg. BinUnavailableReason accepts both an empty
// and a payload-matching full, so the claim path must pre-filter to empties —
// otherwise a full of the part could be claimed and delivered to the press.
func TestEmptyBinsOnly(t *testing.T) {
	t.Parallel()
	full := &bins.Bin{ID: 1, Label: "FULL", PayloadCode: "PART-A"}
	empty1 := &bins.Bin{ID: 2, Label: "EMPTY-1", PayloadCode: ""}
	empty2 := &bins.Bin{ID: 3, Label: "EMPTY-2", PayloadCode: ""}

	got := emptyBinsOnly([]*bins.Bin{full, empty1, full, empty2})
	if len(got) != 2 {
		t.Fatalf("emptyBinsOnly returned %d bins, want 2 (the empties)", len(got))
	}
	for _, b := range got {
		if b.PayloadCode != "" {
			t.Errorf("emptyBinsOnly returned a payload-bearing bin %d (%s)", b.ID, b.Label)
		}
	}

	if n := len(emptyBinsOnly([]*bins.Bin{full})); n != 0 {
		t.Errorf("emptyBinsOnly([full]) returned %d, want 0", n)
	}
	if n := len(emptyBinsOnly(nil)); n != 0 {
		t.Errorf("emptyBinsOnly(nil) returned %d, want 0", n)
	}
}
