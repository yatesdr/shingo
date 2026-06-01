package engine

import (
	"fmt"
	"log"
	"slices"

	"shingo/protocol"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// loadablePayloads returns the payload codes an operator may load or request at
// this manual_swap node: every payload configured on the physical loader across
// all styles and all cells sharing it (PayloadsForLoader's `all`). It is
// deliberately NOT gated by which style is active — a loader responds to what is
// called for, not to the running style:
//
//   - A normal loader fills system demand (UOP-threshold / default replenish)
//     and doesn't care whether a style is active or inactive; a shared loader
//     (e.g. SNF2 + SNF3) must accept either cell's payload.
//   - A transitional loader is operator-driven and stages ahead for upcoming
//     styles.
//
// The active-vs-all distinction is purely a board *display* concern: a
// transitional board defaults to the active union and toggles to "show all".
// The server gate only ensures the payload is one this loader is physically
// configured for — so it's the same `all` union for both loader types.
//
// Fails open to this node's active-claim list if the union read errors or comes
// back empty, so a DB hiccup can't strand the operator with zero loadable cards.
func (e *Engine) loadablePayloads(node *processes.Node, claim *processes.NodeClaim) []string {
	_, all, _, err := processes.PayloadsForLoader(e.db.DB, node.CoreNodeName, claim.Role)
	if err != nil {
		e.logFn("loader: payload union read for %s failed, using active claim: %v", node.CoreNodeName, err)
		return claim.AllowedPayloads()
	}
	if len(all) == 0 {
		return claim.AllowedPayloads()
	}
	return all
}

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

	// Validate payload code against the loader-wide loadable set (see
	// loadablePayloads) — the loader fills what the system/operator calls for
	// across every cell sharing it, not just this node's running style.
	allowed := e.loadablePayloads(node, claim)
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
	if l1ID, l1Confirmed := e.confirmLoaderL1OnLoad(node.CoreNodeName, uopCount); l1Confirmed {
		log.Printf("bin_ops: confirmed L1 order %d on operator load at node %d", l1ID, nodeID)
		// Belt-and-suspenders: set active_bin_id directly from Core's LoadBin
		// response. handleLoaderEmptyInCompletion will also try to set it
		// from the L1 order's BinID, but if Core's order.delivered envelope
		// arrived without bin_id (multi-bin order, or pre-fix Core build)
		// the event handler ends up with nil. The LoadBin response is the
		// authoritative pointer at this exact moment.
		if loadResp != nil && loadResp.BinID > 0 {
			if e.inventoryDelta != nil {
				if err := e.inventoryDelta.BindActiveBin(nodeID, loadResp.BinID); err != nil {
					log.Printf("bin_ops: bind active bin for node %d: %v", nodeID, err)
				}
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
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.ManualLoad(nodeID, &claimID, activeBinID, int(uopCount)); err != nil {
			log.Printf("bin_ops: set runtime for node %d: %v", nodeID, err)
		}
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
// Looks the L1 up by the loader's CORE NODE (delivery_node), NOT by the
// process_node the operator loaded at. On a loader shared across styles/cells
// one core node has many process_node rows, and the staged empty may be tracked
// against a different row than the operator's station — a process-node-scoped
// query then misses it, LoadBin falls through to its direct-L2 fallback, and the
// L1 orphans at `delivered` while the full bin still ships (plant 2026-06-01).
// A core node is one physical slot, so there is at most one delivered empty
// there to confirm. Querying delivery_node also sidesteps a drifted
// runtime.ActiveOrderID (a prior fallback overwrites it with the L2 move ID).
func (e *Engine) confirmLoaderL1OnLoad(coreNodeName string, uopCount int64) (int64, bool) {
	delivered, err := e.db.ListDeliveredRetrieveByDeliveryNode(coreNodeName, true)
	if err != nil {
		log.Printf("bin_ops: list delivered empties for node %s: %v", coreNodeName, err)
		return 0, false
	}
	if len(delivered) == 0 {
		return 0, false
	}
	l1ID := delivered[0].ID // oldest delivered empty at this slot
	if err := e.orderMgr.ConfirmDelivery(l1ID, uopCount); err != nil {
		log.Printf("bin_ops: confirm L1 %d on load: %v", l1ID, err)
		return 0, false
	}
	return l1ID, true
}

// ClearBin clears the manifest on the bin at a manual_swap node and, for
// consume-role nodes, confirms the active U1 retrieve_full so the side-cycle
// progresses to U2. Used by unloaders after physical removal and for fixing
// mis-loads.
//
// Symmetric to LoadBin's confirmLoaderL1OnLoad: the operator's tap IS the
// explicit confirmation that the inbound order completed. Without confirming,
// the U1 sits at `delivered` forever and handleUnloaderFullInCompletion never
// fires — bin cleared physically but order lifecycle stuck. Mis-load case (no
// active U1) is preserved: the confirm helper returns ok=false and ClearBin
// proceeds with the manifest clear only.
//
// Post-clear, if the claim has AutoPush enabled, MaybePushUnloader is called
// to fire the next U1 retrieve_full immediately — push-driven unloaders
// don't wait for a kanban demand signal, so the clear-event IS the next-bin
// trigger.
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
	// Confirm U1 BEFORE the bin clear so handleUnloaderFullInCompletion
	// sees a coherent (still-loaded) bin and the U2 it creates carries the
	// right PayloadCode from the U1 order row. The bin manifest clear that
	// follows is purely physical state — order-lifecycle progression is
	// owned by the confirm + OrderCompleted event handler.
	if claim.Role == protocol.ClaimRoleConsume {
		if u1ID, ok := e.confirmUnloaderU1OnClear(node.CoreNodeName); ok {
			log.Printf("bin_ops: confirmed U1 order %d on operator clear at node %s", u1ID, node.CoreNodeName)
		}
	}
	if err := e.coreClient.ClearBin(node.CoreNodeName); err != nil {
		return fmt.Errorf("clear bin: %w", err)
	}
	claimID := claim.ID
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.SetClaimAndCount(nodeID, &claimID, 0); err != nil {
			log.Printf("bin_ops: set runtime for node %d: %v", nodeID, err)
		}
	}
	// Push-driven unloader: bin just left the window, fire the next pull.
	// Gated inside MaybePushUnloader so non-push claims are no-ops.
	if claim.Role == protocol.ClaimRoleConsume && claim.AutoPush {
		e.MaybePushUnloader(nodeID)
	}
	// Push-driven loader (transitional): the operator cleared the window, so
	// stage the next empty. Gated inside MaybePushLoader on transitional.
	if claim.Role == protocol.ClaimRoleProduce {
		e.MaybePushLoader(nodeID)
	}
	return nil
}

// confirmUnloaderU1OnClear confirms the inbound retrieve_full (U1) at this
// unloader, treating the operator's CLEAR tap as the receipt acknowledgement.
// Returns (orderID, true) when a U1 was actually confirmed; (0, false)
// otherwise (no delivered U1 found, or the confirm transition itself failed).
//
// Mirror of confirmLoaderL1OnLoad, including the core-node lookup: the
// discriminator vs L1 is RetrieveEmpty (U1 = retrieve with RetrieveEmpty=false).
// Looks up by the unloader's CORE NODE so a shared unloader's U1 is found even
// when it's tracked against a sibling process_node (same orphan-at-delivered bug
// as the loader side). Passes 0 for finalCount: the bin is empty after the
// operator processes the contents, matching the inventoryDelta zero-out below.
func (e *Engine) confirmUnloaderU1OnClear(coreNodeName string) (int64, bool) {
	delivered, err := e.db.ListDeliveredRetrieveByDeliveryNode(coreNodeName, false)
	if err != nil {
		log.Printf("bin_ops: list delivered full-ins for node %s: %v", coreNodeName, err)
		return 0, false
	}
	if len(delivered) == 0 {
		return 0, false
	}
	u1ID := delivered[0].ID // oldest delivered full-in at this slot
	if err := e.orderMgr.ConfirmDelivery(u1ID, 0); err != nil {
		log.Printf("bin_ops: confirm U1 %d on clear: %v", u1ID, err)
		return 0, false
	}
	return u1ID, true
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

	// Payload handling splits by mode:
	//
	//   - manual_swap (bin loader): an empty is a generic carrier, so the
	//     operator-initiated request is payload-AGNOSTIC. A blank payloadCode is
	//     the normal case — the order ships untagged, Core's planRetrieveEmpty
	//     sources any compatible empty, and LoadBin binds the real payload when
	//     the operator fills it. A non-blank code (direct API caller, or a
	//     future carrier picker) is still validated against the loadable set.
	//
	//     Blank sourcing assumes the loader is SINGLE-CARRIER: planRetrieveEmpty's
	//     bin-type advisory clause is permissive when the order names no payload,
	//     so on a loader spanning multiple carrier types it could fetch the wrong
	//     container. TODO: add a bin_type/carrier field to OrderRequest so a
	//     multi-carrier loader can request "an empty of carrier X" without naming
	//     a payload (today payload_code is the only carrier proxy on the wire).
	//
	//   - simple / multi-step (press swap) nodes: the empty rides the same robot
	//     choreography as the part it precedes, so a payload is still required.
	if claim.SwapMode == protocol.SwapModeManualSwap {
		if payloadCode != "" && !slices.Contains(e.loadablePayloads(node, claim), payloadCode) {
			return nil, fmt.Errorf("payload %q not in allowed list for node %s", payloadCode, node.Name)
		}
	} else {
		if payloadCode == "" {
			return nil, fmt.Errorf("no payload code specified")
		}
		if !slices.Contains(e.loadablePayloads(node, claim), payloadCode) {
			return nil, fmt.Errorf("payload %q not in allowed list for node %s", payloadCode, node.Name)
		}
	}

	// Anti-spam: a manual_swap loader has ONE physical bin slot, so at most one
	// empty may be inbound at a time. Reject a second request while a
	// retrieve_empty is already non-terminal at this node. The board greys its
	// request button the instant a request fires (hasBin/hasDemand), so this is
	// belt-and-suspenders for double-tap races and direct API callers — without
	// it CanAcceptOrders waves manual_swap through (multi-order queue) and the
	// queue could be stacked with redundant empties. Fail closed on a read
	// error: better to make the operator retry than to dispatch into the dark.
	active, err := e.db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		return nil, fmt.Errorf("node %s: check in-flight empties: %w", node.Name, err)
	}
	for _, o := range active {
		if o.RetrieveEmpty {
			return nil, fmt.Errorf("node %s: an empty bin is already inbound", node.Name)
		}
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
	if claim.SwapMode != protocol.SwapModeSimple && claim.SwapMode != protocol.SwapModeManualSwap {
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
	//
	// Source group is the loader's claim.InboundSource (the supermarket the
	// operator is configured to pull empties from). Without this, Core's
	// planRetrieveEmpty falls back to a global FIFO scan and can return a
	// payload-matching empty bin from anywhere — including the empty-tote
	// return area (Hopkinsville, 2026-05-14, Mission #51 pulled SMN_07
	// instead of from Supermarket Area).
	order, err := e.orderMgr.CreateRetrieveOrder(
		&nodeID, true, 1, node.CoreNodeName, claim.InboundSource, "",
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
	//
	// Source group is claim.InboundSource (the FG supermarket the unloader
	// pulls full bins from). Without this, Core's planRetrieve falls back to
	// global FIFO and can pull from the wrong supermarket. Same root cause
	// as the empty-side bug above.
	autoConfirm := false
	order, err := e.orderMgr.CreateRetrieveOrder(
		&nodeID, false, 1, node.CoreNodeName, claim.InboundSource, "",
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
