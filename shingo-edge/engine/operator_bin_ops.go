package engine

import (
	"fmt"
	"log"
	"slices"

	"shingo/protocol"
	edgeorders "shingoedge/orders"
	"shingoedge/store/orders"
)

// LoadBin marks a bin at a manual_swap node as loaded with the given manifest.
// Calls Core's HTTP API directly to set the manifest on the existing bin at
// that node. No transport order is created — the bin stays in place until a
// move order sends it to OutboundDestination.
func (e *Engine) LoadBin(nodeID int64, payloadCode string, uopCount int64, manifest []protocol.IngestManifestItem) error {
	node, _, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return err
	}
	if claim == nil {
		return fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.SwapMode != protocol.SwapModeManualSwap {
		return fmt.Errorf("node %s is not a manual_swap node", node.Name)
	}
	if len(manifest) == 0 {
		return fmt.Errorf("manifest is empty")
	}

	// Check that a bin is actually at this node and that it's empty.
	// Loading on top of an existing payload would silently overwrite the
	// manifest and double-trigger the side-cycle (bin already in flight to
	// outbound). The card stays clickable in stale views — server has to
	// refuse rather than rely on the UI gate.
	if e.coreClient.Available() {
		bins, _ := e.coreClient.FetchNodeBins([]string{node.CoreNodeName})
		if len(bins) == 0 || !bins[0].Occupied {
			return fmt.Errorf("no bin at node %s — request an empty bin first", node.Name)
		}
		if bins[0].PayloadCode != "" {
			return fmt.Errorf("bin at node %s already loaded with %s — wait for outbound move", node.Name, bins[0].PayloadCode)
		}
	}

	// Validate payload code against allowed list
	allowed := claim.AllowedPayloads()
	if payloadCode == "" && len(allowed) > 0 {
		payloadCode = allowed[0]
	}
	if payloadCode == "" {
		return fmt.Errorf("no payload code specified")
	}
	if !slices.Contains(allowed, payloadCode) {
		return fmt.Errorf("payload %q not in allowed list for node %s", payloadCode, node.Name)
	}


	if uopCount <= 0 {
		for _, item := range manifest {
			uopCount += item.Quantity
		}
	}

	// Load bin via direct HTTP to Core — synchronous, immediate feedback
	items := make([]ManifestItem, len(manifest))
	for i, m := range manifest {
		items[i] = ManifestItem{PartNumber: m.PartNumber, Quantity: m.Quantity, Description: m.Description}
	}
	loadResp, err := e.coreClient.LoadBin(&BinLoadRequest{
		NodeName:    node.CoreNodeName,
		PayloadCode: payloadCode,
		UOPCount:    uopCount,
		Manifest:    items,
	})
	if err != nil {
		return fmt.Errorf("load bin: %w", err)
	}

	// The operator's tap on LOAD is the explicit confirmation that the L1
	// retrieve_empty arrived and has been filled. Confirming the L1 here
	// transitions it delivered → confirmed, sends a delivery receipt to Core,
	// and emits the OrderCompleted event that handleLoaderEmptyInCompletion
	// is wired to — that handler creates the L2 (filled-bin → outbound) move
	// order and updates the loader's runtime. Pre-fix the L1 stayed at
	// `delivered` indefinitely (Core would auto-confirm on its side, but
	// Edge had no continuous status sync) and L2 was created here directly,
	// duplicating the side-cycle handler's responsibility.
	if l1ID, l1Confirmed := e.confirmLoaderL1OnLoad(nodeID, uopCount); l1Confirmed {
		log.Printf("bin_ops: confirmed L1 order %d on operator load at node %d", l1ID, nodeID)
		// Belt-and-suspenders: set active_bin_id directly from Core's LoadBin
		// response. handleLoaderEmptyInCompletion will also try to set it
		// from the L1 order's BinID, but if Core's order.delivered envelope
		// arrived without bin_id (multi-bin order, or pre-fix Core build)
		// the event handler ends up with nil. The LoadBin response is the
		// authoritative pointer at this exact moment.
		if loadResp != nil && loadResp.BinID > 0 {
			v := loadResp.BinID
			if err := e.db.SetProcessNodeActiveBinID(nodeID, &v); err != nil {
				log.Printf("bin_ops: set active_bin_id for node %d: %v", nodeID, err)
			}
		}
		// Flush trigger: bin-loader confirm is the produce/manual_swap
		// loader-side boundary at which the outgoing bin is "done" —
		// any accumulated deltas attributed to that bin should ship
		// before the new bin starts driving counts. Periodic 5s flush
		// would catch them eventually, but firing here makes the audit
		// trail align with the operator action.
		if e.inventoryDelta != nil {
			e.inventoryDelta.Flush()
		}
		return nil
	}

	// Fallback: no L1 in flight (e.g. operator loaded a bin that was placed
	// at the loader manually rather than via a retrieve_empty). Set runtime
	// and create L2 directly so the bin still gets dispatched to outbound.
	// active_bin_id comes from Core's LoadBin response — that's the
	// authoritative pointer to the bin physically at this slot, regardless
	// of whether any order was tracking it.
	claimID := claim.ID
	var activeBinID *int64
	if loadResp != nil && loadResp.BinID > 0 {
		v := loadResp.BinID
		activeBinID = &v
	}
	if err := e.db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, activeBinID, int(uopCount)); err != nil {
		log.Printf("bin_ops: set runtime for node %d: %v", nodeID, err)
	}
	if claim.OutboundDestination != "" {
		// L2 to OutboundDestination is unattended (supermarket node) — must
		// auto-confirm or it sticks at `delivered` forever. See the same
		// reasoning in handleLoaderEmptyInCompletion. Thread the operator-
		// selected payloadCode through so the order tile in operator-station
		// renders IN_TRANSIT against the correct payload card (claim's
		// primary payload would mis-bind on multi-payload loaders).
		order, err := e.orderMgr.CreateMoveOrderWithPayloadCode(&nodeID, 1, node.CoreNodeName, claim.OutboundDestination, payloadCode, true)
		if err != nil {
			log.Printf("manual_swap: move to outbound for node %s: %v", node.Name, err)
		} else {
			if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil); err != nil {
				log.Printf("bin_ops: update runtime orders for node %d: %v", nodeID, err)
			}
		}
	}

	return nil
}

// confirmLoaderL1OnLoad confirms the inbound retrieve_empty (L1) at this
// loader, treating the operator's LOAD tap as the receipt acknowledgement.
// Returns (orderID, true) when an L1 was actually confirmed; (0, false)
// otherwise (no delivered L1 found, or the confirm transition itself failed).
//
// Searches active orders for the node rather than trusting
// runtime.ActiveOrderID. The runtime pointer can drift (a prior fallback
// path that created an L2 directly will have overwritten it with the move
// order ID), so a direct query is the only reliable way to find the L1.
func (e *Engine) confirmLoaderL1OnLoad(nodeID int64, uopCount int64) (int64, bool) {
	active, err := e.db.ListActiveOrdersByProcessNodeAndType(nodeID, edgeorders.TypeRetrieve)
	if err != nil {
		log.Printf("bin_ops: list retrieve orders for node %d: %v", nodeID, err)
		return 0, false
	}
	var l1ID int64
	for _, o := range active {
		if o.RetrieveEmpty && o.Status == protocol.StatusDelivered {
			l1ID = o.ID
			break
		}
	}
	if l1ID == 0 {
		return 0, false
	}
	if err := e.orderMgr.ConfirmDelivery(l1ID, uopCount); err != nil {
		log.Printf("bin_ops: confirm L1 %d on load: %v", l1ID, err)
		return 0, false
	}
	return l1ID, true
}

// ClearBin clears the manifest on the bin at a manual_swap node, resetting it
// to empty. Used by unloaders after physical removal and for fixing mis-loads.
func (e *Engine) ClearBin(nodeID int64) error {
	node, _, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return err
	}
	if claim == nil {
		return fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.SwapMode != protocol.SwapModeManualSwap {
		return fmt.Errorf("node %s is not a manual_swap node", node.Name)
	}
	if err := e.coreClient.ClearBin(node.CoreNodeName); err != nil {
		return fmt.Errorf("clear bin: %w", err)
	}
	claimID := claim.ID
	if err := e.db.SetProcessNodeRuntime(nodeID, &claimID, 0); err != nil {
		log.Printf("bin_ops: set runtime for node %d: %v", nodeID, err)
		}
	return nil
}

// RequestEmptyBin delivers an empty bin to a produce node. Manual_swap and
// simple modes issue a single retrieve order; multi-step modes (single_robot,
// two_robot, two_robot_press_index, sequential) reuse the swap dispatch so
// the robot choreography is identical to a Finalize swap — empties move
// through the same multi-stop trip a full bin would. Returns the primary
// order; the second leg (R2) is tracked on the runtime row.
func (e *Engine) RequestEmptyBin(nodeID int64, payloadCode string) (*orders.Order, error) {
	node, runtime, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return nil, err
	}
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.Role != protocol.ClaimRoleProduce {
		return nil, fmt.Errorf("node %s: only produce nodes request empty bins", node.Name)
	}
	if ok, reason := e.CanAcceptOrders(nodeID); !ok {
		return nil, fmt.Errorf("node %s unavailable: %s", node.Name, reason)
	}

	// Validate payload code against allowed list
	if payloadCode == "" {
		return nil, fmt.Errorf("no payload code specified")
	}
	if !slices.Contains(claim.AllowedPayloads(), payloadCode) {
		return nil, fmt.Errorf("payload %q not in allowed list for node %s", payloadCode, node.Name)
	}

	autoConfirm := claim.AutoConfirm || e.cfg.Web.AutoConfirm

	// manual_swap claims (bin loaders/unloaders) require operator confirmation
	// after physically loading/unloading the bin. Auto-confirming here would
	// immediately fire L2/U2 (move back to supermarket) before the operator
	// has finished. claim.AutoConfirm is true on these claims (mandatory for
	// the robot-drop signal), but that flag means "robot confirms it dropped
	// the bin", not "operator confirmed they loaded parts". Override both
	// flags for manual_swap to match MaybeCreateLoaderEmptyIn / MaybeCreateUnloaderFullIn.
	skipAutoConfirm := false
	if claim.SwapMode == protocol.SwapModeManualSwap {
		autoConfirm = false
		skipAutoConfirm = true
	}

	// Multi-step swap modes reuse the same dispatch the consume side uses on
	// RequestNodeMaterial / produce uses on Finalize. Robots execute the same
	// choreography for empty and full bins; the order shape doesn't depend
	// on contents.
	if claim.SwapMode != "" && claim.SwapMode != protocol.SwapModeSimple && claim.SwapMode != protocol.SwapModeManualSwap {
		dispatch, err := BuildSwapDispatch(node, claim)
		if err != nil {
			return nil, err
		}
		if dispatch != nil {
			if dispatch.RequiresActiveSwapGuard {
				if err := e.guardNoActiveSwap(node, runtime, claim); err != nil {
					return nil, err
				}
			}
			orderA, err := e.dispatchComplexLeg(nodeID, 1, dispatch.StepsA, dispatch.DeliveryNodeA, dispatch.ProcessNode, dispatch.AutoConfirmA)
			if err != nil {
				return nil, err
			}
			var orderB *orders.Order
			if dispatch.StepsB != nil {
				orderB, err = e.dispatchComplexLeg(nodeID, 1, dispatch.StepsB, "", dispatch.ProcessNode, dispatch.AutoConfirmB)
				if err != nil {
					return nil, err
				}
			}
			var orderBID *int64
			if orderB != nil {
				orderBID = &orderB.ID
			}
			if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, orderBID); err != nil {
				log.Printf("bin_ops: update runtime orders for node %d: %v", nodeID, err)
			}
			if orderB != nil {
				// Return-error on failure: see comment in
				// operator_stations.go:LinkOrderSiblings call site.
				if err := e.db.LinkOrderSiblings(orderA.ID, orderB.ID); err != nil {
					return nil, fmt.Errorf("link order siblings %d↔%d: %w", orderA.ID, orderB.ID, err)
				}
			}
			return orderA, nil
		}
	}

	// Simple / manual_swap modes: single retrieve. Core queues if no empty is
	// immediately available.
	order, err := e.orderMgr.CreateRetrieveOrder(
		&nodeID, true, 1, node.CoreNodeName, "",
		"standard", payloadCode, autoConfirm, skipAutoConfirm,
	)
	if err != nil {
		return nil, err
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil); err != nil {
		log.Printf("bin_ops: update runtime orders for node %d: %v", nodeID, err)
	}
	return order, nil
}

// RequestFullBin requests a full bin of the given payload to be delivered to a
// manual_swap consume node. Core queues the order if no full bins of that
// payload are available. Unlike RequestEmptyBin, this does NOT check node occupancy
// — the unloader expects full bins to arrive.
func (e *Engine) RequestFullBin(nodeID int64, payloadCode string) (*orders.Order, error) {
	node, _, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return nil, err
	}
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.SwapMode != protocol.SwapModeManualSwap {
		return nil, fmt.Errorf("node %s is not a manual_swap node", node.Name)
	}
	if claim.Role != protocol.ClaimRoleConsume {
		return nil, fmt.Errorf("node %s: only consume nodes request full bins", node.Name)
	}
	if ok, reason := e.CanAcceptOrders(nodeID); !ok {
		return nil, fmt.Errorf("node %s unavailable: %s", node.Name, reason)
	}

	// Validate payload code against allowed list
	if payloadCode == "" {
		return nil, fmt.Errorf("no payload code specified")
	}
	if !slices.Contains(claim.AllowedPayloads(), payloadCode) {
		return nil, fmt.Errorf("payload %q not in allowed list for node %s", payloadCode, node.Name)
	}

	// Create retrieve order for a full bin — Core queues if none available.
	// Same override as RequestEmptyBin: manual_swap unloader requires operator
	// confirmation (U1 must not auto-confirm, or U2 fires before processing).
	autoConfirm := false
	order, err := e.orderMgr.CreateRetrieveOrder(
		&nodeID, false, 1, node.CoreNodeName, "",
		"standard", payloadCode, autoConfirm, true,
	)
	if err != nil {
		return nil, err
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil); err != nil {
		log.Printf("bin_ops: update runtime orders for node %d: %v", nodeID, err)
		}
	return order, nil
}
