//go:build sim

package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/service"
	"shingoedge/store/processes"
)

// sim_operator_tap.go — what the operator taps at an unloader window whose
// station the sim config names (sim.operators.unloader_taps). A person at that
// board reads the card: the carts are sent back as the lit choice says, through
// the same two doors the panel posts to (operator-render.js confirmPushEmpty,
// confirmUnloadSwap): the CLEAR with the type, or PUSH EMPTY. A station not
// named keeps the plain CLEAR.
//
// THE SAME ASSIGNMENT THE PANEL LIGHTS: service.AssignmentAt over the Edge's copy
// of the group lines, the read StampBins makes for the tile.

// unloaderTap is one tap on a window's panel.
type unloaderTap struct {
	pushEmpty bool   // PUSH EMPTY; otherwise a CLEAR
	binType   string // the CLEAR's type; "" a plain CLEAR
	layers    *int   // the CLEAR's height, for a type with wanted heights
}

// litChoice is the choice the panel lights for an assignment, nil for none. The
// one place an assignment is read as lit: an assignment that says nothing is
// short lights nothing, and that reading belongs here.
func litChoice(a *domain.TileAssignment) *domain.TileAssignment {
	return a
}

// chooseUnloaderTap is the operator's whole decision: what to tap for the cart at
// a window, given what the station is set to do, whether the cart holds parts, and
// what is lit. A full always takes a CLEAR, because PUSH EMPTY refuses one; with
// nothing lit or under `empty` it is the plain CLEAR.
//
// An empty with nothing lit is PUSH EMPTY: it parks against the outbound group
// until something is short.
func chooseUnloaderTap(mode string, stocked bool, lit *domain.TileAssignment) unloaderTap {
	if mode == config.SimTapLit && lit != nil {
		return unloaderTap{binType: lit.BinType, layers: lit.Layers}
	}
	if stocked {
		return unloaderTap{}
	}
	return unloaderTap{pushEmpty: true}
}

// unloaderTapAt is the node's station's setting, "" for none.
func (op *simOperator) unloaderTapAt(node *processes.Node) string {
	if len(op.ops.UnloaderTaps) == 0 || node == nil || node.OperatorStationID == nil {
		return ""
	}
	st, err := op.e.db.GetOperatorStation(*node.OperatorStationID)
	if err != nil || st == nil {
		return ""
	}
	for _, t := range op.ops.UnloaderTaps {
		if t.Station != st.Code {
			continue
		}
		if t.Tap != config.SimTapLit && t.Tap != config.SimTapEmpty {
			op.e.debugFn("[sim] operator: station %s has unknown unloader tap %q, keeping the plain CLEAR", st.Code, t.Tap)
			return ""
		}
		return t.Tap
	}
	return ""
}

// tapUnloader reads the window as the board shows it and taps.
//
// A cart whose empty-out is already filed is not touched: it is on its way, and
// PUSH EMPTY would refuse it.
func (op *simOperator) tapUnloader(nodeID int64, coreNode, mode string) error {
	held := op.e.LocalNodeBins()
	row, ok := held.Rows[coreNode]
	if !held.Held || !ok || !row.Occupied {
		return fmt.Errorf("no bin at node %s", coreNode)
	}
	if !row.Stocked && op.emptyOutFiled(row.BinID) {
		return nil
	}
	tap := chooseUnloaderTap(mode, row.Stocked, litChoice(service.AssignmentAt(held.Groups, coreNode)))
	if tap.pushEmpty {
		return op.e.PushEmptyOut(nodeID)
	}
	return op.e.ClearBin(nodeID, tap.binType, false, tap.layers)
}

// emptyOutFiled reports whether a move is already carrying this cart. A read that
// could not say counts as filed, the direction emptyOutInFlight's callers take: a
// duplicate sends two robots for one bin.
func (op *simOperator) emptyOutFiled(binID int64) bool {
	_, inFlight, err := op.e.emptyOutInFlight(binID)
	return err != nil || inFlight
}

// operatorTapsAt is the sweep's decision at a window with a tap setting: a person
// works every cart standing there, empty or full, except a bare cart (its panel
// is the stage 2's, not this one) and one whose empty-out is already filed.
func (op *simOperator) operatorTapsAt(row protocol.NodeBinsRow) bool {
	if row.BinID == 0 || row.Bare {
		return false
	}
	return row.Stocked || !op.emptyOutFiled(row.BinID)
}
