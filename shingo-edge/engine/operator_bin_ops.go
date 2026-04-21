package engine

import (
	"fmt"
	"log"
	"slices"

	"shingo/protocol"
	"shingoedge/store"
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

	// Check that a bin is actually at this node
	if e.coreClient.Available() {
		bins, _ := e.coreClient.FetchNodeBins([]string{node.CoreNodeName})
		if len(bins) == 0 || !bins[0].Occupied {
			return fmt.Errorf("no bin at node %s — request an empty bin first", node.Name)
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

	// Server-side demand guard: require an active order matching this payload.
	// Prevents API bypass of the HMI demand check that protects storage capacity.
	activeOrders, err := e.db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		return fmt.Errorf("check demand at node %s: %w", node.Name, err)
	}
	if len(activeOrders) == 0 {
		return fmt.Errorf("no active demand at node %s — cannot load without a pending order", node.Name)
	}
	// Per-payload demand check: if orders carry payload_code, verify a match.
	hasPayloadMatch := false
	hasLegacy := false
	for _, o := range activeOrders {
		if o.PayloadCode == payloadCode {
			hasPayloadMatch = true
			break
		}
		if o.PayloadCode == "" {
			hasLegacy = true
		}
	}
	if !hasPayloadMatch && !hasLegacy {
		return fmt.Errorf("no active demand for payload %q at node %s", payloadCode, node.Name)
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

	// Update edge-side runtime state
	claimID := claim.ID
	if err := e.db.SetProcessNodeRuntime(nodeID, &claimID, int(uopCount)); err != nil {
		log.Printf("bin_ops: set runtime for node %d: %v", nodeID, err)
		}

	// If outbound destination is configured, move the loaded bin there
	if claim.OutboundDestination != "" {
		order, err := e.orderMgr.CreateMoveOrder(&nodeID, 1, node.CoreNodeName, claim.OutboundDestination, claim.AutoConfirm || e.cfg.Web.AutoConfirm)
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
func (e *Engine) RequestEmptyBin(nodeID int64, payloadCode string) (*store.Order, error) {
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

	// Check that node doesn't already have a bin
	if e.coreClient.Available() {
		bins, _ := e.coreClient.FetchNodeBins([]string{node.CoreNodeName})
		if len(bins) > 0 && bins[0].Occupied {
			return nil, fmt.Errorf("node %s already has a bin", node.Name)
		}
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
func (e *Engine) RequestFullBin(nodeID int64, payloadCode string) (*store.Order, error) {
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
