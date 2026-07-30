// operator_supply_refusal.go — the loader operator's "I cannot fill this call",
// and the withdrawal of it.
//
// One person telling another person they cannot supply them. Not an inventory
// record and not a verdict: Shingo's own pool count cannot make this statement,
// because Shingo is a subset of the greater Martinrea system and a human on a
// reach truck can see material — and the absence of material — that no query
// here will ever know about.

package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// RefuseSupply records that the operator at this loader window cannot fill the
// standing call for a payload.
//
// SCOPED TO THE CARD, and the scoping is what makes owner decision 2 —
// "must not fail loudly for anything nobody asked about" — structural rather
// than a rule somebody has to remember. The refusal is keyed
// (loader_node, payload_code) and is only accepted where a live order for that
// payload already exists at that window. No order, no refusal: it is not
// possible to declare a payload unavailable that nobody has asked for.
func (e *Engine) RefuseSupply(processNodeID int64, payloadCode, refusedBy string) error {
	node, err := e.loaderCardNode(processNodeID, payloadCode)
	if err != nil {
		return err
	}
	if err := e.requireLiveCallFor(node, payloadCode); err != nil {
		return err
	}
	if refusedBy == "" {
		refusedBy = node.Name
	}
	if err := e.db.OpenSupplyRefusal(node.CoreNodeName, payloadCode, refusedBy); err != nil {
		return err
	}
	e.logFn("supply_refusal: %s cannot supply %s (by %s)", node.CoreNodeName, payloadCode, refusedBy)
	return nil
}

// UndoSupplyRefusal withdraws a refusal.
//
// Required, not optional. A one-way destructive button a human can mis-tap,
// whose consequence may be another operator abandoning a run, is not acceptable
// on a floor. It is the mis-tap path — the NORMAL ending is a LOAD at that
// window, which clears the same row.
//
// Deliberately does NOT require the live-call check. A refusal can outlive the
// order that justified it (the cell changed over, the order was cancelled), and
// the operator must still be able to take back something they said.
func (e *Engine) UndoSupplyRefusal(processNodeID int64, payloadCode string) error {
	node, err := e.loaderCardNode(processNodeID, payloadCode)
	if err != nil {
		return err
	}
	if err := e.db.DeleteSupplyRefusal(node.CoreNodeName, payloadCode); err != nil {
		return err
	}
	e.logFn("supply_refusal: %s withdrew the refusal for %s", node.CoreNodeName, payloadCode)
	return nil
}

// loaderCardNode resolves a process node to the loader window a card lives on,
// and refuses anything that is not one.
//
// THE AUTHORIZATION THIS HMI CAN ACTUALLY MAKE, stated plainly. There is no
// per-request station identity on the operator endpoints — every operator action
// here takes a node id and no session — so "reject a station that does not own
// this window" cannot be enforced as a session check. What IS enforceable, and
// is enforced here, is that the node is a real manual_swap loader window that
// renders on some station's board. A node with no operator_station_id renders
// nowhere, so a refusal against it could never have been made by a person
// looking at a card.
func (e *Engine) loaderCardNode(processNodeID int64, payloadCode string) (*processes.Node, error) {
	if payloadCode == "" {
		return nil, fmt.Errorf("a refusal names a payload; none was given")
	}
	node, err := e.db.GetProcessNode(processNodeID)
	if err != nil || node == nil {
		return nil, fmt.Errorf("no such process node %d", processNodeID)
	}
	if node.CoreNodeName == "" {
		return nil, fmt.Errorf("node %s has no core node name", node.Name)
	}
	if node.OperatorStationID == nil {
		return nil, fmt.Errorf("node %s belongs to no operator station, so it renders no card",
			node.Name)
	}
	// Engine.loadActiveNode, not the package-level one: it carries the
	// Core-owned-loader fallback, synthesising a manual_swap claim for a window
	// that has no per-style edge claim. Without it every Core-owned loader — the
	// direction the whole loader refactor went — would fail this check and the
	// button would be dead on exactly the boards it was built for.
	_, _, claim, cerr := e.loadActiveNode(processNodeID)
	if cerr != nil || claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.SwapMode != protocol.SwapModeManualSwap {
		return nil, fmt.Errorf("node %s is not a loader window (swap mode %q)", node.Name, claim.SwapMode)
	}
	return node, nil
}

// requireLiveCallFor is the "somebody asked" term, server-side.
//
// The card's red condition is a conjunction of three things — queued for a bin,
// an active style claims it, none filled — and this checks ONE of them on
// purpose. It is the one that carries decision 2: the active-style term is
// effectively always true on a dedicated-home board (LoadablePayloadCodesAt
// overrides the claim-derived set), so it authorises nothing, and "none filled"
// is a display state that flickers with every delivery.
//
// A live order for this payload at this window is the whole of "somebody asked",
// it is stable, and it is the term the card cannot be red without.
//
// Not a reimplementation of cardModel. The full three-term condition lives in
// the render layer, and duplicating it here would create two definitions of red
// that drift — with the server's copy winning silently.
func (e *Engine) requireLiveCallFor(node *processes.Node, payloadCode string) error {
	live, err := e.db.ListActiveOrdersByProcessNodeOrSource(node.ID, node.CoreNodeName)
	if err != nil {
		return fmt.Errorf("check for a live call at %s: %w", node.Name, err)
	}
	for _, o := range live {
		if o.PayloadCode == payloadCode {
			return nil
		}
	}
	return fmt.Errorf("no outstanding call for %s at %s — a refusal answers a request, "+
		"and nothing has been requested", payloadCode, node.Name)
}
