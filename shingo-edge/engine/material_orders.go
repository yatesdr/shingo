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

// BuildDeliverSteps builds steps to deliver material to a node.
// For consume nodes: pickup full bin from source, dropoff at core node.
// For produce nodes: pickup empty bin from source, dropoff at core node.
// TODO(dead-code): no callers as of 2026-04-17; verify before the next refactor.
func BuildDeliverSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		buildStep("pickup", claim.InboundSource),
		{Action: "dropoff", Node: claim.CoreNodeName},
	}
}

// BuildReleaseSteps builds steps to remove material from a node and send it
// to the configured outbound destination.
func BuildReleaseSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
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
		buildStep("pickup", claim.InboundSource),        // 1
		{Action: "dropoff", Node: claim.InboundStaging}, // 2
		{Action: "wait", Node: claim.CoreNodeName},      // 3 drive to node + hold
		{Action: "pickup", Node: claim.CoreNodeName},    // 4
		{Action: "dropoff", Node: claim.OutboundStaging},  // 5
		{Action: "pickup", Node: claim.InboundStaging},    // 6
		{Action: "dropoff", Node: claim.CoreNodeName},     // 7
		{Action: "pickup", Node: claim.OutboundStaging},   // 8
		buildStep("dropoff", claim.OutboundDestination),   // 9
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
		buildStep("pickup", claim.InboundSource),         // pick new from source
		{Action: "dropoff", Node: claim.InboundStaging},  // stage new
		{Action: "wait", Node: claim.InboundStaging},     // hold at staging until line clears
		{Action: "pickup", Node: claim.InboundStaging},   // pick new from staging
		{Action: "dropoff", Node: claim.CoreNodeName},    // deliver to production
	}
	// Robot B: drive to node and hold, wait for release, remove old to destination
	orderB = []protocol.ComplexOrderStep{
		{Action: "wait", Node: claim.CoreNodeName},                    // drive to node + hold (RDS BinTask=Wait)
		{Action: "pickup", Node: claim.CoreNodeName},                  // remove old from production
		buildStep("dropoff", claim.OutboundDestination),               // deliver to destination
	}
	return orderA, orderB
}

// BuildTwoRobotPressIndexSwapSteps builds steps for a press-indexing two-robot
// swap. The press has two positions: A (front/output, claim.CoreNodeName) and
// B (back/input, claim.PairedCoreNode). When A is full:
//
//	Robot 1 (R1, multi-bin order):
//	  wait(A) → pickup(A) → dropoff(OutboundDestination)
//	         → pickup(InboundSource) → dropoff(B)
//	Robot 2 (R2, single-bin order):
//	  wait(B) → pickup(B) → dropoff(A)
//
// Both fire on operator release. R1 carries the full bin out then returns
// with a fresh bin for the back position; R2 indexes the staged bin from B
// forward into A. Leg sequencing on node A is enforced by the fleet manager
// (R2's dropoff(A) waits for R1's pickup(A) to clear A).
func BuildTwoRobotPressIndexSwapSteps(claim *processes.NodeClaim) (orderR1, orderR2 []protocol.ComplexOrderStep) {
	if claim.PairedCoreNode == "" || claim.OutboundDestination == "" {
		return nil, nil
	}
	orderR1 = []protocol.ComplexOrderStep{
		{Action: "wait", Node: claim.CoreNodeName},      // park at A, hold for operator release
		{Action: "pickup", Node: claim.CoreNodeName},    // lift full bin off A
		buildStep("dropoff", claim.OutboundDestination), // deliver full to outgoing
		buildStep("pickup", claim.InboundSource),        // fetch fresh bin from incoming
		{Action: "dropoff", Node: claim.PairedCoreNode}, // drop fresh bin at B (back position)
	}
	orderR2 = []protocol.ComplexOrderStep{
		{Action: "wait", Node: claim.PairedCoreNode},   // park at B, hold for operator release
		{Action: "pickup", Node: claim.PairedCoreNode}, // lift staged bin off B
		{Action: "dropoff", Node: claim.CoreNodeName},  // index forward to A
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
	return []protocol.ComplexOrderStep{
		buildStep("pickup", claim.InboundSource), // 1
		{Action: "dropoff", Node: claim.CoreNodeName},                              // 2
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Changeover step builders (Phase 3: orders-up-front with operator gates)
// ──────────────────────────────────────────────────────────────────────────
//
// These builders construct the complex-order step sequences for changeover
// flows. All orders for a node are created at changeover start; the operator
// controls flow by releasing wait steps.

// BuildSwapChangeoverSteps builds Robot B's complex order for a swap changeover
// (no tool clearance). Single wait point. Robot drives to core and holds (Wait),
// operator releases, then evacuates old → delivers new → clears old to final.
func BuildSwapChangeoverSteps(fromClaim, toClaim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "wait", Node: fromClaim.CoreNodeName},       // drive to node + hold ("ready")
		{Action: "pickup", Node: fromClaim.CoreNodeName},     // evacuate old
		{Action: "dropoff", Node: fromClaim.OutboundStaging}, // park old
		{Action: "pickup", Node: toClaim.InboundStaging},     // grab new
		{Action: "dropoff", Node: toClaim.CoreNodeName},      // deliver new
		{Action: "pickup", Node: fromClaim.OutboundStaging},  // grab old
		buildStep("dropoff", fromClaim.OutboundDestination),  // clear old to final
	}
}

// BuildEvacuateChangeoverSteps builds Robot B's complex order for an evacuate
// changeover (tool clearance needed). Two wait points — "ready" and "tooling done".
func BuildEvacuateChangeoverSteps(fromClaim, toClaim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "wait", Node: fromClaim.CoreNodeName},       // drive to node + hold ("ready")
		{Action: "pickup", Node: fromClaim.CoreNodeName},     // evacuate old
		{Action: "dropoff", Node: fromClaim.OutboundStaging}, // park old
		{Action: "wait"},                                      // "tooling done"
		{Action: "pickup", Node: toClaim.InboundStaging},     // grab new
		{Action: "dropoff", Node: toClaim.CoreNodeName},      // deliver new
		{Action: "pickup", Node: fromClaim.OutboundStaging},  // grab old
		buildStep("dropoff", fromClaim.OutboundDestination),  // clear old to final
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
		{Action: "wait"},                                   // "ready"
		{Action: "pickup", Node: toClaim.InboundStaging},  // grab new
		{Action: "dropoff", Node: toClaim.CoreNodeName},   // deliver to line
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
		{Action: "wait"},                                   // "ready"
		{Action: "pickup", Node: toClaim.InboundStaging},  // grab new
		{Action: "dropoff", Node: toClaim.CoreNodeName},   // deliver to line
	}
}
