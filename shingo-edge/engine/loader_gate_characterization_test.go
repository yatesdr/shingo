package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// THE NET UNDER THE LOADER CLUSTER.
//
// These pin the behaviour of the "is this a loader?" question at the operator
// entry points, so that moving the question off the swap-mode field is
// checkable rather than hopeful. They assert BEHAVIOUR — who is refused, with
// what message — not which field the code reads to decide, which is the thing
// being changed.
//
// THEY MUST PASS UNCHANGED BEFORE AND AFTER THE MOVE. A test that has to be
// edited to make the move pass is reporting a behaviour change wearing a
// refactor's clothes, and it stops for a ruling rather than being adjusted.
//
// WHY THESE SITES. Four operator entry points carry a byte-identical
// precondition — loadActiveNode, then a nil-claim check, then a mode check that
// refuses with the same sentence. That duplication is the one place in the
// cluster with a real collapse in it, so it is the place that most needs a net
// under it, and it had NO test coverage at all: no test in the tree asserted
// the refusal string, which means nothing would have noticed if a collapse
// dropped a guard or changed what an operator reads on the HMI.
//
// NOT COVERED HERE, DELIBERATELY:
//
//   - loaderCardNode (operator_supply_refusal.go) asks the same question but
//     answers it with a different, richer sentence that names the offending
//     mode, and it requires an operator station on the node before it gets
//     that far. It keeps its own check and its own message.
//   - Loader.SynthClaim's stamp is already pinned, twice, by
//     domain/loader_synthclaim_test.go and engine/loader_synthclaim_test.go.
//   - guardStyleTransition and guardCatidMismatch already have loader-exemption
//     coverage in guard_style_transition_test.go and guard_catid_mismatch_test.go.
//     guardPositionSpokenFor did not, and it is added below.

// notALoaderMessage is the operator-facing refusal the four entry points share
// today, verbatim. It is asserted rather than paraphrased because it is what a
// human reads on the HMI when a tap is refused: a refactor may move which field
// decides, but silently rewording an operator's error is a product change.
const notALoaderMessage = "is not a manual_swap node"

// seedClaimWithSwapMode is seedManualSwapClaim's general form: same fixture, any
// configurable mode, so a test can stand a NON-loader node next to a loader one
// and compare. Kept local to this file rather than widening the shared helper.
func seedClaimWithSwapMode(
	t *testing.T, db *store.DB, prefix string, role protocol.ClaimRole,
	payloadCode, outbound string, mode protocol.SwapMode,
) (nodeID int64) {
	t.Helper()
	processID, err := db.CreateProcess(prefix+"-PROC", prefix+" cell", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: prefix + "-NODE",
		Code:         prefix[:3],
		Name:         prefix + " cell",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err := db.CreateStyle(prefix+"-STYLE", prefix+" style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	db.SetActiveStyle(processID, &styleID)

	if _, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        prefix + "-NODE",
		Role:                role,
		SwapMode:            mode,
		PayloadCode:         payloadCode,
		UOPCapacity:         100,
		OutboundDestination: outbound,
		InboundStaging:      prefix + "-STAGE",
	}); err != nil {
		t.Fatalf("upsert %s claim: %v", mode, err)
	}
	db.EnsureProcessNodeRuntime(nodeID)
	return nodeID
}

// loaderOnlyOps are the operator entry points whose first substantive act is to
// refuse a node that is not a loader. Each is reduced to (nodeID) -> error so
// the table can drive them together; a nil error is as much a failure as the
// wrong error, because it means the gate did not fire.
func loaderOnlyOps(e *Engine) []struct {
	name string
	call func(nodeID int64) error
} {
	return []struct {
		name string
		call func(nodeID int64) error
	}{
		{"LoadBin", func(id int64) error { return e.LoadBin(id, "PART-X", nil, nil) }},
		{"ClearBin", func(id int64) error { return e.ClearBin(id, "") }},
		{"PushEmptyOut", func(id int64) error { return e.PushEmptyOut(id) }},
		{"RequestFullBin", func(id int64) error { _, err := e.RequestFullBin(id, "PART-X"); return err }},
	}
}

// TestLoaderOnlyOperations_RefuseNonLoaderNodes is the negative half: a node
// that is not a loader must be refused by every loader-only operation, and
// refused with the sentence the operator already knows.
//
// MUTATION: delete any one of the four mode checks — that operation starts
// accepting a line cell, which is how a forklift LOAD lands on a robot-served
// press. This names the operation.
func TestLoaderOnlyOperations_RefuseNonLoaderNodes(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	// single_robot: a robot-served line cell, the ordinary case a loader-only
	// operation must turn away.
	lineNode := seedClaimWithSwapMode(t, db, "LINE-REFUSE", protocol.ClaimRoleConsume,
		"PART-X", "STORAGE-NODE", protocol.SwapModeSingleRobot)

	for _, op := range loaderOnlyOps(eng) {
		err := op.call(lineNode)
		if err == nil {
			t.Errorf("%s on a single_robot node returned nil — the loader gate did not fire, so a "+
				"line cell can be driven by an operation that only a forklift window should accept", op.name)
			continue
		}
		if !strings.Contains(err.Error(), notALoaderMessage) {
			t.Errorf("%s on a single_robot node: error = %q, want it to contain %q. The gate fired but "+
				"the operator-facing wording changed; that is a product change, not a refactor",
				op.name, err.Error(), notALoaderMessage)
		}
	}
}

// TestLoaderOnlyOperations_AdmitLoaderNodes is the positive half, and it is the
// one that stops the net from being satisfiable by refusing everything.
//
// It asserts only that the LOADER GATE does not fire — not that the call
// succeeds. Past the gate these operations reach Core, bind carriers and write
// orders, and pinning all of that here would be pinning other people's
// behaviour. What matters for the move is that a genuine loader is never
// mistaken for a line cell. The engine is pointed at an unreachable Core so
// each call fails AFTER the gate, for a reason that is visibly not the gate.
func TestLoaderOnlyOperations_AdmitLoaderNodes(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	unreachableCore(t, eng)

	loaderNode, _ := seedManualSwapClaim(t, db, "LOADER-ADMIT", protocol.ClaimRoleProduce,
		"PART-X", "STORAGE-NODE")

	for _, op := range loaderOnlyOps(eng) {
		err := op.call(loaderNode)
		if err != nil && strings.Contains(err.Error(), notALoaderMessage) {
			t.Errorf("%s on a manual_swap loader was refused as %q — the gate fired on the very "+
				"population it exists to admit, which is a dead button on the loader board",
				op.name, err.Error())
		}
	}
}

// TestLoaderOnlyOperations_RefuseNodesWithNoClaim pins the OTHER half of the
// shared precondition. The four entry points check a nil claim before they
// check the mode, and a collapse that folds both checks into one resolver has
// to keep refusing an unclaimed node — otherwise the mode check runs against a
// nil pointer.
func TestLoaderOnlyOperations_RefuseNodesWithNoClaim(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	processID, err := db.CreateProcess("NOCLAIM-PROC", "no claim", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	bareNode, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "NOCLAIM-NODE",
		Code:         "NOC",
		Name:         "no claim",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	db.EnsureProcessNodeRuntime(bareNode)

	for _, op := range loaderOnlyOps(eng) {
		if err := op.call(bareNode); err == nil {
			t.Errorf("%s on a node with no active claim returned nil — a claimless node has no "+
				"loader identity to check, so admitting it dereferences nothing and decides anyway", op.name)
		}
	}
}

// TestGuardPositionSpokenFor_ExemptsLoaders nets the third line-cell guard's
// loader exemption. The other two already have one — guardStyleTransition in
// guard_style_transition_test.go, guardCatidMismatch in
// guard_catid_mismatch_test.go — and all three carry the same exemption for the
// same reason: a loader window runs a multi-order queue on purpose, so the
// serial-occupancy rules that protect a line cell would read its normal state
// as a fault and kill the operator's request button.
//
// MUTATION: delete the exemption in guardPositionSpokenFor — a loader with a
// live order in its runtime slot starts refusing the operator's next tap, which
// is the Springfield regression shape.
func TestGuardPositionSpokenFor_ExemptsLoaders(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	// In-memory claim: the guard reaches its loader exemption before it touches
	// the database, which is itself part of what is being pinned — the exemption
	// is a property of the claim, decidable without a round trip.
	node := &processes.Node{ID: 1, ProcessID: 1, CoreNodeName: "LOADER-GPSF", Name: "loader gpsf"}
	runtime := &processes.RuntimeState{}
	loaderClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeManualSwap}

	if err := eng.guardPositionSpokenFor(node, runtime, loaderClaim); err != nil {
		t.Errorf("guardPositionSpokenFor on a loader claim = %v, want nil — a loader's multi-order "+
			"queue is its normal state, not a position already spoken for", err)
	}
}
