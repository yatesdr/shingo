package engine

import (
	"database/sql"
	"errors"
	"fmt"

	"shingo/protocol"
	"shingoedge/domain"
	ordermgr "shingoedge/orders"
	"shingoedge/store/processes"
)

// operator_send_back.go — SEND BACK on a tote press: the route, its CANCEL,
// and START's step. The line is decided pure (domain.SendBackLine); here are
// the reads it is decided from, under the line's lock, and the two orders.
//
// SEND BACK sends the cart standing on the press's outside position to the
// claim's outbound (the unloader's card decides what it goes back as) and
// pulls tops & bottoms of the press's tote to the outside in its place. Both
// orders are created on the OUTSIDE's own process node, auto-confirmed (the
// outside has no tile and nobody receives them) and owed to no demand
// (NoDemand). Neither ever touches the press's runtime slots.

// SendBackResult is the two orders SEND BACK made.
type SendBackResult struct {
	Move *domain.Order `json:"move"`
	Pull *domain.Order `json:"pull"`
}

// sendBackCell is what the route, CANCEL and START read of one press: its
// claim, the outside's process node and its live orders.
type sendBackCell struct {
	node    *processes.Node
	claim   *processes.NodeClaim
	outside *processes.Node // nil when the outside has no process node row
}

// sendBackCellOf finds a press's outside process node on its process (one
// read). nil outside is no refusal here; the line says absent.
func (e *Engine) sendBackCellOf(node *processes.Node, claim *processes.NodeClaim) (sendBackCell, error) {
	c := sendBackCell{node: node, claim: claim}
	if claim == nil || claim.PairedCoreNode == "" {
		return c, nil
	}
	nodes, err := e.db.ListProcessNodesByProcess(node.ProcessID)
	if err != nil {
		return c, fmt.Errorf("process nodes of %s: %w", node.Name, err)
	}
	for i := range nodes {
		if nodes[i].CoreNodeName == claim.PairedCoreNode {
			c.outside = &nodes[i]
			break
		}
	}
	return c, nil
}

// sendBackFacts reads the line's facts for a press, as the board reads them
// from its own rows: the claim's style's claims by core node (whether one
// stands on the paired position, domain.PairedClaimed), the press's and the
// outside's live orders and its runtime slots, the bins copy's rows for the
// outside and the press, and whether a changeover is armed on the process.
func (e *Engine) sendBackFacts(c sendBackCell, runtime *processes.RuntimeState) (domain.SendBackFacts, error) {
	f := domain.SendBackFacts{Produce: c.claim.Role == protocol.ClaimRoleProduce, Paired: c.claim.PairedCoreNode,
		Outbound: c.claim.OutboundDestination, OutsideNode: c.outside != nil}
	if c.claim.PairedCoreNode != "" {
		byStyle, err := processes.ClaimsByNodeForStyles(e.db.DB, []int64{c.claim.StyleID})
		if err != nil {
			return f, fmt.Errorf("claims of %s's style: %w", c.node.Name, err)
		}
		f.PairedClaimed = domain.PairedClaimed(c.claim, byStyle[c.claim.StyleID])
	}
	if !f.Produce || !domain.FedShape(f.Paired, f.PairedClaimed) || c.outside == nil {
		return f, nil
	}
	press, err := e.db.ListActiveOrdersByProcessNode(c.node.ID)
	if err != nil {
		return f, fmt.Errorf("orders of %s: %w", c.node.Name, err)
	}
	outside, err := e.db.ListActiveOrdersByProcessNode(c.outside.ID)
	if err != nil {
		return f, fmt.Errorf("orders of %s: %w", c.outside.Name, err)
	}
	var slots []*int64
	if runtime != nil {
		slots = []*int64{runtime.ActiveOrderID, runtime.StagedOrderID}
	}
	f.OrderFacts(press, outside, slots...)
	rows, err := e.binsForDecision(c.claim.PairedCoreNode, c.claim.CoreNodeName)
	if err != nil {
		return f, err
	}
	if r, ok := rows[c.claim.PairedCoreNode]; ok {
		f.Outside = &r
	}
	if r, ok := rows[c.claim.CoreNodeName]; ok {
		f.Core = &r
	}
	co, err := e.db.GetActiveProcessChangeover(c.node.ProcessID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return f, fmt.Errorf("changeover of %s: %w", c.node.Name, err)
	}
	f.ChangeoverArmed = err == nil && co != nil
	return f, nil
}

// SendBack is the route's door: the press at nodeID (its core position's
// process node) sends the cart on its outside back and pulls tops & bottoms
// in its place. Under the line's prime lock it decides the line again from its
// own reads (SendBackLine) and acts only on enabled; then the named-bin move
// of the cart to the outbound (it keeps the drop Core chooses for it), and
// the pull, in that order, both on the outside's process node.
func (e *Engine) SendBack(nodeID int64) (*SendBackResult, error) {
	node, runtime, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return nil, err
	}
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	cellMu := e.primeNodeLock(claim)
	cellMu.Lock()
	defer cellMu.Unlock()

	c, err := e.sendBackCellOf(node, claim)
	if err != nil {
		return nil, err
	}
	f, err := e.sendBackFacts(c, runtime)
	if err != nil {
		return nil, err
	}
	line := domain.SendBackLine(f)
	if line.State != domain.SendBackEnabled {
		return nil, e.sendBackRefusal(node, c, f, line)
	}
	if f.Outside == nil || f.Outside.BinID == 0 {
		return nil, fmt.Errorf("%s: the cart on %s is not known to Core", node.Name, f.Paired)
	}
	origin := ordermgr.NoDemand()
	move, err := e.orderMgr.CreateMoveOrderForBin(&c.outside.ID, f.Paired, claim.OutboundDestination,
		f.Outside.PayloadCode, f.Outside.BinID, origin)
	if err != nil {
		return nil, fmt.Errorf("send back %s: %w", f.Paired, err)
	}
	pull, err := e.orderMgr.CreateTopsAndBottomsPull(&c.outside.ID, f.Paired, claim.InboundSource, claim.PayloadCode, origin)
	if err != nil {
		return &SendBackResult{Move: move}, fmt.Errorf("tops & bottoms to %s: %w", f.Paired, err)
	}
	e.logFn("send back: %s sent %s (bin %d) to %s, pull %d for tops & bottoms from %s",
		node.Name, f.Paired, f.Outside.BinID, claim.OutboundDestination, pull.ID, claim.InboundSource)
	return &SendBackResult{Move: move, Pull: pull}, nil
}

// sendBackAtStart is START's one SEND BACK step, taken before its cancel from
// the live orders START already read: for the fed outside of each claim the
// changeover changes (the claims behind changingNodeSet; an Unchanged press's
// SEND BACK is not read), SEND BACK's orders on the outside's process node
// (plan.nodes holds it). Any in a Block status refuses START, and nothing is
// cancelled: a faulted one with changeoverBlockerFor's faulted sentence, any
// other with "<outside>: SEND BACK in progress — start the changeover when it
// finishes". Otherwise the unmoved ones are returned, for the start's cancel
// to abort with its own.
func sendBackAtStart(plan *changeoverPlan, live []domain.Order) ([]int64, error) {
	changing := map[string]bool{}
	for _, n := range changingNodeSet(plan) {
		changing[n] = true
	}
	claimed := map[string]bool{}
	for i := range plan.fromClaims {
		claimed[plan.fromClaims[i].CoreNodeName] = true
	}
	nodeOf := map[string]int64{}
	for _, n := range plan.nodes {
		nodeOf[n.CoreNodeName] = n.ID
	}
	var cancel []int64
	for i := range plan.fromClaims {
		c := &plan.fromClaims[i]
		if !changing[c.CoreNodeName] || c.Role != protocol.ClaimRoleProduce ||
			!domain.FedShape(c.PairedCoreNode, domain.PairedClaimed(c, claimed)) {
			continue
		}
		id, ok := nodeOf[c.PairedCoreNode]
		if !ok {
			continue
		}
		var rows []domain.Order
		for _, o := range live {
			if o.ProcessNodeID != nil && *o.ProcessNodeID == id {
				rows = append(rows, o)
			}
		}
		for _, o := range domain.SendBackOrders(rows, c.PairedCoreNode) {
			switch protocol.ChangeoverStartActionFor(o.Status) {
			case protocol.ChangeoverStartBlock:
				if o.Status == protocol.StatusFaulted {
					return nil, fmt.Errorf("cannot start changeover — %s", changeoverBlockerFor(c.PairedCoreNode, o.ID, o.Status))
				}
				return nil, fmt.Errorf("cannot start changeover — %s: SEND BACK in progress — start the changeover when it finishes",
					c.PairedCoreNode)
			case protocol.ChangeoverStartCancel:
				cancel = append(cancel, o.ID)
			}
		}
	}
	return cancel, nil
}

// sendBackRefusal is why the route did not act, in the operator's words.
func (e *Engine) sendBackRefusal(node *processes.Node, c sendBackCell, f domain.SendBackFacts, line domain.SendBack) error {
	switch {
	case f.Produce && domain.FedShape(f.Paired, f.PairedClaimed) && c.outside == nil:
		return fmt.Errorf("%s has no process node on %s", f.Paired, e.processName(node.ProcessID))
	case line.State == domain.SendBackWaiting:
		return errors.New("SEND BACK is already in progress on " + f.Paired)
	case line.State == domain.SendBackDisabled && f.SwapLive:
		return fmt.Errorf("%s: a swap is queued or in flight", node.Name)
	case line.State == domain.SendBackDisabled:
		return fmt.Errorf("%s: another order is placing at %s", node.Name, f.Paired)
	case f.ChangeoverArmed:
		return fmt.Errorf("%s: a changeover is armed", node.Name)
	}
	return fmt.Errorf("%s: nothing to send back", node.Name)
}

// CancelSendBack aborts SEND BACK's orders on the press at nodeID that have
// not moved (protocol.ChangeoverStartActionFor says Cancel), under the line's
// lock. A moving one finishes. The existing abort door would abort a moving
// one too, so this asks first.
func (e *Engine) CancelSendBack(nodeID int64) (int, error) {
	node, _, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return 0, err
	}
	if claim == nil {
		return 0, fmt.Errorf("node %s has no active claim", node.Name)
	}
	cellMu := e.primeNodeLock(claim)
	cellMu.Lock()
	defer cellMu.Unlock()
	c, err := e.sendBackCellOf(node, claim)
	if err != nil {
		return 0, err
	}
	if c.outside == nil {
		return 0, fmt.Errorf("%s has no process node on %s", claim.PairedCoreNode, e.processName(node.ProcessID))
	}
	rows, err := e.db.ListActiveOrdersByProcessNode(c.outside.ID)
	if err != nil {
		return 0, fmt.Errorf("orders of %s: %w", c.outside.Name, err)
	}
	n := 0
	for _, o := range domain.SendBackOrders(rows, claim.PairedCoreNode) {
		if protocol.ChangeoverStartActionFor(o.Status) != protocol.ChangeoverStartCancel {
			continue
		}
		if err := e.orderMgr.AbortOrderWithReason(o.ID, "SEND BACK cancelled at "+node.Name); err != nil {
			return n, fmt.Errorf("cancel order %d: %w", o.ID, err)
		}
		n++
	}
	return n, nil
}
