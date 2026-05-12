// handlers_test_orders_helpers.go — small step-builder helpers shared
// by the Kafka and Direct complex-order submitters. They make sense as
// package-level functions (no receiver state needed) and are used from
// both handlers_test_orders_kafka.go and handlers_test_orders_direct.go.

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
