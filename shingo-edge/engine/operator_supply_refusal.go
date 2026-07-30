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
	"time"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// RefuseSupply records that the operator at this loader window cannot supply a
// payload.
//
// SCOPED TO THE CARD. The refusal is keyed (loader_node, payload_code), which is
// exactly what the operator is standing in front of, so it cannot be aimed at
// anything wider than the card under their thumb.
//
// IT DOES NOT REQUIRE A LIVE CALL, and that is a reversal. The original design
// accepted a refusal only where an order for that payload already existed at
// that window — "a refusal answers a request, and nothing has been requested."
// Owner correction, and the floor argument beats the symmetry one: the person on
// the reach truck knows the rack is empty when they look at it, and holding the
// warning until a cell calls throws away the notice they could have given. A
// refusal with no call outstanding is not shouted at anybody — the cell side
// attaches it only to a cell that has a call for that payload, so it reaches the
// next cell to ask rather than nobody.
func (e *Engine) RefuseSupply(processNodeID int64, payloadCode, refusedBy string) error {
	node, err := e.loaderCardNode(processNodeID, payloadCode)
	if err != nil {
		return err
	}
	if refusedBy == "" {
		refusedBy = node.Name
	}
	if err := e.db.OpenSupplyRefusal(node.CoreNodeName, payloadCode, refusedBy); err != nil {
		return err
	}
	e.logFn("supply_refusal: %s cannot supply %s (by %s)", node.CoreNodeName, payloadCode, refusedBy)
	// LOCAL FIRST, THEN CORE. The write above already turned the card dormant;
	// this only tells the rest of the plant. A Core that is down must never stop
	// an operator recording what they found on the floor.
	e.emitSupplyRefusal(protocol.SupplyRefusalState{
		Action: protocol.SupplyRefusalOpened, LoaderNode: node.CoreNodeName,
		PayloadCode: payloadCode, RefusedAt: time.Now().UTC(), RefusedBy: refusedBy,
	})
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
	e.emitSupplyRefusal(protocol.SupplyRefusalState{
		Action: protocol.SupplyRefusalClosed, LoaderNode: node.CoreNodeName, PayloadCode: payloadCode,
	})
	return nil
}

// AckSupplyRefusal records the cell operator's answer to a refusal.
//
// THE ANSWER IS THE DISMISSAL, and that is what makes a modal defensible here
// after four review rounds argued against one. The objection was never "don't
// interrupt" — it was that a modal needs a dismiss policy and every dismiss
// policy is an arbitrary snooze interval somebody invented. There is no interval
// to invent when dismissing IS answering: the question stops being asked the
// moment it is answered, and the answer is durable, so it is never asked twice.
//
// WAIT is recorded rather than being the absence of an action. "The operator
// chose to keep waiting" and "nobody has looked at the screen" are different
// facts, and the second one is the complaint this whole project started from.
func (e *Engine) AckSupplyRefusal(processNodeID int64, loaderNode, payloadCode, choice string) error {
	switch choice {
	case protocol.SupplyRefusalChoiceWait, protocol.SupplyRefusalChoiceChangeover:
	default:
		return fmt.Errorf("unknown answer %q — a refusal is answered with wait or changeover", choice)
	}
	node, err := e.db.GetProcessNode(processNodeID)
	if err != nil || node == nil {
		return fmt.Errorf("no such process node %d", processNodeID)
	}
	// The process NAME, matching the demand grain — the same identity the episode
	// key carries, so the two join with no translation.
	processName := e.processName(node.ProcessID)
	ok, err := e.db.AckSupplyRefusal(loaderNode, payloadCode, choice, processName)
	if err != nil {
		return err
	}
	if !ok {
		// Already answered. Not an error the operator should see as a failure —
		// it means a second tap, or a second screen, and the first answer stands.
		e.logFn("supply_refusal: %s/%s already answered; %q from %s ignored",
			loaderNode, payloadCode, choice, processName)
		return nil
	}
	e.logFn("supply_refusal: %s answered %s/%s with %q", processName, loaderNode, payloadCode, choice)
	now := time.Now().UTC()
	e.emitSupplyRefusal(protocol.SupplyRefusalState{
		Action: protocol.SupplyRefusalAcked, LoaderNode: loaderNode, PayloadCode: payloadCode,
		AckAt: &now, AckChoice: choice, AckProcessID: processName,
	})
	return nil
}

// emitSupplyRefusal puts one refusal message on the outbox for Core.
//
// BEST-EFFORT BY DESIGN, unlike the demand-episode emit it otherwise resembles.
// An episode that never reaches Core leaves a gap in the duration record and
// nothing else can recover it. A refusal that never reaches Core still did its
// job locally — the card is dormant and the cells on this edge see it on their
// next poll — and Core's copy is for history, cross-edge supply, and the Core UI.
// So a failed enqueue is logged and swallowed rather than failing the operator's
// action, which is the opposite disposition from emitOriginState and deliberately
// so.
func (e *Engine) emitSupplyRefusal(msg protocol.SupplyRefusalState) {
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectSupplyRefusal,
		protocol.Address{Role: protocol.RoleEdge, Station: e.cfg.StationID()},
		protocol.Address{Role: protocol.RoleCore},
		&msg,
	)
	if err != nil {
		e.logFn("supply_refusal: build envelope %s/%s: %v", msg.LoaderNode, msg.PayloadCode, err)
		return
	}
	data, err := env.Encode()
	if err != nil {
		e.logFn("supply_refusal: encode %s/%s: %v", msg.LoaderNode, msg.PayloadCode, err)
		return
	}
	if _, err := e.db.EnqueueOutbox(data, protocol.SubjectSupplyRefusal); err != nil {
		e.logFn("supply_refusal: enqueue %s/%s: %v", msg.LoaderNode, msg.PayloadCode, err)
	}
}

// HandleSupplyRefusalState applies a refusal Core broadcast to every edge.
//
// Every edge receives every refusal and filters locally — that is what removes
// the addressee problem rather than moving it. The sender receives its own
// message back, which is intended: every apply path here is idempotent, and it
// means a single-edge line exercises the identical code path a multi-edge one
// will, instead of leaving the cross-edge path untested until the day it matters.
func (e *Engine) HandleSupplyRefusalState(st protocol.SupplyRefusalState) {
	if st.LoaderNode == "" || st.PayloadCode == "" {
		return
	}
	var err error
	switch st.Action {
	case protocol.SupplyRefusalOpened:
		err = e.db.OpenSupplyRefusal(st.LoaderNode, st.PayloadCode, st.RefusedBy)
	case protocol.SupplyRefusalAcked:
		_, err = e.db.AckSupplyRefusal(st.LoaderNode, st.PayloadCode, st.AckChoice, st.AckProcessID)
	case protocol.SupplyRefusalClosed:
		err = e.db.DeleteSupplyRefusal(st.LoaderNode, st.PayloadCode)
	default:
		e.logFn("supply_refusal: unknown action %q for %s/%s — ignored",
			st.Action, st.LoaderNode, st.PayloadCode)
		return
	}
	if err != nil {
		e.logFn("supply_refusal: apply %s for %s/%s: %v", st.Action, st.LoaderNode, st.PayloadCode, err)
	}
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

// THE "SOMEBODY ASKED" CHECK USED TO LIVE HERE and was deleted, not disabled.
//
// It required a live order for the payload at the window before a refusal was
// accepted, so that a refusal could only ever answer a request. The reasoning
// was sound and the floor disagrees with it: an operator looking at an empty
// rack knows before any cell calls, and the warning is worth most exactly then.
//
// Recorded because the absence is the decision. Anyone reading RefuseSupply and
// wondering where the authorization went should find this rather than conclude
// it was forgotten. What remains is loaderCardNode, which still establishes that
// the target is a real loader window rendering a real card.
