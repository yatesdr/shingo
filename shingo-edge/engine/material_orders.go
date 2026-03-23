package engine

import (
	"shingo/protocol"
	"shingoedge/store"
)

// Material movement step builders.
// These are pure functions that return ComplexOrderStep sequences from a
// StyleNodeClaim's routing config. They are used by both routine
// replenishment and changeover order construction.

// BuildDeliverSteps builds steps to deliver material to a node.
// For consume nodes: pickup full bin from storage, dropoff at core node.
// For produce nodes: pickup empty bin from storage, dropoff at core node.
func BuildDeliverSteps(claim *store.StyleNodeClaim) []protocol.ComplexOrderStep {
	// If inbound staging is configured, deliver from staging to node
	if claim.InboundStaging != "" {
		return []protocol.ComplexOrderStep{
			{Action: "pickup", NodeGroup: claim.InboundStaging},
			{Action: "dropoff", Node: claim.CoreNodeName},
		}
	}
	// Direct delivery — core picks from wherever storage is
	return []protocol.ComplexOrderStep{
		{Action: "pickup"},
		{Action: "dropoff", Node: claim.CoreNodeName},
	}
}

// BuildReleaseSteps builds steps to remove material from a node and send it
// to the configured outbound staging destination.
func BuildReleaseSteps(claim *store.StyleNodeClaim) []protocol.ComplexOrderStep {
	target := claim.OutboundStaging
	if target == "" {
		target = claim.CoreNodeName
	}
	return []protocol.ComplexOrderStep{
		{Action: "pickup", Node: claim.CoreNodeName},
		{Action: "dropoff", Node: target},
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
		{Action: "pickup"},
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

// BuildSingleSwapSteps builds a single-robot swap sequence:
// 1. Pickup new material from storage → dropoff at inbound staging
// 2. Wait for release signal
// 3. Pickup old material from node → dropoff at outbound staging (fast, nearby)
// 4. Pickup new material from inbound staging → dropoff at node
func BuildSingleSwapSteps(claim *store.StyleNodeClaim) []protocol.ComplexOrderStep {
	if claim.InboundStaging == "" || claim.OutboundStaging == "" {
		return nil
	}
	return []protocol.ComplexOrderStep{
		{Action: "pickup"},                                      // pick new from storage
		{Action: "dropoff", Node: claim.InboundStaging},         // park new at inbound staging
		{Action: "wait"},                                         // wait for release signal
		{Action: "pickup", Node: claim.CoreNodeName},             // remove old from production
		{Action: "dropoff", Node: claim.OutboundStaging},         // park old at outbound staging (fast, nearby)
		{Action: "pickup", Node: claim.InboundStaging},           // grab new from inbound staging
		{Action: "dropoff", Node: claim.CoreNodeName},            // deliver new to production — RESUMES HERE
	}
}

// BuildTwoRobotSwapSteps builds steps for a two-robot coordinated swap.
// Returns two step lists — one for each robot order:
//
// Order A (resupply robot): pickup new → stage → wait → pickup from staging → deliver to node
// Order B (removal robot): wait at node → pickup old from node → deliver to outbound staging
//
// Edge coordinates: releases Order B first (remove old), then releases Order A (deliver new).
func BuildTwoRobotSwapSteps(claim *store.StyleNodeClaim) (orderA, orderB []protocol.ComplexOrderStep) {
	if claim.InboundStaging == "" || claim.OutboundStaging == "" {
		return nil, nil
	}
	// Robot A: fetch new material, stage, wait for node clear, then deliver
	orderA = []protocol.ComplexOrderStep{
		{Action: "pickup"},                                      // pick new from storage
		{Action: "dropoff", Node: claim.InboundStaging},         // stage new
		{Action: "wait"},                                         // wait for node to be cleared
		{Action: "pickup", Node: claim.InboundStaging},           // pick new from staging
		{Action: "dropoff", Node: claim.CoreNodeName},            // deliver to production
	}
	// Robot B: wait for release, remove old to outbound staging
	orderB = []protocol.ComplexOrderStep{
		{Action: "wait"},                                         // wait for release signal
		{Action: "pickup", Node: claim.CoreNodeName},             // remove old from production
		{Action: "dropoff", Node: claim.OutboundStaging},         // park at outbound staging
	}
	return orderA, orderB
}

// ChangeoverSituation classifies what needs to happen at a physical node
// when transitioning between two styles.
type ChangeoverSituation string

const (
	SituationUnchanged ChangeoverSituation = "unchanged" // same payload, no evacuation needed
	SituationEvacuate  ChangeoverSituation = "evacuate"  // same payload, but evacuation required
	SituationSwap      ChangeoverSituation = "swap"      // different payload — old out, new in
	SituationDrop      ChangeoverSituation = "drop"      // old style uses node, new style doesn't
	SituationAdd       ChangeoverSituation = "add"       // new style uses node, old style didn't
)

// ChangeoverNodeDiff represents the material change needed at a single physical
// node during a changeover.
type ChangeoverNodeDiff struct {
	CoreNodeName string
	Situation    ChangeoverSituation
	FromClaim    *store.StyleNodeClaim // nil for "add" situations
	ToClaim      *store.StyleNodeClaim // nil for "drop" situations
}

// DiffStyleClaims computes the material changes needed at each physical node
// when transitioning from one style to another.
func DiffStyleClaims(fromClaims, toClaims []store.StyleNodeClaim) []ChangeoverNodeDiff {
	fromMap := make(map[string]*store.StyleNodeClaim, len(fromClaims))
	for i := range fromClaims {
		fromMap[fromClaims[i].CoreNodeName] = &fromClaims[i]
	}
	toMap := make(map[string]*store.StyleNodeClaim, len(toClaims))
	for i := range toClaims {
		toMap[toClaims[i].CoreNodeName] = &toClaims[i]
	}

	// Collect all unique node names
	nodeSet := make(map[string]bool)
	for name := range fromMap {
		nodeSet[name] = true
	}
	for name := range toMap {
		nodeSet[name] = true
	}

	var diffs []ChangeoverNodeDiff
	for name := range nodeSet {
		from := fromMap[name]
		to := toMap[name]

		var situation ChangeoverSituation
		switch {
		case from == nil && to != nil:
			situation = SituationAdd
		case from != nil && to == nil:
			situation = SituationDrop
		case from.PayloadCode == to.PayloadCode && from.Role == to.Role:
			if to.EvacuateOnChangeover {
				situation = SituationEvacuate
			} else {
				situation = SituationUnchanged
			}
		default:
			situation = SituationSwap
		}

		diffs = append(diffs, ChangeoverNodeDiff{
			CoreNodeName: name,
			Situation:    situation,
			FromClaim:    from,
			ToClaim:      to,
		})
	}
	return diffs
}
