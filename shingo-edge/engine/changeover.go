package engine

import (
	"shingo/protocol"
	"shingoedge/store/processes"
)

// BuildRestoreSteps builds steps to return material from outbound staging back
// to the production node. Used after changeover-only node evacuation.
func BuildRestoreSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	if claim.OutboundStaging == "" {
		return nil
	}
	return []protocol.ComplexOrderStep{
		{Action: "pickup", Node: claim.OutboundStaging},
		{Action: "dropoff", Node: claim.CoreNodeName},
	}
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
	FromClaim    *processes.NodeClaim // nil for "add" situations
	ToClaim      *processes.NodeClaim // nil for "drop" situations
}

// DiffStyleClaims computes the material changes needed at each physical node
// when transitioning from one style to another.
func DiffStyleClaims(fromClaims, toClaims []processes.NodeClaim) []ChangeoverNodeDiff {
	fromMap := make(map[string]*processes.NodeClaim, len(fromClaims))
	for i := range fromClaims {
		fromMap[fromClaims[i].CoreNodeName] = &fromClaims[i]
	}
	toMap := make(map[string]*processes.NodeClaim, len(toClaims))
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
			if to.PayloadCode == "__empty__" {
				situation = SituationUnchanged // node was empty, stays empty
			} else {
				situation = SituationAdd
			}
		case from != nil && to == nil:
			situation = SituationDrop
		case to != nil && to.PayloadCode == "__empty__":
			if from != nil && from.PayloadCode == "__empty__" {
				situation = SituationUnchanged // both empty → nothing to do
			} else {
				situation = SituationDrop // explicitly clear the node
			}
		case from != nil && from.PayloadCode == "__empty__":
			situation = SituationAdd // node was empty, now needs material
		case (from != nil && from.Role == "changeover") || (to != nil && to.Role == "changeover"):
			// Changeover-only nodes always evacuate and restore
			situation = SituationEvacuate
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
