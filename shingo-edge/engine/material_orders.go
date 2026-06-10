package engine

import (
	"shingo/protocol"
	"shingoedge/store/processes"
)

// Material movement step builders.
// These are pure functions that return ComplexOrderStep sequences from a
// StyleNodeClaim's routing config. They are used by both routine
// replenishment and changeover order construction.

// buildStep constructs a single ComplexOrderStep. Core auto-detects whether
// the node is a group (NGRP) and resolves it. Empty node triggers global
// fallback via payloadCode.
func buildStep(action, node string) protocol.ComplexOrderStep {
	if node != "" {
		return protocol.ComplexOrderStep{Action: action, Node: node}
	}
	return protocol.ComplexOrderStep{Action: action}
}

// BuildReleaseSteps builds steps to remove material from a node and send it
// to the configured outbound destination.
func BuildReleaseSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "pickup", Node: claim.CoreNodeName},
		buildStep("dropoff", claim.OutboundDestination),
	}
}

// BuildStagedReleaseSteps is BuildReleaseSteps with a leading wait-with-node
// at the source. The robot drives to the lineside, parks, and the order
// reaches status=staged before pickup — giving the operator a chance to
// inspect the partial bin and enter the remaining count at the standard
// release-prompt dialog. After release, the bin is picked up and routed to
// the outbound destination unattended.
//
// Used by the drop planner so a partial bin removed during a changeover gets
// the same release-with-count gate the swap-evac path uses, rather than
// silently disappearing. EvacuateNode (the manual EMPTY FOR TOOL CHANGE path)
// still uses BuildReleaseSteps directly because the operator has already
// confirmed the partial count at the EMPTY click; no second confirmation
// needed.
func BuildStagedReleaseSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "wait", Node: claim.CoreNodeName},
		{Action: "pickup", Node: claim.CoreNodeName},
		buildStep("dropoff", claim.OutboundDestination),
	}
}

// BuildStageSteps builds steps to pre-stage material at the inbound staging
// node in preparation for a swap. Material is fetched and placed at the
// inbound staging node but NOT yet delivered to the production node.
func BuildStageSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	if claim.InboundStaging == "" {
		return nil // no inbound staging configured, cannot pre-stage
	}
	return []protocol.ComplexOrderStep{
		buildStep("pickup", claim.InboundSource),
		{Action: "dropoff", Node: claim.InboundStaging},
	}
}

// BuildStagedDeliverSteps builds steps to move already-staged material from
// the inbound staging node to the production node. Used after staging + evacuation.
func BuildStagedDeliverSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	if claim.InboundStaging == "" {
		return nil
	}
	return []protocol.ComplexOrderStep{
		{Action: "pickup", Node: claim.InboundStaging},
		{Action: "dropoff", Node: claim.CoreNodeName},
	}
}

// BuildSingleSwapSteps builds a 9-step single-robot swap sequence:
//  1. pickup(InboundSource)           — pick new from source
//  2. dropoff(InboundStaging)         — park new at inbound staging
//  3. wait(CoreNodeName)              — drive to node and hold (RDS BinTask=Wait)
//  4. pickup(CoreNodeName)            — pick up old from line
//  5. dropoff(OutboundStaging)        — quick-park old nearby
//  6. pickup(InboundStaging)          — grab new from staging
//  7. dropoff(CoreNodeName)           — deliver new to line
//  8. pickup(OutboundStaging)         — grab old from staging
//  9. dropoff(OutboundDestination)    — deliver old to final destination
func BuildSingleSwapSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	if claim.InboundStaging == "" || claim.OutboundStaging == "" {
		return nil
	}
	return []protocol.ComplexOrderStep{
		buildStep("pickup", claim.InboundSource),         // 1
		{Action: "dropoff", Node: claim.InboundStaging},  // 2
		{Action: "wait", Node: claim.CoreNodeName},       // 3 drive to node + hold
		{Action: "pickup", Node: claim.CoreNodeName},     // 4
		{Action: "dropoff", Node: claim.OutboundStaging}, // 5
		{Action: "pickup", Node: claim.InboundStaging},   // 6
		{Action: "dropoff", Node: claim.CoreNodeName},    // 7
		{Action: "pickup", Node: claim.OutboundStaging},  // 8
		buildStep("dropoff", claim.OutboundDestination),  // 9
	}
}

// BuildTwoRobotSwapSteps builds steps for a two-robot coordinated swap.
// Returns two step lists — one for each robot order:
//
// Order A (resupply robot): pickup new from source → stage → wait → pickup from staging → deliver to node
// Order B (removal robot): wait at node → pickup old from node → deliver to outbound destination
//
// Edge coordinates: releases Order B first (remove old), then releases Order A (deliver new).
func BuildTwoRobotSwapSteps(claim *processes.NodeClaim) (orderA, orderB []protocol.ComplexOrderStep) {
	if claim.InboundStaging == "" {
		return nil, nil
	}
	// Robot A: fetch new material, stage, wait for node clear, then deliver.
	// The wait is wait-with-node at InboundStaging — robot drops the new bin
	// at staging and holds there. wait-with-node produces an RDS Wait block,
	// so RDS reports WAITING and the order reliably transitions to "staged"
	// on Edge. Pre-2026-04-27 this was a bare wait ({Action: "wait"} with no
	// node), which split the order at the dispatcher level and depended on
	// the seerrds adapter correctly reporting WAITING on incremental
	// (complete=false) orders. That path was fragile and Order A would often
	// stay at in_transit while physically parked, breaking swap_ready and
	// requiring two RELEASE clicks. See shingo_todo.md.
	orderA = []protocol.ComplexOrderStep{
		buildStep("pickup", claim.InboundSource),        // pick new from source
		{Action: "dropoff", Node: claim.InboundStaging}, // stage new
		{Action: "wait", Node: claim.InboundStaging},    // hold at staging until line clears
		{Action: "pickup", Node: claim.InboundStaging},  // pick new from staging
		{Action: "dropoff", Node: claim.CoreNodeName},   // deliver to production
	}
	// Robot B: drive to node and hold, wait for release, remove old to destination
	orderB = []protocol.ComplexOrderStep{
		{Action: "wait", Node: claim.CoreNodeName},      // drive to node + hold (RDS BinTask=Wait)
		{Action: "pickup", Node: claim.CoreNodeName},    // remove old from production
		buildStep("dropoff", claim.OutboundDestination), // deliver to destination
	}
	return orderA, orderB
}

// BuildTwoRobotPressIndexSwapSteps builds steps for a press-indexing two-robot
// swap. The press has either two or three positions:
//
//	2-position layout (claim.SecondPairedCoreNode == ""):
//	  A (front/output, CoreNodeName), B (back/input, PairedCoreNode)
//	  R1: wait(A) → pickup(A) → dropoff(OutboundDestination)
//	             → pickup(InboundSource) → dropoff(B)
//	  R2: wait(B) → pickup(B) → dropoff(A)
//
//	3-position layout (claim.SecondPairedCoreNode set, = C):
//	  A (front), B (middle, PairedCoreNode), C (back, SecondPairedCoreNode)
//	  R1: wait(A) → pickup(A) → dropoff(OutboundDestination)
//	             → pickup(InboundSource) → dropoff(C)
//	  R2: wait(B) → pickup(B) → dropoff(A) → pickup(C) → dropoff(B)
//
// Both robots fire on operator release. The fleet manager handles cross-leg
// sequencing on shared nodes (R2's dropoff(A) waits for R1's pickup(A);
// R1's dropoff(C) waits for R2's pickup(C) in the 3-position case).
func BuildTwoRobotPressIndexSwapSteps(claim *processes.NodeClaim) (orderR1, orderR2 []protocol.ComplexOrderStep) {
	if claim.PairedCoreNode == "" || claim.OutboundDestination == "" {
		return nil, nil
	}
	if claim.SecondPairedCoreNode != "" {
		// 3-position: C → B → A index, R1's final dropoff feeds C.
		orderR1 = []protocol.ComplexOrderStep{
			{Action: "wait", Node: claim.CoreNodeName},
			{Action: "pickup", Node: claim.CoreNodeName},
			buildStep("dropoff", claim.OutboundDestination),
			buildStep("pickup", claim.InboundSource),
			{Action: "dropoff", Node: claim.SecondPairedCoreNode},
		}
		orderR2 = []protocol.ComplexOrderStep{
			{Action: "wait", Node: claim.PairedCoreNode},
			{Action: "pickup", Node: claim.PairedCoreNode},
			{Action: "dropoff", Node: claim.CoreNodeName},
			{Action: "pickup", Node: claim.SecondPairedCoreNode},
			{Action: "dropoff", Node: claim.PairedCoreNode},
		}
		return orderR1, orderR2
	}
	// 2-position: B → A index, R1's final dropoff feeds B.
	orderR1 = []protocol.ComplexOrderStep{
		{Action: "wait", Node: claim.CoreNodeName},
		{Action: "pickup", Node: claim.CoreNodeName},
		buildStep("dropoff", claim.OutboundDestination),
		buildStep("pickup", claim.InboundSource),
		{Action: "dropoff", Node: claim.PairedCoreNode},
	}
	orderR2 = []protocol.ComplexOrderStep{
		{Action: "wait", Node: claim.PairedCoreNode},
		{Action: "pickup", Node: claim.PairedCoreNode},
		{Action: "dropoff", Node: claim.CoreNodeName},
	}
	return orderR1, orderR2
}

// BuildSequentialRemovalSteps builds Order A for sequential mode (removal robot).
// Robot drives to line and holds, waits for operator release, picks up old bin, delivers to destination.
//  1. wait(CoreNodeName)            — drive to node + hold (RDS BinTask=Wait)
//  2. pickup(CoreNodeName)          — pick up old from line
//  3. dropoff(OutboundDestination)  — deliver old to destination
func BuildSequentialRemovalSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "wait", Node: claim.CoreNodeName},      // 1 drive to node + hold
		{Action: "pickup", Node: claim.CoreNodeName},    // 2
		buildStep("dropoff", claim.OutboundDestination), // 3
	}
}

// BuildSequentialBackfillSteps builds Order B for sequential mode (backfill robot).
// Robot picks up new material from source and delivers to line.
// Order B is auto-created by wiring when Order A goes in_transit.
//  1. pickup(InboundSource)    — pick new from source
//  2. dropoff(CoreNodeName)    — deliver to line
func BuildSequentialBackfillSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	steps := []protocol.ComplexOrderStep{
		buildStep("pickup", claim.InboundSource),      // 1
		{Action: "dropoff", Node: claim.CoreNodeName}, // 2
	}
	// Produce/consume are duals: a consume backfill pulls a payload-matched FULL bin
	// from the market; a produce backfill pulls a fresh EMPTY carrier (the store dual).
	// The pickup defaults to a full retrieve, so without this a PRODUCE node's backfill
	// hunts a full payload bin in the empty pool and the dispatch fails ("no bin of
	// requested payload in <inbound group>"), stranding produce-side A/B (sequential)
	// after its first removal. Flag the inbound pickup Empty for produce — the same
	// thing BuildSwapDispatch does via markInboundEmpty for the other modes' StepsA/B.
	// This leg is auto-created by the wiring (handleSequentialBackfill) and never passes
	// through BuildSwapDispatch, so it's flagged here at the source instead.
	if claim.Role == protocol.ClaimRoleProduce && claim.InboundSource != "" {
		markInboundEmpty(steps, claim.InboundSource)
	}
	return steps
}

// ──────────────────────────────────────────────────────────────────────────
// Changeover step builders (Phase 3: orders-up-front with operator gates)
// ──────────────────────────────────────────────────────────────────────────
//
// These builders construct the complex-order step sequences for changeover
// flows. All orders for a node are created at changeover start; the operator
// controls flow by releasing wait steps.

// ChangeoverDispatch is the per-mode shape returned by the SwapMode-aware
// changeover step builders. It packages the two complex-order step lists
// (supply and evac) along with their per-order flags so the planner glue
// can assemble NodeAction.SupplyOrder / EvacOrder directly without
// duplicating the per-mode switch on the planner side.
//
// Single-robot modes: StepsA holds a stage list, StepsB holds the full
// 7-step swap (or 8-step evacuate). Multi-robot modes: StepsA and StepsB
// each hold one robot's coordinated leg.
type ChangeoverDispatch struct {
	StepsA        []protocol.ComplexOrderStep
	DeliveryNodeA string
	AutoConfirmA  bool

	StepsB       []protocol.ComplexOrderStep
	AutoConfirmB bool
}

// BuildSwapChangeoverSteps builds the changeover swap dispatch (no tool
// clearance) for the given SwapMode. Internal switch on fromClaim.SwapMode.
//
//   - single_robot: 7-step Order B at fromClaim.CoreNodeName plus a stage
//     Order A. Single wait at the line.
//   - two_robot: Order A pre-stages new → waits at staging → delivers;
//     Order B drives to line → waits → evacuates to OutboundDestination.
//     Both fire on operator release.
//   - two_robot_press_index: mirrors BuildTwoRobotPressIndexSwapSteps —
//     R1 evacuates from CoreNodeName then refills from InboundSource into
//     the back position; R2 indexes the press.
//   - sequential: single robot, single complex order, mid-sequence wait
//     at the active position. Cutover click flips ActivePull and
//     releases the wait.
//
// For unrecognised / "simple" modes, falls back to the single-robot
// pattern.
//
// inactiveNode / activeNode are populated only for sequential mode (the
// planner reads ActivePull at plan time and computes them); other modes
// ignore the values.
func BuildSwapChangeoverSteps(fromClaim, toClaim *processes.NodeClaim, inactiveNode, activeNode string) ChangeoverDispatch {
	switch fromClaim.SwapMode {
	case protocol.SwapModeTwoRobot:
		return buildTwoRobotChangeoverSwap(fromClaim, toClaim)
	case protocol.SwapModeTwoRobotPressIndex:
		return buildPressIndexChangeoverSwap(fromClaim, toClaim, false /* tooling */)
	case protocol.SwapModeSequential:
		return buildSequentialChangeoverSwap(fromClaim, toClaim, inactiveNode, activeNode)
	case pressPositionSwapMode:
		// Synthesized per-position diff from the press-index different-
		// bin-type fan-out: each position dispatches its own NodeAction
		// so up to three robots fire in parallel for one press's changeover.
		return buildPressIndexPerPositionSwap(fromClaim, toClaim)
	default:
		return buildSingleRobotChangeoverSwap(fromClaim, toClaim, false)
	}
}

// BuildEvacuateChangeoverSteps builds the changeover evacuate dispatch
// (tool clearance needed). Same SwapMode switch as BuildSwapChangeoverSteps:
//
//   - single_robot: wait between dropoff-old-at-staging and pickup-new.
//   - two_robot: identical to Swap — two independent robots clear the
//     line during tooling without an extra operator gate, so no second
//     wait is needed.
//   - two_robot_press_index: extra wait on R1 between dropoff old at
//     OutboundDestination and pickup new from InboundSource.
//   - sequential: paired A/B parallel evac+backfill; single tooling-done
//     click releases both robots' bare waits.
//
// inactiveNode / activeNode are accepted for signature symmetry with
// BuildSwapChangeoverSteps; evacuate doesn't read them (both positions
// come off the line, so the inactive/active distinction is moot).
func BuildEvacuateChangeoverSteps(fromClaim, toClaim *processes.NodeClaim, inactiveNode, activeNode string) ChangeoverDispatch {
	_ = inactiveNode
	_ = activeNode
	switch fromClaim.SwapMode {
	case protocol.SwapModeTwoRobot:
		return buildTwoRobotChangeoverSwap(fromClaim, toClaim)
	case protocol.SwapModeTwoRobotPressIndex:
		return buildPressIndexChangeoverSwap(fromClaim, toClaim, true)
	case protocol.SwapModeSequential:
		return buildSequentialChangeoverEvacuate(fromClaim, toClaim)
	case pressPositionSwapMode:
		// Per-position dispatch: the parent evacuate situation drives the
		// "evacuate" semantics, but at the per-position level the robot
		// work is identical to Swap (evac old, fetch new, deliver new).
		return buildPressIndexPerPositionSwap(fromClaim, toClaim)
	default:
		return buildSingleRobotChangeoverSwap(fromClaim, toClaim, true)
	}
}

// buildSingleRobotChangeoverSwap is the legacy single-robot 7- or
// 8-step pattern. Order A pre-stages at InboundStaging (operator
// confirms staging); Order B does the line-side swap on operator
// release.
func buildSingleRobotChangeoverSwap(fromClaim, toClaim *processes.NodeClaim, tooling bool) ChangeoverDispatch {
	stepsB := []protocol.ComplexOrderStep{
		{Action: "wait", Node: fromClaim.CoreNodeName},       // drive to node + hold ("ready")
		{Action: "pickup", Node: fromClaim.CoreNodeName},     // evacuate old
		{Action: "dropoff", Node: fromClaim.OutboundStaging}, // park old
	}
	if tooling {
		stepsB = append(stepsB, protocol.ComplexOrderStep{Action: "wait"}) // "tooling done"
	}
	stepsB = append(stepsB,
		protocol.ComplexOrderStep{Action: "pickup", Node: toClaim.InboundStaging},    // grab new
		protocol.ComplexOrderStep{Action: "dropoff", Node: toClaim.CoreNodeName},     // deliver new
		protocol.ComplexOrderStep{Action: "pickup", Node: fromClaim.OutboundStaging}, // grab old
		buildStep("dropoff", fromClaim.OutboundDestination),                          // clear old to final
	)
	return ChangeoverDispatch{
		StepsA:        BuildStageSteps(toClaim),
		DeliveryNodeA: toClaim.InboundStaging,
		AutoConfirmA:  false,
		StepsB:        stepsB,
		AutoConfirmB:  true,
	}
}

// buildTwoRobotChangeoverSwap mirrors BuildTwoRobotSwapSteps adapted for
// changeover: from-claim drives the outbound side, to-claim drives the
// inbound side. The "ready" wait is the operator gate that releases both
// robots; once released, Robot B clears the line while Robot A delivers
// the new bin. No second wait point: with two robots running independently
// the line is naturally clear during tooling, and Swap and Evacuate
// produce the same step list.
func buildTwoRobotChangeoverSwap(fromClaim, toClaim *processes.NodeClaim) ChangeoverDispatch {
	if toClaim.InboundStaging == "" {
		return ChangeoverDispatch{}
	}
	stepsA := []protocol.ComplexOrderStep{
		buildStep("pickup", toClaim.InboundSource),        // pick new from source
		{Action: "dropoff", Node: toClaim.InboundStaging}, // stage new
		{Action: "wait", Node: toClaim.InboundStaging},    // "ready" — shared release gate
		{Action: "pickup", Node: toClaim.InboundStaging},
		{Action: "dropoff", Node: toClaim.CoreNodeName},
	}
	stepsB := []protocol.ComplexOrderStep{
		{Action: "wait", Node: fromClaim.CoreNodeName},      // drive to node + hold (shared "ready")
		{Action: "pickup", Node: fromClaim.CoreNodeName},    // evacuate old
		buildStep("dropoff", fromClaim.OutboundDestination), // straight to final
	}
	return ChangeoverDispatch{
		StepsA:        stepsA,
		DeliveryNodeA: toClaim.CoreNodeName,
		AutoConfirmA:  true,
		StepsB:        stepsB,
		AutoConfirmB:  true,
	}
}

// buildPressIndexChangeoverSwap mirrors BuildTwoRobotPressIndexSwapSteps —
// R1 evacuates from CoreNodeName and reloads the back position from
// InboundSource; R2 indexes intermediate positions. Honors 2-pos vs 3-pos
// via fromClaim.SecondPairedCoreNode (using fromClaim's geometry is
// consistent with the swap_dispatch pattern).
//
// For evacuate, a "tooling done" wait sits on R1 between the outbound
// dropoff and the inbound pickup so the operator gates the refill leg
// after the line has been cleared.
func buildPressIndexChangeoverSwap(fromClaim, toClaim *processes.NodeClaim, tooling bool) ChangeoverDispatch {
	if fromClaim.PairedCoreNode == "" || fromClaim.OutboundDestination == "" {
		return ChangeoverDispatch{}
	}
	// R1 prefix is identical for 2-pos and 3-pos: wait, evac, dropoff destination.
	r1 := []protocol.ComplexOrderStep{
		{Action: "wait", Node: fromClaim.CoreNodeName},
		{Action: "pickup", Node: fromClaim.CoreNodeName},
		buildStep("dropoff", fromClaim.OutboundDestination),
	}
	if tooling {
		r1 = append(r1, protocol.ComplexOrderStep{Action: "wait"}) // "tooling done"
	}
	r1 = append(r1, buildStep("pickup", toClaim.InboundSource))
	if fromClaim.SecondPairedCoreNode != "" {
		// 3-position: refill back position.
		r1 = append(r1, protocol.ComplexOrderStep{Action: "dropoff", Node: fromClaim.SecondPairedCoreNode})
	} else {
		// 2-position: refill paired (back) position.
		r1 = append(r1, protocol.ComplexOrderStep{Action: "dropoff", Node: fromClaim.PairedCoreNode})
	}
	var r2 []protocol.ComplexOrderStep
	if fromClaim.SecondPairedCoreNode != "" {
		r2 = []protocol.ComplexOrderStep{
			{Action: "wait", Node: fromClaim.PairedCoreNode},
			{Action: "pickup", Node: fromClaim.PairedCoreNode},
			{Action: "dropoff", Node: fromClaim.CoreNodeName},
			{Action: "pickup", Node: fromClaim.SecondPairedCoreNode},
			{Action: "dropoff", Node: fromClaim.PairedCoreNode},
		}
	} else {
		r2 = []protocol.ComplexOrderStep{
			{Action: "wait", Node: fromClaim.PairedCoreNode},
			{Action: "pickup", Node: fromClaim.PairedCoreNode},
			{Action: "dropoff", Node: fromClaim.CoreNodeName},
		}
	}
	return ChangeoverDispatch{
		StepsA:        r1,
		DeliveryNodeA: fromClaim.CoreNodeName,
		AutoConfirmA:  true,
		StepsB:        r2,
		AutoConfirmB:  true,
	}
}

// buildPressIndexPerPositionSwap is the per-position dispatch for a
// synthesized press-index different-bin-type fan-out claim. Single
// complex order, 4 steps, no operator gate inside the order:
//
//	pickup(my position)       evac old bin
//	dropoff(OutboundDestination)
//	pickup(InboundSource)     fetch new bin
//	dropoff(my position)      deliver new bin
//
// The fan-out post-processor synthesized one such per-position claim
// per occupied/needed position; each gets its own NodeAction with this
// dispatch, so up to 3 robots fire in parallel for one press's
// changeover. Half-cases (from-only or to-only positions) reach the
// planner as SituationDrop / SituationAdd respectively and route
// through the simpler builders (BuildReleaseSteps for evac-only;
// planFallbackStagingAction → Retrieve order for refill-only).
//
// Returns ChangeoverDispatch with StepsA only (single-order shape).
// Empty dispatch when required fields are missing — the planner
// already validated them via the per-mode registry, but the empty-
// dispatch backstop catches the impossible case where validation
// passed but the synthesized claim is malformed.
func buildPressIndexPerPositionSwap(fromClaim, toClaim *processes.NodeClaim) ChangeoverDispatch {
	if fromClaim == nil || toClaim == nil {
		return ChangeoverDispatch{}
	}
	if fromClaim.OutboundDestination == "" || toClaim.InboundSource == "" {
		return ChangeoverDispatch{}
	}
	if fromClaim.CoreNodeName == "" {
		return ChangeoverDispatch{}
	}
	pos := fromClaim.CoreNodeName
	steps := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: pos},                       // evac old bin
		buildStep("dropoff", fromClaim.OutboundDestination), // old bin to destination
		buildStep("pickup", toClaim.InboundSource),          // fetch new bin
		{Action: "dropoff", Node: pos},                      // deliver new bin
	}
	return ChangeoverDispatch{
		StepsA:        steps,
		DeliveryNodeA: pos,
		AutoConfirmA:  true,
		StepsB:        nil,
	}
}

// buildSequentialChangeoverEvacuate handles sequential A/B paired
// evacuate. Sequential swap_mode is always A/B paired (two physical
// bins at the line; A/B cycling keeps the line running). Evacuate
// evacuates BOTH positions in parallel via two robots, AND each robot
// fetches new material in the same order so the deliver legs are pre-
// staged and gated on a single tooling-done click.
//
// Shape per robot (no initial wait — start-changeover IS the trigger):
//
//	pickup(my position)               // evac old
//	dropoff(OutboundDestination)
//	pickup(InboundSource)             // fetch new material
//	wait()                            // tooling-done shared release gate
//	dropoff(my position)              // deliver new
//
// The bare wait (no Node) lets a single tooling-done click release both
// robots' waits via ReleaseChangeoverWait's existing per-task fan-out.
//
// The inactive/active distinction doesn't matter for evac — both
// positions come off the line because production is going down anyway —
// so the choreography uses the from-claim's CoreNodeName and
// PairedCoreNode in a stable order regardless of which side is
// currently active.
//
// Empty dispatch (no steps) is the planner's signal to emit NodeAction.Err
// — sequential without PairedCoreNode is outside the spec'd model.
func buildSequentialChangeoverEvacuate(fromClaim, toClaim *processes.NodeClaim) ChangeoverDispatch {
	if fromClaim.PairedCoreNode == "" {
		return ChangeoverDispatch{}
	}
	if fromClaim.OutboundDestination == "" || toClaim.InboundSource == "" {
		return ChangeoverDispatch{}
	}
	posA := fromClaim.CoreNodeName
	posB := fromClaim.PairedCoreNode
	stepsA := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: posA},                      // evac old A
		buildStep("dropoff", fromClaim.OutboundDestination), // old A to destination
		buildStep("pickup", toClaim.InboundSource),          // fetch new A
		{Action: "wait"},                // shared "tooling done" gate
		{Action: "dropoff", Node: posA}, // deliver new A
	}
	stepsB := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: posB},                      // evac old B
		buildStep("dropoff", fromClaim.OutboundDestination), // old B to destination
		buildStep("pickup", toClaim.InboundSource),          // fetch new B
		{Action: "wait"},                // shared "tooling done" gate
		{Action: "dropoff", Node: posB}, // deliver new B
	}
	return ChangeoverDispatch{
		StepsA:        stepsA,
		DeliveryNodeA: posA,
		AutoConfirmA:  true,
		StepsB:        stepsB,
		AutoConfirmB:  true,
	}
}

// buildSequentialChangeoverSwap is the sequential A/B paired swap:
// single robot, single complex order, single wait inside.
//
// SME model:
//  1. Robot fires immediately, no start-of-order wait.
//  2. Inactive-side swap (direct trips, no staging hop):
//     pickup(inactive position) → dropoff(OutboundDestination)
//     pickup(InboundSource)     → dropoff(inactive position)
//  3. Robot drives to active position and waits for cutover click.
//  4. Cutover click flips ActivePull AND releases the wait
//     (SequentialChangeoverCutover orchestrates both).
//  5. Active-side swap (same direct-trip pattern):
//     pickup(active position)   → dropoff(OutboundDestination)
//     pickup(InboundSource)     → dropoff(active position)
//
// Direct trips, no InboundStaging hop. Sequential's steady-state pattern
// (BuildSequentialBackfillSteps) fetches InboundSource and drops
// directly at CoreNodeName, so changeover follows the same pattern.
// Sequential nodes don't need InboundStaging configured for changeover —
// the only required fields beyond steady-state are OutboundDestination
// + InboundSource (the same fields steady-state sequential needs).
//
// Active-pull awareness: the planner reads ActivePull at plan time and
// passes inactive/active node names. The "swap first" side is the
// inactive one (line keeps running on active during the inactive swap).
// The "swap second" side is the active one, whose swap is gated by the
// operator's cutover click. The cutover click flips active-pull TO the
// freshly-swapped previously-inactive side, making the previously-active
// side the new inactive — which the robot then proceeds to swap.
//
// Empty dispatch when PairedCoreNode is missing or required routing
// fields are empty — the planner emits NodeAction.Err.
func buildSequentialChangeoverSwap(fromClaim, toClaim *processes.NodeClaim, inactiveNode, activeNode string) ChangeoverDispatch {
	if fromClaim.PairedCoreNode == "" {
		return ChangeoverDispatch{}
	}
	if fromClaim.OutboundDestination == "" || toClaim.InboundSource == "" {
		return ChangeoverDispatch{}
	}
	if inactiveNode == "" || activeNode == "" {
		return ChangeoverDispatch{}
	}
	steps := []protocol.ComplexOrderStep{
		// Inactive-side swap (line keeps running on active).
		{Action: "pickup", Node: inactiveNode},              // evac old inactive
		buildStep("dropoff", fromClaim.OutboundDestination), // old inactive to destination
		buildStep("pickup", toClaim.InboundSource),          // fetch new inactive
		{Action: "dropoff", Node: inactiveNode},             // deliver new inactive
		// Robot drives to active position and parks (wait-with-node
		// makes RDS report WAITING so the order reliably transitions to
		// "staged" on Edge — same fragility-fix the two_robot pattern
		// applies for its mid-sequence wait).
		{Action: "wait", Node: activeNode}, // cutover gate
		// Active-side swap (cutover already flipped pull to the
		// previously-inactive side; this side is now safe to evacuate).
		{Action: "pickup", Node: activeNode},                // evac old active
		buildStep("dropoff", fromClaim.OutboundDestination), // old active to destination
		buildStep("pickup", toClaim.InboundSource),          // fetch new active
		{Action: "dropoff", Node: activeNode},               // deliver new active
	}
	return ChangeoverDispatch{
		StepsA:        steps,
		DeliveryNodeA: activeNode, // last dropoff
		AutoConfirmA:  true,
		StepsB:        nil, // single-order shape
	}
}

// BuildKeepStagedEvacSteps builds Robot B's complex order for keep-staged
// changeovers. Simpler than swap/evacuate — no outbound staging hop, goes
// straight to final destination after evacuation.
func BuildKeepStagedEvacSteps(fromClaim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "wait", Node: fromClaim.CoreNodeName},      // drive to node + hold ("ready")
		{Action: "pickup", Node: fromClaim.CoreNodeName},    // evacuate old
		buildStep("dropoff", fromClaim.OutboundDestination), // straight to final
	}
}

// BuildKeepStagedDeliverSteps builds Robot A's complex order for keep-staged
// changeovers (split mode — two robots). Stages new material then waits for
// operator release to deliver.
func BuildKeepStagedDeliverSteps(toClaim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		buildStep("pickup", toClaim.InboundSource),        // grab new
		{Action: "dropoff", Node: toClaim.InboundStaging}, // stage new
		{Action: "wait"}, // "ready"
		{Action: "pickup", Node: toClaim.InboundStaging}, // grab new
		{Action: "dropoff", Node: toClaim.CoreNodeName},  // deliver to line
	}
}

// BuildKeepStagedCombinedSteps builds Robot A's complex order for keep-staged
// changeovers (combined mode — single robot). Clears the keep-staged bin, stages
// new material, waits, then delivers.
func BuildKeepStagedCombinedSteps(fromClaim, toClaim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "pickup", Node: toClaim.InboundStaging},  // grab keep-staged bin
		buildStep("dropoff", fromClaim.InboundSource),     // return to market/source
		buildStep("pickup", toClaim.InboundSource),        // grab changeover material
		{Action: "dropoff", Node: toClaim.InboundStaging}, // stage new
		{Action: "wait"}, // "ready"
		{Action: "pickup", Node: toClaim.InboundStaging}, // grab new
		{Action: "dropoff", Node: toClaim.CoreNodeName},  // deliver to line
	}
}
