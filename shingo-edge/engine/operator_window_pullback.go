// operator_window_pullback.go — "Pull From Market" feature for shared-window loaders.
//
// Operator workflow: during a cell launch or restart, bins may be sitting in
// the outbound supermarket that the operator wants to pull back to the loader
// window for manual clearing before resuming normal flow. The operator opens
// the loader HMI, clicks "PULL FROM MARKET", picks a bin from the market list,
// and Shingo dispatches a single move order: market slot → loader window.
//
// After the robot delivers the bin to the window the operator clears it
// physically (or via the normal Clear Bin action) and then restarts the
// standard empty-request / load-full cycle.

package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
)

// MarketBinInfo describes one bin available in the outbound supermarket for
// a loader's pull-back picker.
type MarketBinInfo struct {
	NodeName     string `json:"node_name"`
	NodeLabel    string `json:"node_label"`
	PayloadCode  string `json:"payload_code"`
	UOPRemaining int    `json:"uop_remaining"`
}

// FetchMarketBins returns the bins currently sitting in the outbound
// supermarket for the given produce loader node. Used by the HMI picker to
// let the operator choose which bin to pull back.
//
// Steps:
//  1. Resolve the loader's outbound_destination from the active claim.
//  2. Fetch the direct children of that node from Core.
//  3. Fetch bin states for those children.
//  4. Return occupied children that hold a bin with a payload.
func (e *Engine) FetchMarketBins(nodeID int64) ([]MarketBinInfo, error) {
	node, _, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return nil, err
	}
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.Role != protocol.ClaimRoleProduce {
		return nil, fmt.Errorf("node %s is not a produce node", node.Name)
	}
	if claim.OutboundDestination == "" {
		return nil, fmt.Errorf("node %s has no outbound_destination configured", node.Name)
	}

	children, err := e.coreClient.FetchNodeChildren(claim.OutboundDestination)
	if err != nil || len(children) == 0 {
		return nil, nil
	}

	childNames := make([]string, len(children))
	for i, c := range children {
		childNames[i] = c.Name
	}

	bins, err := e.coreClient.FetchNodeBins(childNames)
	if err != nil {
		return nil, fmt.Errorf("fetch market bins: %w", err)
	}

	labelByName := make(map[string]string, len(children))
	for _, c := range children {
		labelByName[c.Name] = c.Name
	}

	var result []MarketBinInfo
	for _, b := range bins {
		if !b.Occupied || b.PayloadCode == "" {
			continue
		}
		result = append(result, MarketBinInfo{
			NodeName:     b.NodeName,
			NodeLabel:    labelByName[b.NodeName],
			PayloadCode:  b.PayloadCode,
			UOPRemaining: b.UOPRemaining,
		})
	}
	return result, nil
}

// PullFromMarket creates a single move order to pull a bin from a market slot
// back to the loader window. The source must be a direct child of the loader's
// outbound_destination and must hold a bin.
func (e *Engine) PullFromMarket(nodeID int64, sourceCoreName string) error {
	node, _, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return err
	}
	if claim == nil {
		return fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.Role != protocol.ClaimRoleProduce {
		return fmt.Errorf("node %s is not a produce node", node.Name)
	}
	if claim.OutboundDestination == "" {
		return fmt.Errorf("node %s has no outbound_destination configured", node.Name)
	}
	if sourceCoreName == "" {
		return fmt.Errorf("source_core_node is required")
	}

	// Verify the source is a child of this loader's outbound destination.
	children, err := e.coreClient.FetchNodeChildren(claim.OutboundDestination)
	if err != nil {
		return fmt.Errorf("pull from market: fetch market children: %w", err)
	}
	var validSource bool
	for _, c := range children {
		if c.Name == sourceCoreName {
			validSource = true
			break
		}
	}
	if !validSource {
		return fmt.Errorf("node %s is not in the outbound market for %s", sourceCoreName, node.Name)
	}

	// Check the source actually has a bin to pull.
	bins, err := e.coreClient.FetchNodeBins([]string{sourceCoreName})
	if err != nil || len(bins) == 0 || !bins[0].Occupied {
		return fmt.Errorf("no bin at market slot %s", sourceCoreName)
	}
	payload := bins[0].PayloadCode

	order, err := e.orderMgr.CreateMoveOrderWithPayloadCode(&nodeID, 1,
		sourceCoreName, node.CoreNodeName, payload, true)
	if err != nil {
		return fmt.Errorf("pull from market: create order: %w", err)
	}
	log.Printf("market_pullback: order %d: %s → %s payload=%q (auto-clear on delivery)", order.ID, sourceCoreName, node.CoreNodeName, payload)

	e.marketPullbacksMu.Lock()
	e.marketPullbacks[order.UUID] = nodeID
	e.marketPullbacksMu.Unlock()

	return nil
}
