// changeover_sequential_order.go — a sequential pair's changeover legs, in the
// order the release gate already requires.

package engine

import (
	"log"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/engine/changeover"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// ── THE PULLING SIDE'S LEG WAITS FOR THE OTHER SIDE'S ─────────────────────
//
// On a sequential pair the line pulls from one position, and the release of
// that position moves the line onto its partner, so the release gate
// (flipTargetReady) refuses it until the partner's changeover leg has
// delivered. The pulling side's leg used to be created at the start beside the
// partner's, and Core claims every pickup bin of a leg at dispatch: parked at
// its station wait, it hard-claimed the incoming part's mouth bin for a pickup
// it could not reach until the partner delivered. When the partner's own bin
// of that part sat behind that mouth bin, the partner queued on a dig Core
// would not plan, and neither moved (FINDINGS-ab-strand-press10-2026-10-20).
//
// So the pulling side's leg is created when the partner's leg delivers, the
// order the gate already enforces. Its
// robot would have stood at the gate until then anyway. Only the partner's
// delivery creates it, never its failure: a failed partner is recovered by a
// retried leg, and a pulling leg created before that retry would claim its
// bins ahead of it and could form the same cycle. The
// retried leg that delivers creates it then; a cancelled changeover takes both
// sides as it always has.
//
// The held task keeps the state it was created in, with no order, so it reads
// as a swap still to request. The leg is created from the partner's status
// change; an Edge that stops between recording that change and creating the
// leg leaves the task so, for the operator's REQUEST on the node.

// pullingSideHeld reports whether action is the pulling side of a sequential
// pair whose partner's leg, created by this plan, has not delivered. plan is
// the whole plan; the partner's leg is read after its action was applied.
func (e *Engine) pullingSideHeld(co *processes.Changeover, plan changeover.Plan, action changeover.NodeAction) bool {
	partner, ok := e.sequentialPartnerToHoldFor(action)
	if !ok {
		return false
	}
	partnerPlanned := false
	for _, a := range plan.Actions {
		if a.NodeID == partner.ID && a.Err == nil && a.SupplyOrder != nil &&
			a.Situation == string(SituationSwap) {
			partnerPlanned = true
		}
	}
	if !partnerPlanned {
		return false
	}
	task, err := e.db.GetChangeoverNodeTaskByNode(co.ID, partner.ID)
	if err != nil || task == nil || task.NextMaterialOrderID == nil {
		return false
	}
	o, err := e.db.GetOrder(*task.NextMaterialOrderID)
	return err == nil && o != nil && o.Status != orders.StatusDelivered && o.Status != orders.StatusConfirmed
}

// sequentialPartnerToHoldFor resolves the partner of a swap action on the
// pulling side of a sequential pair: the action's node pulls, its partner does
// not. ok=false for anything else.
func (e *Engine) sequentialPartnerToHoldFor(action changeover.NodeAction) (*processes.Node, bool) {
	if action.Err != nil || action.SupplyOrder == nil || action.Situation != string(SituationSwap) {
		return nil, false
	}
	node, err := e.db.GetProcessNode(action.NodeID)
	if err != nil || node == nil {
		return nil, false
	}
	claim := e.claimAtNode(node)
	if claim == nil || claim.SwapMode != protocol.SwapModeSequential || claim.PairedCoreNode == "" {
		return nil, false
	}
	partner := e.processNodeByCore(node.ProcessID, claim.PairedCoreNode)
	if partner == nil {
		return nil, false
	}
	own, oErr := e.db.GetProcessNodeRuntime(node.ID)
	other, pErr := e.db.GetProcessNodeRuntime(partner.ID)
	if oErr != nil || pErr != nil || own == nil || other == nil || !own.ActivePull || other.ActivePull {
		return nil, false
	}
	return partner, true
}

// createHeldPullingLeg creates the held pulling side's leg when its partner's
// changeover leg delivers (an EventOrderStatusChanged subscriber): its current
// next-material leg, so a retried partner's delivery counts and a failed one
// does not. The
// leg is planned again from the held task alone: its claims, its situation,
// the pulling bit as the start read it, and the two styles' tooling. The task
// is left alone unless it still has no order and is in the state the start
// created it in.
func (e *Engine) createHeldPullingLeg(changed OrderStatusChangedEvent) {
	switch protocol.Status(changed.NewStatus) {
	case orders.StatusDelivered, orders.StatusConfirmed:
	default:
		return
	}
	if changed.ProcessNodeID == nil {
		return
	}
	node, err := e.db.GetProcessNode(*changed.ProcessNodeID)
	if err != nil || node == nil {
		return
	}
	co, err := e.db.GetActiveProcessChangeover(node.ProcessID)
	if err != nil || co == nil {
		return
	}
	task, err := e.db.GetChangeoverNodeTaskByNode(co.ID, node.ID)
	if err != nil || task == nil || task.NextMaterialOrderID == nil || *task.NextMaterialOrderID != changed.OrderID {
		return
	}
	claim := e.claimAtNode(node)
	if claim == nil || claim.SwapMode != protocol.SwapModeSequential || claim.PairedCoreNode == "" {
		return
	}
	pulling := e.processNodeByCore(node.ProcessID, claim.PairedCoreNode)
	if pulling == nil {
		return
	}
	held, err := e.db.GetChangeoverNodeTaskByNode(co.ID, pulling.ID)
	if err != nil || held == nil || held.NextMaterialOrderID != nil || held.OldMaterialReleaseOrderID != nil ||
		held.Situation != string(SituationSwap) || held.State != domain.NodeTaskSwapRequired {
		return
	}
	action, ok := e.planHeldPullingLeg(co, held, pulling.CoreNodeName, node.CoreNodeName)
	if !ok {
		return
	}
	log.Printf("changeover: %s's leg is created now that %s's leg %d is %s", pulling.CoreNodeName,
		node.CoreNodeName, changed.OrderID, changed.NewStatus)
	e.applyNodeAction(held, action)
}

// planHeldPullingLeg plans the held task's action as the start would have.
func (e *Engine) planHeldPullingLeg(co *processes.Changeover, held *processes.NodeTask, pullingCore, partnerCore string) (changeover.NodeAction, bool) {
	if held.FromClaimID == nil || held.ToClaimID == nil || co.FromStyleID == nil {
		return changeover.NodeAction{}, false
	}
	from, fErr := e.db.GetStyleNodeClaim(*held.FromClaimID)
	to, tErr := e.db.GetStyleNodeClaim(*held.ToClaimID)
	fromClaims, fcErr := e.db.ListStyleNodeClaims(*co.FromStyleID)
	toClaims, tcErr := e.db.ListStyleNodeClaims(co.ToStyleID)
	nodes, nErr := e.db.ListProcessNodesByProcess(co.ProcessID)
	if fErr != nil || tErr != nil || fcErr != nil || tcErr != nil || nErr != nil || from == nil || to == nil {
		log.Printf("changeover: %s's held leg cannot be planned: the claims or nodes cannot be read", pullingCore)
		return changeover.NodeAction{}, false
	}
	diff := ChangeoverNodeDiff{CoreNodeName: pullingCore, Situation: SituationSwap, FromClaim: from, ToClaim: to}
	plan := BuildChangeoverPlan([]ChangeoverNodeDiff{diff}, nodes, e.cfg.Web.AutoConfirm,
		map[string]bool{pullingCore: true, partnerCore: false}, planToolingChangeover(fromClaims, toClaims))
	if len(plan.Actions) != 1 || plan.Actions[0].Err != nil {
		log.Printf("changeover: %s's held leg did not plan: %+v", pullingCore, plan.Actions)
		return changeover.NodeAction{}, false
	}
	return plan.Actions[0], true
}

// processNodeByCore is the process's node named coreNode, nil when none.
func (e *Engine) processNodeByCore(processID int64, coreNode string) *processes.Node {
	nodes, err := e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return nil
	}
	if n := findNodeByCoreName(nodes, coreNode); n != nil {
		cp := *n
		return &cp
	}
	return nil
}
