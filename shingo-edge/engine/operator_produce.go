package engine

import (
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// RequestProduceSwap (formerly FinalizeProduceNode — the /finalize route
// keeps its name) dispatches the order(s) to remove the filled bin and bring
// the next empty. Builds a ProducePlan (pure validation + dispatch shape)
// and then applies it. Swap-mode dispatch shape is shared with consume
// via SwapDispatch — the robot doesn't care whether the bin is filling
// or emptying, the choreography is the same.
//
// Fix D renamed the tap: on two-robot modes this call only REQUESTS robots.
// The manifest snapshot and the count reset — the actual "finalize" — happen
// at the RELEASE tap (produceIngestAtRelease), because every part pressed
// between this call and the physical swap still lands in the departing bin.
// Snapshotting here understated the shipped tote and pre-credited the next
// one, and calling robots early (desirable) widened that window on purpose.
// Non-two-robot modes have no staged release step, so request time IS
// release time for them and the paperwork stays here.
func (e *Engine) RequestProduceSwap(nodeID int64) (*NodeOrderResult, error) {
	node, runtime, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return nil, err
	}

	plan, err := BuildProducePlan(node, runtime, claim, time.Now())
	if err != nil {
		return nil, err
	}

	// Bug 3 guard: refuse to start a second swap on top of an in-flight one.
	// Runs BEFORE setProduceManifest so we don't burn an ingest order on a
	// node that's about to be rejected. Edge-runtime-only — Core anomalies
	// don't shut down the line.
	if plan.Dispatch != nil && plan.Dispatch.RequiresActiveSwapGuard {
		if err := e.guardNoActiveSwap(node, runtime, claim); err != nil {
			return nil, err
		}
	}

	return e.applyProducePlan(node, runtime, claim, plan)
}

// applyProducePlan is the impure half of the produce-finalize pipeline:
// it manifests the filled bin, dispatches the planned complex orders,
// resets node UOP, and re-reads the resulting orders. Direction-specific
// glue around the shared SwapDispatch.
func (e *Engine) applyProducePlan(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, plan *ProducePlan) (*NodeOrderResult, error) {
	nodeID := node.ID

	// Fix D: two-robot modes DEFER the paperwork (manifest ingest + count
	// reset) to the release tap — the bin keeps filling until the robots are
	// actually sent in, so the release-time count is the true shipped count.
	// The runtime ORDER pointers still stamp below either way (release
	// resolution and the swap_ready gate depend on them).
	deferPaperwork := claim.SwapMode.IsTwoRobot()
	if !deferPaperwork {
		if err := e.dispatchProduceIngest(node, claim, plan); err != nil {
			return nil, err
		}
	}

	// Produce always has a swap mode now (BuildProducePlan errors otherwise), so
	// Dispatch is always set.
	dispatch := plan.Dispatch
	orderA, err := e.dispatchComplexLeg(nodeID, 1, dispatch.StepsA, dispatch.DeliveryNodeA, dispatch.ProcessNode, dispatch.AutoConfirmA, "")
	if err != nil {
		return nil, err
	}

	var orderB *orders.Order
	if dispatch.StepsB != nil {
		// Removal/evac leg carries the supply leg's UUID so Core can pair
		// them at intake (before this leg's dispatch claims the line bin).
		orderB, err = e.dispatchComplexLeg(nodeID, 1, dispatch.StepsB, "", dispatch.ProcessNode, dispatch.AutoConfirmB, orderA.UUID)
		if err != nil {
			return nil, err
		}
	}

	var orderBID *int64
	if orderB != nil {
		orderBID = &orderB.ID
	}
	e.resetProduceRuntime(nodeID, runtime, &orderA.ID, orderBID, !deferPaperwork)
	if orderB != nil {
		// Return-error on failure: see comment in
		// operator_stations.go:LinkOrderSiblings call site.
		if err := e.db.LinkOrderSiblings(orderA.ID, orderB.ID); err != nil {
			return nil, fmt.Errorf("link order siblings %d↔%d: %w", orderA.ID, orderB.ID, err)
		}
	}

	orderA, err = e.refreshOrder(orderA.ID)
	if err != nil {
		return nil, err
	}
	if orderB != nil {
		orderB, err = e.refreshOrder(orderB.ID)
		if err != nil {
			return nil, err
		}
	}

	if orderB == nil {
		return &NodeOrderResult{CycleMode: dispatch.CycleMode, Order: orderA, ProcessNodeID: nodeID}, nil
	}
	return &NodeOrderResult{CycleMode: dispatch.CycleMode, OrderA: orderA, OrderB: orderB, ProcessNodeID: nodeID}, nil
}

// dispatchProduceIngest stamps Core's bin manifest with the produced count.
// Produce is always manifest-only: the swap's complex order carries the bin, so
// a local ingest order would only be a phantom for the abort fan-out to cancel
// (the "not_found" bug). Fire-and-forget via QueueIngestManifest — no local
// order, no reply on success. Non-two-robot request-time path only; two-robot
// modes stamp at release via produceIngestAtRelease.
func (e *Engine) dispatchProduceIngest(node *processes.Node, claim *processes.NodeClaim, plan *ProducePlan) error {
	return e.orderMgr.QueueIngestManifest(
		claim.PayloadCode,
		"", // bin label resolved by core from node contents
		0,  // bin id likewise
		node.CoreNodeName,
		plan.Manifest[0].Quantity,
		plan.Manifest,
		plan.ProducedAtRFC3339,
	)
}

// produceIngestAtRelease is Fix D's deferred paperwork: at the RELEASE tap of
// a two-robot produce swap, snapshot the manifest from the LIVE count — every
// part pressed since the request landed in the departing bin, and the count
// kept ticking because the request-time reset was skipped — then clear the
// runtime so post-release ticks hold and replay onto the NEXT bin only.
//
// Enqueue ordering is the contract with Core: the ingest is queued BEFORE the
// two OrderRelease envelopes (same goroutine, sequential outbox inserts, and
// the outbox drains strictly ORDER BY id), so Core applies the manifest
// before any release-side manifest action. The ingest pins the departing bin
// by runtime.ActiveBinID — node-based resolution could land on the freshly
// indexed tote by the time Core processes a press-index release.
//
// Zero/negative count skips the stamp (nothing pressed, or a retry after a
// prior successful stamp+clear — the guard is what makes the release click
// idempotent). Ingest enqueue failure fails the release CLOSED: a full bin
// must not leave un-manifested when the operator can just click again.
func (e *Engine) produceIngestAtRelease(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim) error {
	if claim.Role != protocol.ClaimRoleProduce {
		return nil
	}
	if runtime == nil || runtime.RemainingUOPCached <= 0 {
		e.logFn("produce release: node %s remaining=%d — no release-time manifest to stamp",
			node.Name, runtimeRemaining(runtime))
		return nil
	}
	qty := int64(runtime.RemainingUOPCached)
	var binID int64
	if runtime.ActiveBinID != nil {
		binID = *runtime.ActiveBinID
	}
	manifest := []protocol.IngestManifestItem{{
		PartNumber:  claim.PayloadCode,
		Quantity:    qty,
		Description: claim.PayloadCode,
	}}
	if err := e.orderMgr.QueueIngestManifest(
		claim.PayloadCode, "", binID, node.CoreNodeName, qty, manifest,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("queue release-time ingest for node %s: %w", node.Name, err)
	}
	// Snapshot taken — the count now belongs to the departing bin. Clear
	// active + zero so the hold-and-replay window starts HERE, not at the
	// request. Log-only on failure: the manifest already shipped, and the
	// stale count would only re-stamp on a retry (Core's SetForProduction
	// is idempotent).
	if e.inventoryDelta != nil {
		var claimID *int64
		if runtime.ActiveClaimID != nil {
			claimID = runtime.ActiveClaimID
		}
		if err := e.inventoryDelta.ClearActiveAndReset(node.ID, claimID); err != nil {
			log.Printf("produce release: clear active bin for node %d: %v", node.ID, err)
		}
	}
	return nil
}

// runtimeRemaining is a nil-safe read for log lines.
func runtimeRemaining(runtime *processes.RuntimeState) int {
	if runtime == nil {
		return 0
	}
	return runtime.RemainingUOPCached
}

// dispatchComplexLeg issues a single complex order with the right auto-
// confirm wiring. Direction-agnostic — produce passes quantity=1 (the
// bin), consume passes the operator-requested quantity. processNodeName
// is the line node both legs of a swap belong to (= claim.CoreNodeName);
// threaded into ComplexOrderRequest.ProcessNode so Core picks the line
// bin for order.BinID.
func (e *Engine) dispatchComplexLeg(nodeID int64, quantity int64, steps []protocol.ComplexOrderStep, deliveryNode, processNodeName string, autoConfirm bool, siblingUUID string) (*orders.Order, error) {
	dn := deliveryNode
	if autoConfirm {
		dn = ""
	}
	return e.orderMgr.CreateComplexOrderSibling(&nodeID, quantity, dn, processNodeName, steps, autoConfirm, "", siblingUUID)
}

// resetProduceRuntime stamps the dispatched legs on the runtime and, when
// clearCounts is set, resets the counting state. clearCounts=true is the
// non-two-robot path (request time IS release time there): clear
// active_bin_id, which puts the tick path into hold mode — parts produced
// before the next empty bin lands accumulate in pending_uop_delta and replay
// onto the new bin when its OrderDelivered seeds active_bin_id + epoch.
// clearCounts=false is the two-robot request (Fix D): the bin is still under
// the press until the RELEASE tap, so the count keeps ticking on it and
// produceIngestAtRelease owns the clear.
//
// Errors are logged only — the order(s) already shipped, so failing
// here would leave the caller with no actionable recovery.
func (e *Engine) resetProduceRuntime(nodeID int64, runtime *processes.RuntimeState, activeID, stagedID *int64, clearCounts bool) {
	if clearCounts && e.inventoryDelta != nil {
		// Clear the active bin (slot is empty after finalize → ticks hold)
		// and zero the count (the next empty bin starts at 0). Preserves
		// the claim so the next delivery binds against it.
		var claimID *int64
		if runtime != nil {
			claimID = runtime.ActiveClaimID
		}
		if err := e.inventoryDelta.ClearActiveAndReset(nodeID, claimID); err != nil {
			log.Printf("produce: clear active bin for node %d: %v", nodeID, err)
		}
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, activeID, stagedID); err != nil {
		log.Printf("produce: update runtime orders for node %d: %v", nodeID, err)
	}
}

// refreshOrder re-reads an order after the runtime-orders write so the
// caller sees the updated process_node_id linkage in the response.
func (e *Engine) refreshOrder(orderID int64) (*orders.Order, error) {
	o, err := e.db.GetOrder(orderID)
	if err != nil {
		log.Printf("produce: re-read order %d after runtime update: %v", orderID, err)
		return nil, fmt.Errorf("re-read order %d: %w", orderID, err)
	}
	return o, nil
}
