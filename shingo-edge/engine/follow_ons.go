package engine

import (
	"shingo/protocol"
	"shingoedge/engine/changeover"
)

// followOnPositions is the positions of a cell that a pair's legs lift from
// and no leg sets down on, in positions' order. Each is owed a carrier by an
// order made after the lift, a follow-on: a sequential line's backfill, made
// when its removal starts moving.
//
// READ FROM THE LEGS' OWN STEPS, never from a setting, so a claim edited
// between the dispatch and the lift cannot make the cell owe a second carrier
// or none. A node that is no position of the cell (a market, a staging spot)
// owes nothing.
func followOnPositions(positions []string, legs ...[]protocol.ComplexOrderStep) []string {
	lifted, setDown := map[string]bool{}, map[string]bool{}
	for _, steps := range legs {
		for _, s := range steps {
			switch s.Action {
			case protocol.ActionPickup:
				lifted[s.Node] = true
			case protocol.ActionDropoff:
				setDown[s.Node] = true
			}
		}
	}
	var out []string
	for _, p := range positions {
		if lifted[p] && !setDown[p] {
			out = append(out, p)
		}
	}
	return out
}

// actionLegs is a changeover action's legs' steps, for followOnPositions.
func actionLegs(a changeover.NodeAction) [][]protocol.ComplexOrderStep {
	var legs [][]protocol.ComplexOrderStep
	for _, spec := range []*changeover.OrderSpec{a.SupplyOrder, a.EvacOrder} {
		if spec != nil && spec.Complex != nil {
			legs = append(legs, spec.Complex.Steps)
		}
	}
	return legs
}
