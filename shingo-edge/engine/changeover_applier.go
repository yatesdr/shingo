package engine

import (
	"log"

	"shingoedge/domain"
	"shingoedge/engine/changeover"
	ordermgr "shingoedge/orders"
	"shingoedge/store/processes"
)

// applyChangeoverPlan creates orders for each NodeAction in the plan, links
// them to their changeover node tasks, and advances the task state.
//
// Error handling policy: log and continue. A failure on one node must not
// abort the rest of the changeover. See 2567plandiscussion.md.
func (e *Engine) applyChangeoverPlan(co *processes.Changeover, plan changeover.Plan) {
	for _, action := range plan.Actions {
		nodeTask, err := e.db.GetChangeoverNodeTaskByNode(co.ID, action.NodeID)
		if err != nil {
			log.Printf("changeover: cannot find node task for %s: %v", action.NodeName, err)
			continue
		}
		if action.Err != nil {
			log.Printf("changeover: auto-create orders for %s (%s): %v — operator must handle manually",
				action.NodeName, action.Situation, action.Err)
			if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, domain.NodeTaskError); err != nil {
				log.Printf("changeover: update node task %d state to error: %v", nodeTask.ID, err)
			}
			continue
		}
		e.applyNodeAction(nodeTask, action)
	}
}

func (e *Engine) applyNodeAction(nodeTask *processes.NodeTask, action changeover.NodeAction) {
	nodeID := action.NodeID

	// THE CHANGEOVER ALREADY HAS AN EPISODE AND ITS ORDERS DID NOT CARRY IT.
	// openChangeoverEpisode mints an origin and stamps it back onto the
	// changeover row, but the legs this applier creates were built through the
	// unattributed constructors — so every changeover swap reached Core with no
	// origin and landed as an orphan, and the episode it belonged to sat open
	// with no children. That also fed reconcileChildlessEpisodes, which closes
	// childless episodes as unattributed: a live changeover was eligible for it.
	//
	// Read once per action rather than per leg, so both legs of a swap take the
	// SAME origin — one demand served by two rows, which is what
	// CreateComplexOrderSiblingWithOrigin documents.
	origin := e.changeoverOrigin(nodeTask.ProcessChangeoverID)

	// BOTH UUIDs BEFORE EITHER CREATE — the same fix as applyProducePlan, and
	// this path needed it more. The supply went in with siblingUUID == "",
	// then the evac was stamped with a uuid READ BACK from the database, so a
	// swap's pairing depended on a refetch succeeding and on the supply being
	// created first. SYNTH-round2 found the changeover path creates the filler
	// first while the produce path creates the supply first, and once both
	// legs can be held no single ordering is safe.
	//
	// Pre-minting removes the refetch AND the ordering dependency: neither leg
	// is ever unpaired. The read-back-failure branch below is gone with it —
	// it existed only because the uuid was not knowable in advance.
	//
	// Creation ORDER is unchanged: the outbox drains strictly ORDER BY id, so
	// whichever leg is created first is the one Core is asked for first. That
	// is a dispatch-sequencing fact, not a pairing one.
	//
	// ONLY COMPLEX LEGS GET ONE, because only ComplexOrderRequest carries
	// SiblingOrderUUID on the wire — a retrieve leg is structurally unpairable
	// at Core. The old read-back did not know that and would happily stamp an
	// evac with a retrieve's uuid, producing exactly the ASYMMETRIC link
	// swap_hold rejects (it checks sib.SiblingOrderUUID == order.EdgeUUID).
	// Leaving it blank there fails open cleanly instead. Local pairing is
	// unaffected either way: LinkOrderSiblings below works on row ids, and it
	// is what supply_bin_guard and ComputeSwapReady read.
	supplyUUID, evacUUID := mintPairableLegUUID(action.SupplyOrder), mintPairableLegUUID(action.EvacOrder)

	var supplyID, evacID *int64
	if action.SupplyOrder != nil {
		id, err := e.createPlannedOrder(nodeID, action.SupplyOrder, evacUUID, supplyUUID, origin)
		if err != nil {
			log.Printf("changeover: auto-create orders for %s (%s): create supply order: %v — operator must handle manually",
				action.NodeName, action.Situation, err)
			if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, domain.NodeTaskError); err != nil {
				log.Printf("changeover: update node task %d state to error: %v", nodeTask.ID, err)
			}
			return
		}
		supplyID = &id
	}
	if action.EvacOrder != nil {
		id, err := e.createPlannedOrder(nodeID, action.EvacOrder, supplyUUID, evacUUID, origin)
		if err != nil {
			log.Printf("changeover: auto-create orders for %s (%s): create evac order: %v — operator must handle manually",
				action.NodeName, action.Situation, err)
			if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, domain.NodeTaskError); err != nil {
				log.Printf("changeover: update node task %d state to error: %v", nodeTask.ID, err)
			}
			return
		}
		evacID = &id
	}

	if supplyID != nil || evacID != nil {
		if err := e.db.LinkChangeoverNodeOrders(nodeTask.ID, supplyID, evacID); err != nil {
			log.Printf("changeover: link orders for node task %d: %v", nodeTask.ID, err)
		}
	}
	// Durable supply ↔ evac sibling linkage for two-robot swap pairs.
	// Mirrors operator_stations.go:134 (the operator-initiated path) —
	// without it, isSupplyOrderInTwoRobotSwap can't identify the supply
	// leg via SiblingOrderID, and the supply_bin_guard at
	// operator_release.go:246-256 misses. Plant 2026-05-11 (SNF2 ALN_001):
	// changeover-driven two-robot swap's supply bin (3600 parts) was
	// wiped on a per-order admin release because the guard couldn't
	// identify it as supply without the sibling pointer.
	//
	// Same fingerprint as the 2026-04-23 ALN_002 incident
	// (operator_release.go:497-498), fixed for the operator-initiated
	// path but never backported here.
	// LinkOrderSiblings is log-and-continue here (unlike the three
	// operator-initiated sites in operator_stations.go / operator_bin_ops.go
	// / operator_produce.go which return-error). Rationale:
	//   - Orders are already persisted by createPlannedOrder above; we'd
	//     need a rollback to abort cleanly.
	//   - applyChangeoverPlan iterates per-node and a single node's
	//     failure must not abort the rest of the plan (see comment at
	//     applyChangeoverPlan: "log and continue").
	// Residual risk: a silent linkage failure here leaves a changeover
	// two-robot pair without sibling pointers, which makes
	// ComputeSwapReady return false (operator gets WAITING FOR OTHER
	// ROBOT with no escape). SHINGO_TODO.md "Residual risk" entry tracks
	// the mitigation (on-read repair or startup audit).
	if supplyID != nil && evacID != nil {
		if err := e.db.LinkOrderSiblings(*supplyID, *evacID); err != nil {
			log.Printf("changeover: link order siblings %d↔%d for node task %d: %v",
				*supplyID, *evacID, nodeTask.ID, err)
		}
	}
	// Point the node's runtime slots at THIS cycle's legs. Every
	// operator-initiated pair-creation site does this (operator_stations.go,
	// operator_bin_ops.go, operator_produce.go, operator_changeover_start.go);
	// this one never has, and the omission is what takes the RELEASE button
	// away from a changeover swap.
	//
	// Springfield SNF2 / ALN_001, 2026-08-03: the 21:19 changeover created
	// 3993 (supply) + 3994 (evac) and linked them correctly, but the runtime
	// row still named the 18:49 pair. ComputeSwapReady resolves the evac from
	// StagedOrderID, read the previous cycle's evac — confirmed 2½ hours
	// earlier — and returned false, so the operator watched both legs sit
	// staged for 13 minutes with no button and recovered through the Edge
	// admin UI. The task pointer held the right answer the whole time, but
	// ResolveSwapPair only reaches its task fallback when BOTH runtime
	// pointers are nil, so a stale pointer shadows it rather than losing to it.
	//
	// active = supply, staged = evac — the positional mapping ResolveSwapPair
	// and the four operator sites already share. Writing nil for a leg the
	// plan didn't create is deliberate: a one-legged changeover action must
	// clear the other slot, not inherit last cycle's order there.
	//
	// Log-and-continue, matching LinkOrderSiblings above and for the same
	// reason: the orders are already persisted and one node's failure must
	// not abort the rest of the plan.
	if supplyID != nil || evacID != nil {
		if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, supplyID, evacID); err != nil {
			log.Printf("changeover: update runtime orders for node %d (supply=%v evac=%v): %v",
				nodeID, supplyID, evacID, err)
		}
	}
	if action.NextState != "" {
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, action.NextState); err != nil {
			log.Printf("changeover: update node task %d state to %s: %v", nodeTask.ID, action.NextState, err)
		}
	}

	logChangeoverAction(action, supplyID, evacID)
}

// mintPairableLegUUID pre-mints a uuid for a leg that can actually carry a
// sibling pointer to Core, and returns "" for one that cannot (nil, or a
// retrieve spec — see the call site).
func mintPairableLegUUID(spec *changeover.OrderSpec) string {
	if spec == nil || spec.Complex == nil {
		return ""
	}
	return ordermgr.NewOrderUUID()
}

// createPlannedOrder creates one leg. orderUUID is the pre-minted uuid for this
// leg (empty for a single-order spec, which mints its own); siblingUUID is its
// partner's.
func (e *Engine) createPlannedOrder(nodeID int64, spec *changeover.OrderSpec, siblingUUID, orderUUID string, origin ordermgr.Origin) (int64, error) {
	switch {
	case spec.Complex != nil:
		return e.createComplexFromSpec(nodeID, spec.Complex, siblingUUID, orderUUID, origin)
	case spec.Retrieve != nil:
		return e.createRetrieveFromSpec(nodeID, spec.Retrieve, origin)
	}
	return 0, nil
}

func (e *Engine) createComplexFromSpec(nodeID int64, c *changeover.ComplexOrderSpec, siblingUUID, orderUUID string, origin ordermgr.Origin) (int64, error) {
	o, err := e.orderMgr.CreateComplexOrderPaired(&nodeID, 1, c.DeliveryNode, c.ProcessNode, c.Steps, c.AutoConfirm, c.PayloadCode, siblingUUID, orderUUID, origin)
	if err != nil {
		return 0, err
	}
	return o.ID, nil
}

func (e *Engine) createRetrieveFromSpec(nodeID int64, r *changeover.RetrieveOrderSpec, origin ordermgr.Origin) (int64, error) {
	o, err := e.orderMgr.CreateRetrieveOrder(&nodeID, r.RetrieveEmpty, 1, r.DeliveryNode, r.SourceNode, r.StagingNode, r.LoadType, r.PayloadCode, r.AutoConfirm, false, origin)
	if err != nil {
		return 0, err
	}
	return o.ID, nil
}

func logChangeoverAction(action changeover.NodeAction, supplyID, evacID *int64) {
	switch action.LogTag {
	case "swap":
		log.Printf("changeover: swap node %s — supply=%d (staging), evac=%d (swap w/ wait)", action.NodeName, derefID(supplyID), derefID(evacID))
	case "evacuate":
		log.Printf("changeover: evacuate node %s — supply=%d (staging), evac=%d (evacuate w/ 2 waits)", action.NodeName, derefID(supplyID), derefID(evacID))
	case "drop":
		log.Printf("changeover: drop node %s — evac=%d (single-robot release w/ staged wait)", action.NodeName, derefID(evacID))
	case "keep_staged_split":
		log.Printf("changeover: keep-staged split node %s — supply=%d (deliver w/ wait), evac=%d (evac w/ wait)", action.NodeName, derefID(supplyID), derefID(evacID))
	case "keep_staged_combined":
		log.Printf("changeover: keep-staged combined node %s — supply=%d (combined w/ wait), evac=%d (evac w/ wait)", action.NodeName, derefID(supplyID), derefID(evacID))
	case "fallback_staging":
		log.Printf("changeover: FALLBACK staging node %s — supply=%d (stage-only; the mode's swap builder was bypassed because InboundStaging was blank)", action.NodeName, derefID(supplyID))
	case "fallback_retrieve":
		log.Printf("changeover: FALLBACK retrieve node %s — supply=%d (retrieve-only; the mode's swap builder was bypassed because InboundStaging was blank)", action.NodeName, derefID(supplyID))
	case "add":
		log.Printf("changeover: add node %s — supply=%d (direct deliver; node is new in this style, no evacuation)", action.NodeName, derefID(supplyID))
	}
}

func derefID(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}

// changeoverOrigin returns the demand episode a changeover's orders belong to.
//
// ONE SPELLING, FIVE SITES. These three lines were written once, in
// applyNodeAction, and the four operator-driven changeover entry points
// (StageNodeChangeoverMaterial, EvacuateNode, DeliverNewMaterialForChangeover,
// and the release path) each created their orders through the unattributed
// constructors instead. So an AUTO-planned changeover leg carried its episode
// and an OPERATOR-driven one for the same changeover did not — the same
// changeover, attributed or orphaned depending on which button produced it.
//
// openChangeoverEpisode mints the origin and stamps it onto the changeover row
// precisely so any site can read it back. This is that read, spelled once,
// because a change to it now has to land in five places — which is the rule
// this collapse exists under.
//
// A READ FAILURE DOES NOT BLOCK THE CHANGEOVER. Attribution never blocks
// transport; the leg is created carrying nothing and Core classifies it, which
// is what happened at every one of these sites before.
func (e *Engine) changeoverOrigin(processChangeoverID int64) ordermgr.Origin {
	originID, err := e.db.GetChangeoverOriginID(processChangeoverID)
	if err != nil {
		e.logFn("changeover: read episode for changeover %d: %v — orders will be unattributed",
			processChangeoverID, err)
		return ordermgr.Origin{}
	}
	if originID == "" {
		// The changeover has no episode. Not an error and not no_demand: the
		// episode exists for every changeover this build opens, so an empty one
		// here means an older row, and Core's orphan class is the true answer.
		return ordermgr.Origin{}
	}
	return ordermgr.Attached(originID)
}
