// operator_window_pullback.go — "Pull From Market" feature for shared-window loaders.
//
// Operator workflow: during a cell launch or restart, bins may be sitting in
// the outbound supermarket that the operator wants to pull back to the loader
// window for manual clearing before resuming normal flow. The operator opens
// the loader HMI, clicks "PULL FROM MARKET", picks a bin from the market list,
// and Shingo dispatches a single move order: market slot → loader window.
//
// After the robot delivers the bin to the window Edge auto-clears the UOP
// (via the pullback intent on the order row, taken by the delivery hook in
// wiring_delivered.go — see order_intent.go) so the
// operator can immediately resume the standard empty-request / load-full cycle.

package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
	ordermgr "shingoedge/orders"
)

// MarketBinInfo describes one bin available in the outbound supermarket for
// a loader's pull-back picker: what its row in the bins table carries, which
// is whether it holds parts and fewer than its part's capacity, never a count.
type MarketBinInfo struct {
	NodeName    string `json:"node_name"`
	NodeLabel   string `json:"node_label"`
	PayloadCode string `json:"payload_code"`
	Stocked     bool   `json:"stocked"`
	Partial     bool   `json:"partial"`
	UOPCapacity int    `json:"uop_capacity,omitempty"`
}

// FetchMarketBins returns the bins currently sitting in the outbound
// supermarket for the given produce loader node. Used by the HMI picker to
// let the operator choose which bin to pull back.
//
// Steps:
//  1. Resolve the loader's outbound_destination from the active claim.
//  2. Its members: the market's group row on the bins copy (groupMembers).
//  3. Their rows in the bins table (site 12 of the reads), under the decision
//     rule: a table that may not be decided from is refused with its sentence.
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

	childNames, known := e.groupMembers(claim.OutboundDestination)
	if !known || len(childNames) == 0 {
		return nil, nil
	}

	rows, err := e.binsForDecision(childNames...)
	if err != nil {
		return nil, err
	}

	var result []MarketBinInfo
	for _, c := range childNames {
		b, ok := rows[c]
		if !ok || !b.Occupied || b.PayloadCode == "" {
			continue
		}
		result = append(result, MarketBinInfo{
			NodeName:    c,
			NodeLabel:   c,
			PayloadCode: b.PayloadCode,
			Stocked:     b.Stocked,
			Partial:     b.Partial,
			UOPCapacity: b.UOPCapacity,
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
	children, known := e.groupMembers(claim.OutboundDestination)
	if !known {
		return fmt.Errorf("pull from market: the slots of %s are not known yet", claim.OutboundDestination)
	}
	var validSource bool
	for _, c := range children {
		if c == sourceCoreName {
			validSource = true
			break
		}
	}
	if !validSource {
		return fmt.Errorf("node %s is not in the outbound market for %s", sourceCoreName, node.Name)
	}

	// Check the source actually has a bin to pull, from its row in the bins
	// table (site 13 of the reads). A table that may not be decided from
	// refuses with its own sentence (BinsNotCurrent), never "no bin at market
	// slot X": a read that could not answer must not tell the operator the slot
	// is empty. No order is created either way.
	slot, err := e.binsRowAt(sourceCoreName)
	if err != nil {
		return err
	}
	if !slot.Occupied {
		return fmt.Errorf("no bin at market slot %s", sourceCoreName)
	}
	payload := slot.PayloadCode

	// NoDemand: a pullback is an operator moving stock between market and window.
	// It serves no cell's level and belongs to no episode. The move names the bin
	// the read above found: Core refuses it when that bin has left the slot, and
	// lifts it and no other.
	order, err := e.orderMgr.CreateMoveOrderWithPayloadCodeForBin(&nodeID, 1,
		sourceCoreName, node.CoreNodeName, payload, slot.BinID, true, ordermgr.NoDemand())
	if err != nil {
		return fmt.Errorf("pull from market: create order: %w", err)
	}
	log.Printf("market_pullback: order %d: %s → %s payload=%q (auto-clear on delivery)", order.ID, sourceCoreName, node.CoreNodeName, payload)

	// Arm the auto-clear on the order row, not in memory, so it survives an
	// Edge restart before the delivery (order_intent.go).
	e.armOrderIntent(order.ID, orderIntent{Kind: intentPullback, NodeID: nodeID})

	return nil
}
