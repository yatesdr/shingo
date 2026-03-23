package engine

import (
	"shingo/protocol"
	"shingoedge/store"
)

// Material movement step builders.
// These are pure functions that return ComplexOrderStep sequences from a
// StyleNodeClaim's routing config. They are used by both routine
// replenishment and changeover order construction.

// buildStep constructs a single ComplexOrderStep using 3-tier resolution:
//   node (explicit) → nodeGroup (Core resolves) → empty (global fallback via payloadCode)
func buildStep(action, node, nodeGroup string) protocol.ComplexOrderStep {
	if node != "" {
		return protocol.ComplexOrderStep{Action: action, Node: node}
	}
	if nodeGroup != "" {
		return protocol.ComplexOrderStep{Action: action, NodeGroup: nodeGroup}
	}
	return protocol.ComplexOrderStep{Action: action}
}

// BuildDeliverSteps builds steps to deliver material to a node.
// For consume nodes: pickup full bin from source, dropoff at core node.
// For produce nodes: pickup empty bin from source, dropoff at core node.
func BuildDeliverSteps(claim *store.StyleNodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		buildStep("pickup", claim.InboundSourceNode, claim.InboundSourceNodeGroup),
		{Action: "dropoff", Node: claim.CoreNodeName},
	}
}

// BuildReleaseSteps builds steps to remove material from a node and send it
// to the configured outbound destination.
func BuildReleaseSteps(claim *store.StyleNodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "pickup", Node: claim.CoreNodeName},
		buildStep("dropoff", claim.OutboundSourceNode, claim.OutboundSourceNodeGroup),
	}
}

// BuildStageSteps builds steps to pre-stage material at the inbound staging
// node in preparation for a swap. Material is fetched and placed at the
// inbound staging node but NOT yet delivered to the production node.
func BuildStageSteps(claim *store.StyleNodeClaim) []protocol.ComplexOrderStep {
	if claim.InboundStaging == "" {
		return nil // no inbound staging configured, cannot pre-stage
	}
	return []protocol.ComplexOrderStep{
		buildStep("pickup", claim.InboundSourceNode, claim.InboundSourceNodeGroup),
		{Action: "dropoff", Node: claim.InboundStaging},
	}
}

// BuildStagedDeliverSteps builds steps to move already-staged material from
// the inbound staging node to the production node. Used after staging + evacuation.
func BuildStagedDeliverSteps(claim *store.StyleNodeClaim) []protocol.ComplexOrderStep {
	if claim.InboundStaging == "" {
		return nil
	}
	return []protocol.ComplexOrderStep{
		{Action: "pickup", Node: claim.InboundStaging},
		{Action: "dropoff", Node: claim.CoreNodeName},
	}
}

// BuildSingleSwapSteps builds a 10-step single-robot swap sequence:
//  1. pickup(InboundSource)      — pick new from source
//  2. dropoff(InboundStaging)    — park new at inbound staging
//  3. dropoff(CoreNodeName)      — pre-position at lineside
//  4. wait                       — operator releases
//  5. pickup(CoreNodeName)       — pick up old from line
//  6. dropoff(OutboundStaging)   — quick-park old nearby
//  7. pickup(InboundStaging)     — grab new from staging
//  8. dropoff(CoreNodeName)      — deliver new to line
//  9. pickup(OutboundStaging)    — grab old from staging
// 10. dropoff(OutboundSource)    — deliver old to final destination
func BuildSingleSwapSteps(claim *store.StyleNodeClaim) []protocol.ComplexOrderStep {
	if claim.InboundStaging == "" || claim.OutboundStaging == "" {
		return nil
	}
	return []protocol.ComplexOrderStep{
		buildStep("pickup", claim.InboundSourceNode, claim.InboundSourceNodeGroup), // 1
		{Action: "dropoff", Node: claim.InboundStaging},                            // 2
		{Action: "dropoff", Node: claim.CoreNodeName},                              // 3 pre-position
		{Action: "wait"},                                                            // 4
		{Action: "pickup", Node: claim.CoreNodeName},                               // 5
		{Action: "dropoff", Node: claim.OutboundStaging},                           // 6
		{Action: "pickup", Node: claim.InboundStaging},                             // 7
		{Action: "dropoff", Node: claim.CoreNodeName},                              // 8
		{Action: "pickup", Node: claim.OutboundStaging},                            // 9
		buildStep("dropoff", claim.OutboundSourceNode, claim.OutboundSourceNodeGroup), // 10
	}
}

// BuildTwoRobotSwapSteps builds steps for a two-robot coordinated swap.
// Returns two step lists — one for each robot order:
//
// Order A (resupply robot): pickup new from source → stage → wait → pickup from staging → deliver to node
// Order B (removal robot): pre-position at node → wait → pickup old from node → deliver to outbound destination
//
// Edge coordinates: releases Order B first (remove old), then releases Order A (deliver new).
func BuildTwoRobotSwapSteps(claim *store.StyleNodeClaim) (orderA, orderB []protocol.ComplexOrderStep) {
	if claim.InboundStaging == "" {
		return nil, nil
	}
	// Robot A: fetch new material, stage, wait for node clear, then deliver
	orderA = []protocol.ComplexOrderStep{
		buildStep("pickup", claim.InboundSourceNode, claim.InboundSourceNodeGroup), // pick new from source
		{Action: "dropoff", Node: claim.InboundStaging},                            // stage new
		{Action: "wait"},                                                            // wait for node to be cleared
		{Action: "pickup", Node: claim.InboundStaging},                             // pick new from staging
		{Action: "dropoff", Node: claim.CoreNodeName},                              // deliver to production
	}
	// Robot B: pre-position at node, wait for release, remove old to destination
	orderB = []protocol.ComplexOrderStep{
		{Action: "dropoff", Node: claim.CoreNodeName},                                  // pre-position
		{Action: "wait"},                                                                // wait for release signal
		{Action: "pickup", Node: claim.CoreNodeName},                                   // remove old from production
		buildStep("dropoff", claim.OutboundSourceNode, claim.OutboundSourceNodeGroup),  // deliver to destination
	}
	return orderA, orderB
}

// BuildSequentialRemovalSteps builds Order A for sequential mode (removal robot).
// Robot navigates to line, waits for operator release, picks up old bin, delivers to destination.
//  1. dropoff(CoreNodeName)    — pre-position at lineside
//  2. wait                     — operator releases
//  3. pickup(CoreNodeName)     — pick up old from line
//  4. dropoff(OutboundSource)  — deliver old to destination
func BuildSequentialRemovalSteps(claim *store.StyleNodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "dropoff", Node: claim.CoreNodeName},                                  // 1
		{Action: "wait"},                                                                // 2
		{Action: "pickup", Node: claim.CoreNodeName},                                   // 3
		buildStep("dropoff", claim.OutboundSourceNode, claim.OutboundSourceNodeGroup),  // 4
	}
}

// BuildSequentialBackfillSteps builds Order B for sequential mode (backfill robot).
// Robot picks up new material from source and delivers to line.
// Order B is auto-created by wiring when Order A goes in_transit.
//  1. pickup(InboundSource)    — pick new from source
//  2. dropoff(CoreNodeName)    — deliver to line
func BuildSequentialBackfillSteps(claim *store.StyleNodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		buildStep("pickup", claim.InboundSourceNode, claim.InboundSourceNodeGroup), // 1
		{Action: "dropoff", Node: claim.CoreNodeName},                              // 2
	}
}
