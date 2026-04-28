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
	if claim.SwapMode != "manual_swap" {
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
	if _, err := e.coreClient.LoadBin(&BinLoadRequest{
		NodeName:    node.CoreNodeName,
		PayloadCode: payloadCode,
		UOPCount:    uopCount,
		Manifest:    items,
	}); err != nil {
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
		return nil
	}

	// Fallback: no L1 in flight (e.g. operator loaded a bin that was placed
	// at the loader manually rather than via a retrieve_empty). Set runtime
	// and create L2 directly so the bin still gets dispatched to outbound.
	claimID := claim.ID
	if err := e.db.SetProcessNodeRuntime(nodeID, &claimID, int(uopCount)); err != nil {
		log.Printf("bin_ops: set runtime for node %d: %v", nodeID, err)
	}
	if claim.OutboundDestination != "" {
		// L2 to OutboundDestination is unattended (supermarket node) — must
		// auto-confirm or it sticks at `delivered` forever. See the same
		// reasoning in handleLoaderEmptyInCompletion.
		order, err := e.orderMgr.CreateMoveOrder(&nodeID, 1, node.CoreNodeName, claim.OutboundDestination, true)
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
	if claim.SwapMode != "manual_swap" {
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

// RequestEmptyBin requests an empty bin compatible with the given payload to be
// delivered to a manual_swap produce node. Core queues the order if no empties are
// immediately available. payloadCode determines bin type compatibility.
func (e *Engine) RequestEmptyBin(nodeID int64, payloadCode string) (*orders.Order, error) {
	node, _, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return nil, err
	}
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.SwapMode != "manual_swap" {
		return nil, fmt.Errorf("node %s is not a manual_swap node", node.Name)
	}
	if claim.Role != "produce" {
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

	// Create retrieve order for an empty bin — Core queues if none available.
	// Use claim-level auto_confirm if set, otherwise fall back to global config.
	autoConfirm := claim.AutoConfirm || e.cfg.Web.AutoConfirm
	order, err := e.orderMgr.CreateRetrieveOrder(
		&nodeID, true, 1, node.CoreNodeName, "",
		"standard", payloadCode, autoConfirm,
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
	if claim.SwapMode != "manual_swap" {
		return nil, fmt.Errorf("node %s is not a manual_swap node", node.Name)
	}
	if claim.Role != "consume" {
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
	autoConfirm := claim.AutoConfirm || e.cfg.Web.AutoConfirm
	order, err := e.orderMgr.CreateRetrieveOrder(
		&nodeID, false, 1, node.CoreNodeName, "",
		"standard", payloadCode, autoConfirm,
	)
	if err != nil {
		return nil, err
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil); err != nil {
		log.Printf("bin_ops: update runtime orders for node %d: %v", nodeID, err)
		}
	return order, nil
}
