// handlers_test_orders_helpers.go — small step-builder helpers shared
// by the Kafka and Direct complex-order submitters. They make sense as
// package-level functions (no receiver state needed) and are used from
// both handlers_test_orders_kafka.go and handlers_test_orders_direct.go.
//
// The swap-mode step lists live HERE, as named builders, rather than as
// inline literals at each handler's call site, for the same reason the
// Edge keeps its builders enumerable: a staging dropoff that is not
// declared exclusive looks exactly like one that is, and the difference
// is a reservation Core either makes or does not. The two handlers build
// byte-identical lists, so there is one list of builders to walk —
// TestEveryWwwStagingDropoffIsDeclared (handlers_test_orders_staging_test.go)
// walks it, and a seventh staging dropoff added as a raw literal reaches
// the plant undeclared unless it is added here.

package www

import "shingo/protocol"

func pickupStepDirect(node string) protocol.ComplexOrderStep {
	if node != "" {
		return protocol.ComplexOrderStep{Action: "pickup", Node: node}
	}
	return protocol.ComplexOrderStep{Action: "pickup"}
}

func dropoffStep(node string) protocol.ComplexOrderStep {
	if node != "" {
		return protocol.ComplexOrderStep{Action: "dropoff", Node: node}
	}
	return protocol.ComplexOrderStep{Action: "dropoff"}
}

// stagingDropoffStep is the declared staging dropoff — the one form Core
// cannot recognise on its own. A staging node is a station with no parent,
// so isConcreteStorageDropoff rejects it and both destination gates stand
// down: reserved by nothing, checked by nothing, free for a second order
// to take while the first robot is on its way. Declaring it is not inference — the operator typed the
// node into a field named staging, and the handler refuses the request
// without one.
func stagingDropoffStep(node string) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{Action: "dropoff", Node: node, ExclusiveSlot: true}
}

// complexSwapRequest is the /test-orders complex-order form, shared by both
// submission routes (Kafka publish and in-process dispatch). One named type
// rather than two anonymous ones, so the step builders below can take it
// without re-declaring the field list at every seam.
type complexSwapRequest struct {
	CycleMode           protocol.SwapMode `json:"cycle_mode"`
	Location            string            `json:"location"`
	InboundStaging      string            `json:"inbound_staging"`
	OutboundStaging     string            `json:"outbound_staging"`
	InboundSource       string            `json:"inbound_source"`
	OutboundDestination string            `json:"outbound_destination"`
	PayloadCode         string            `json:"payload_code"`
	Priority            int               `json:"priority"`
}

// buildSwapSequentialSteps is the sequential swap: drop an empty at the
// cell, wait, take the full away. One order, no sibling.
func buildSwapSequentialSteps(req complexSwapRequest) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "dropoff", Node: req.Location},
		{Action: "wait"},
		{Action: "pickup", Node: req.Location},
		dropoffStep(req.OutboundDestination),
	}
}

// buildSwapResupplySteps is the two-robot supply leg: fetch a bin, park it
// at the staging node, wait for the swap, place it at the cell.
func buildSwapResupplySteps(req complexSwapRequest) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		pickupStepDirect(req.InboundSource),
		stagingDropoffStep(req.InboundStaging),
		{Action: "wait"},
		{Action: "pickup", Node: req.InboundStaging},
		{Action: "dropoff", Node: req.Location},
	}
}

// buildSwapRemovalSteps is the two-robot removal leg, carrying the supply
// leg's uuid: drop an empty at the cell, wait, take the full away.
func buildSwapRemovalSteps(req complexSwapRequest) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "dropoff", Node: req.Location},
		{Action: "wait"},
		{Action: "pickup", Node: req.Location},
		dropoffStep(req.OutboundDestination),
	}
}

// buildSwapSingleRobotSteps is the single-robot mode: stage the supply at
// the inbound staging node, run the cell, retrieve it, then take the old
// bin away through the outbound staging node.
func buildSwapSingleRobotSteps(req complexSwapRequest) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		pickupStepDirect(req.InboundSource),
		stagingDropoffStep(req.InboundStaging),
		{Action: "dropoff", Node: req.Location},
		{Action: "wait"},
		{Action: "pickup", Node: req.Location},
		stagingDropoffStep(req.OutboundStaging),
		{Action: "pickup", Node: req.InboundStaging},
		{Action: "dropoff", Node: req.Location},
		{Action: "pickup", Node: req.OutboundStaging},
		dropoffStep(req.OutboundDestination),
	}
}
