// preview.go — read-only capacity preview for the UI. Phase 4d of
// bin-transit-state.
//
// Mirrors the shape of Edge's PreviewChangeoverPlan: callers ask the
// dispatcher "would this dispatch right now or would it queue?" before
// submitting, so the operator UI can render "this would queue,
// blocking on Node X" inline. Same predicate as the CheckDropoffCapacity
// gate that runs at intake — single source of truth.

package dispatch

import "shingo/protocol"

// DropoffCapacityPreview is the read-only result of asking the dispatcher
// whether a given delivery node would accept a fresh order right now.
// Fields mirror CheckDropoffCapacity's return tuple in JSON-friendly
// form so the UI can serialize and display directly.
type DropoffCapacityPreview struct {
	Blocked      bool   `json:"blocked"`
	Reason       string `json:"reason,omitempty"`
	DeliveryNode string `json:"delivery_node"`
}

// PreviewDropoffCapacity wraps CheckDropoffCapacity for caller-facing use.
// excludeOrderID=0 since previews are not tied to an existing order
// (the in-flight count includes everything that's actually inbound).
//
// The capacity model can shift between preview-call and actual dispatch
// (operator clicks twice in quick succession, scanner runs, etc.); the
// preview is "best-effort current snapshot" rather than a reservation.
// The actual gate at HandleComplexOrderRequest / planRetrieve / etc. is
// what enforces correctness.
func (d *Dispatcher) PreviewDropoffCapacity(deliveryNode string) DropoffCapacityPreview {
	blocked, block := CheckDropoffCapacity(d.db, deliveryNode, 0)
	reason := ""
	if blocked {
		reason = FormatQueueSentence(protocol.QueueWaitingForSlot, block.Params)
	}
	return DropoffCapacityPreview{
		Blocked:      blocked,
		Reason:       reason,
		DeliveryNode: deliveryNode,
	}
}
