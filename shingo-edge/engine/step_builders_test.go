package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store"
)

// countWaits counts the number of "wait" actions in a step list.
func countWaits(steps []protocol.ComplexOrderStep) int {
	n := 0
	for _, s := range steps {
		if s.Action == "wait" {
			n++
		}
	}
	return n
}

// stepActions returns the action sequence (e.g. ["pickup","dropoff","wait"]).
func stepActions(steps []protocol.ComplexOrderStep) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Action
	}
	return out
}

// ---------------------------------------------------------------------------
// BuildSwapChangeoverSteps — 1 wait, correct node routing
// ---------------------------------------------------------------------------

func TestBuildSwapChangeoverSteps(t *testing.T) {
	from := &store.StyleNodeClaim{
		CoreNodeName:        "CORE-A",
		OutboundStaging:     "OUT-STAGE",
		OutboundDestination: "DEST-FINAL",
	}
	to := &store.StyleNodeClaim{
		CoreNodeName:   "CORE-B",
		InboundStaging: "IN-STAGE",
	}

	steps := BuildSwapChangeoverSteps(from, to)

	if len(steps) != 8 {
		t.Fatalf("expected 8 steps, got %d", len(steps))
	}
	if w := countWaits(steps); w != 1 {
		t.Errorf("expected 1 wait, got %d", w)
	}

	want := []protocol.ComplexOrderStep{
		{Action: "dropoff", Node: "CORE-A"},
		{Action: "wait"},
		{Action: "pickup", Node: "CORE-A"},
		{Action: "dropoff", Node: "OUT-STAGE"},
		{Action: "pickup", Node: "IN-STAGE"},
		{Action: "dropoff", Node: "CORE-B"},
		{Action: "pickup", Node: "OUT-STAGE"},
		{Action: "dropoff", Node: "DEST-FINAL"},
	}
	for i, s := range steps {
		if s != want[i] {
			t.Errorf("step %d: got %+v, want %+v", i, s, want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// BuildEvacuateChangeoverSteps — 2 waits, correct node routing
// ---------------------------------------------------------------------------

func TestBuildEvacuateChangeoverSteps(t *testing.T) {
	from := &store.StyleNodeClaim{
		CoreNodeName:        "CORE-A",
		OutboundStaging:     "OUT-STAGE",
		OutboundDestination: "DEST-FINAL",
	}
	to := &store.StyleNodeClaim{
		CoreNodeName:   "CORE-B",
		InboundStaging: "IN-STAGE",
	}

	steps := BuildEvacuateChangeoverSteps(from, to)

	if len(steps) != 9 {
		t.Fatalf("expected 9 steps, got %d", len(steps))
	}
	if w := countWaits(steps); w != 2 {
		t.Errorf("expected 2 waits, got %d", w)
	}

	// Verify the two wait positions: index 1 ("ready") and index 4 ("tooling done")
	if steps[1].Action != "wait" {
		t.Errorf("step 1: expected wait, got %q", steps[1].Action)
	}
	if steps[4].Action != "wait" {
		t.Errorf("step 4: expected wait (tooling done), got %q", steps[4].Action)
	}

	actions := stepActions(steps)
	wantActions := []string{"dropoff", "wait", "pickup", "dropoff", "wait", "pickup", "dropoff", "pickup", "dropoff"}
	for i, a := range actions {
		if a != wantActions[i] {
			t.Errorf("step %d action: got %q, want %q", i, a, wantActions[i])
		}
	}
}

// ---------------------------------------------------------------------------
// BuildKeepStagedDeliverSteps — 1 wait, stage→deliver sequence
// ---------------------------------------------------------------------------

func TestBuildKeepStagedDeliverSteps(t *testing.T) {
	to := &store.StyleNodeClaim{
		CoreNodeName:   "CORE-NODE",
		InboundSource:  "SOURCE",
		InboundStaging: "IN-STAGE",
	}

	steps := BuildKeepStagedDeliverSteps(to)

	if len(steps) != 5 {
		t.Fatalf("expected 5 steps, got %d", len(steps))
	}
	if w := countWaits(steps); w != 1 {
		t.Errorf("expected 1 wait, got %d", w)
	}

	want := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: "SOURCE"},
		{Action: "dropoff", Node: "IN-STAGE"},
		{Action: "wait"},
		{Action: "pickup", Node: "IN-STAGE"},
		{Action: "dropoff", Node: "CORE-NODE"},
	}
	for i, s := range steps {
		if s != want[i] {
			t.Errorf("step %d: got %+v, want %+v", i, s, want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// BuildKeepStagedEvacSteps — 1 wait, pre-position→evacuate→final
// ---------------------------------------------------------------------------

func TestBuildKeepStagedEvacSteps(t *testing.T) {
	from := &store.StyleNodeClaim{
		CoreNodeName:        "CORE-NODE",
		OutboundDestination: "DEST-FINAL",
	}

	steps := BuildKeepStagedEvacSteps(from)

	if len(steps) != 4 {
		t.Fatalf("expected 4 steps, got %d", len(steps))
	}
	if w := countWaits(steps); w != 1 {
		t.Errorf("expected 1 wait, got %d", w)
	}

	want := []protocol.ComplexOrderStep{
		{Action: "dropoff", Node: "CORE-NODE"},
		{Action: "wait"},
		{Action: "pickup", Node: "CORE-NODE"},
		{Action: "dropoff", Node: "DEST-FINAL"},
	}
	for i, s := range steps {
		if s != want[i] {
			t.Errorf("step %d: got %+v, want %+v", i, s, want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// BuildKeepStagedCombinedSteps — 1 wait, clear-then-stage sequence
// ---------------------------------------------------------------------------

func TestBuildKeepStagedCombinedSteps(t *testing.T) {
	from := &store.StyleNodeClaim{
		InboundSource: "FROM-SOURCE",
	}
	to := &store.StyleNodeClaim{
		CoreNodeName:   "CORE-NODE",
		InboundSource:  "TO-SOURCE",
		InboundStaging: "IN-STAGE",
	}

	steps := BuildKeepStagedCombinedSteps(from, to)

	if len(steps) != 7 {
		t.Fatalf("expected 7 steps, got %d", len(steps))
	}
	if w := countWaits(steps); w != 1 {
		t.Errorf("expected 1 wait, got %d", w)
	}

	// Verify the clear-then-stage sequence:
	// step 0: pickup from InboundStaging (grab old staged bin)
	// step 1: dropoff to fromClaim.InboundSource (return to market)
	// step 2: pickup from toClaim.InboundSource (grab new material)
	// step 3: dropoff to InboundStaging (stage new)
	if steps[0].Action != "pickup" || steps[0].Node != "IN-STAGE" {
		t.Errorf("step 0: expected pickup IN-STAGE, got %+v", steps[0])
	}
	if steps[1].Action != "dropoff" || steps[1].Node != "FROM-SOURCE" {
		t.Errorf("step 1: expected dropoff FROM-SOURCE (return old), got %+v", steps[1])
	}
	if steps[2].Action != "pickup" || steps[2].Node != "TO-SOURCE" {
		t.Errorf("step 2: expected pickup TO-SOURCE (grab new), got %+v", steps[2])
	}
	if steps[3].Action != "dropoff" || steps[3].Node != "IN-STAGE" {
		t.Errorf("step 3: expected dropoff IN-STAGE (stage new), got %+v", steps[3])
	}
	if steps[4].Action != "wait" {
		t.Errorf("step 4: expected wait, got %q", steps[4].Action)
	}

	want := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: "IN-STAGE"},
		{Action: "dropoff", Node: "FROM-SOURCE"},
		{Action: "pickup", Node: "TO-SOURCE"},
		{Action: "dropoff", Node: "IN-STAGE"},
		{Action: "wait"},
		{Action: "pickup", Node: "IN-STAGE"},
		{Action: "dropoff", Node: "CORE-NODE"},
	}
	for i, s := range steps {
		if s != want[i] {
			t.Errorf("step %d: got %+v, want %+v", i, s, want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// BuildStageSteps — source → staging route
// ---------------------------------------------------------------------------

func TestBuildStageSteps(t *testing.T) {
	claim := &store.StyleNodeClaim{
		InboundSource:  "MARKET",
		InboundStaging: "STAGING-AREA",
	}
	steps := BuildStageSteps(claim)

	if steps == nil {
		t.Fatal("expected steps, got nil")
	}
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	if steps[0] != (protocol.ComplexOrderStep{Action: "pickup", Node: "MARKET"}) {
		t.Errorf("step 0: got %+v", steps[0])
	}
	if steps[1] != (protocol.ComplexOrderStep{Action: "dropoff", Node: "STAGING-AREA"}) {
		t.Errorf("step 1: got %+v", steps[1])
	}
}

func TestBuildStageSteps_NoInboundStaging(t *testing.T) {
	claim := &store.StyleNodeClaim{
		InboundSource: "MARKET",
		// InboundStaging empty — cannot pre-stage
	}
	steps := BuildStageSteps(claim)
	if steps != nil {
		t.Errorf("expected nil when InboundStaging is empty, got %d steps", len(steps))
	}
}

// ---------------------------------------------------------------------------
// BuildReleaseSteps — outbound routing (core → destination)
// ---------------------------------------------------------------------------

func TestBuildReleaseSteps(t *testing.T) {
	claim := &store.StyleNodeClaim{
		CoreNodeName:        "CORE-NODE",
		OutboundDestination: "DEST",
	}
	steps := BuildReleaseSteps(claim)

	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	if steps[0] != (protocol.ComplexOrderStep{Action: "pickup", Node: "CORE-NODE"}) {
		t.Errorf("step 0: got %+v", steps[0])
	}
	if steps[1] != (protocol.ComplexOrderStep{Action: "dropoff", Node: "DEST"}) {
		t.Errorf("step 1: got %+v", steps[1])
	}
}

// Edge case: missing OutboundDestination → dropoff step has no Node.
// Core uses payload-based routing (global fallback).
func TestBuildReleaseSteps_MissingDestination(t *testing.T) {
	claim := &store.StyleNodeClaim{
		CoreNodeName: "CORE-NODE",
		// OutboundDestination empty
	}
	steps := BuildReleaseSteps(claim)

	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	// Step 0: pickup from core node (always present)
	if steps[0].Action != "pickup" || steps[0].Node != "CORE-NODE" {
		t.Errorf("step 0: got %+v, want pickup CORE-NODE", steps[0])
	}
	// Step 1: dropoff with no node — Core resolves via payloadCode
	if steps[1].Action != "dropoff" {
		t.Errorf("step 1 action: got %q, want dropoff", steps[1].Action)
	}
	if steps[1].Node != "" {
		t.Errorf("step 1 node: got %q, want empty string (payload-based fallback)", steps[1].Node)
	}
}

// ---------------------------------------------------------------------------
// BuildRestoreSteps — outbound staging → core route
// ---------------------------------------------------------------------------

func TestBuildRestoreSteps(t *testing.T) {
	claim := &store.StyleNodeClaim{
		CoreNodeName:    "CORE-NODE",
		OutboundStaging: "OUT-STAGE",
	}
	steps := BuildRestoreSteps(claim)

	if steps == nil {
		t.Fatal("expected steps, got nil")
	}
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	if steps[0] != (protocol.ComplexOrderStep{Action: "pickup", Node: "OUT-STAGE"}) {
		t.Errorf("step 0: got %+v", steps[0])
	}
	if steps[1] != (protocol.ComplexOrderStep{Action: "dropoff", Node: "CORE-NODE"}) {
		t.Errorf("step 1: got %+v", steps[1])
	}
}

func TestBuildRestoreSteps_NoOutboundStaging(t *testing.T) {
	claim := &store.StyleNodeClaim{
		CoreNodeName: "CORE-NODE",
		// OutboundStaging empty — nothing to restore
	}
	steps := BuildRestoreSteps(claim)
	if steps != nil {
		t.Errorf("expected nil when OutboundStaging is empty, got %d steps", len(steps))
	}
}

// ---------------------------------------------------------------------------
// Edge case: missing InboundSource in buildStep callers
// ---------------------------------------------------------------------------

// BuildStageSteps with empty InboundSource: pickup step has no Node.
// Core resolves the source via payloadCode.
func TestBuildStageSteps_MissingInboundSource(t *testing.T) {
	claim := &store.StyleNodeClaim{
		// InboundSource empty
		InboundStaging: "STAGING-AREA",
	}
	steps := BuildStageSteps(claim)

	if steps == nil {
		t.Fatal("expected steps (InboundStaging is set), got nil")
	}
	if steps[0].Action != "pickup" {
		t.Errorf("step 0 action: got %q, want pickup", steps[0].Action)
	}
	if steps[0].Node != "" {
		t.Errorf("step 0 node: got %q, want empty string (payload-based fallback)", steps[0].Node)
	}
}

// BuildKeepStagedDeliverSteps with empty InboundSource: first pickup has no Node.
func TestBuildKeepStagedDeliverSteps_MissingInboundSource(t *testing.T) {
	to := &store.StyleNodeClaim{
		CoreNodeName:   "CORE-NODE",
		InboundStaging: "IN-STAGE",
		// InboundSource empty
	}
	steps := BuildKeepStagedDeliverSteps(to)

	if steps[0].Action != "pickup" || steps[0].Node != "" {
		t.Errorf("step 0: expected pickup with empty node (fallback), got %+v", steps[0])
	}
}
