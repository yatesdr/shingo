package dispatch

import (
	"fmt"

	"shingo/protocol"
)

// QueueParams carries the values the operator-visible sentence is generated from.
// They are NOT persisted — the formatter consumes them at set-time to build the
// sentence (queue_reason) and then they are discarded. Nothing rebuilds the
// sentence later; the structured queue_code + queue_cause columns carry the
// analytic signal, and the sentence carries the human one.
//
// Field use is per code:
//   - QueueWaitingForMaterial: Payload (the part) + Kind (full|empty) + Partial.
//   - QueueWaitingForSlot:      Destination.
//   - QueueStorageRearranging:  Lane + Payload.
//   - QueueWaitingForPartner:   Sibling (the partner order's edge UUID).
//   - QueueFleetUnavailable:    none.
type QueueParams struct {
	// Payload is the part code the order is waiting on material for. Empty for
	// an empty-carrier wait ("Waiting for an empty bin").
	Payload string
	// Kind is "full" (a loaded bin of payload) or "empty" (an empty carrier).
	// Defaults to "full" when empty; only meaningful for QueueWaitingForMaterial.
	Kind string
	// Partial is true when the order holds part of a multi-bin set (a complex
	// "3 of 5" reserve). Operators read the same sentence either way; the flag
	// lets analytics count partials via the queue_cause='reserve-holding' tag.
	Partial bool
	// Destination is the delivery node the order is waiting on a slot at.
	Destination string
	// Lane is the storage lane being rearranged (burial / reshuffle).
	Lane string
	// Sibling is the partner order's edge UUID in a two-robot swap.
	Sibling string
}

// FormatQueueSentence renders the operator-visible sentence for a queue code +
// parameters. This is the ONE place the wording lives: every producer passes a
// code + params, the sentence is generated here, and the caller writes
// sentence+code+cause together. Adding a code means handling it here (the
// exhaustiveness test walks AllQueueCodes through this function so an unhandled
// code fails the build, not silently renders empty).
//
// Wording is owner-pinned; a snapshot test pins the exact strings.
func FormatQueueSentence(code protocol.QueueCode, p QueueParams) string {
	switch code {
	case protocol.QueueWaitingForMaterial:
		if p.Kind == "empty" || p.Payload == "" {
			return "Waiting for an empty bin"
		}
		return fmt.Sprintf("Waiting for material: %s", p.Payload)
	case protocol.QueueWaitingForSlot:
		return fmt.Sprintf("Waiting for a slot at %s", p.Destination)
	case protocol.QueueStorageRearranging:
		return "Rearranging storage to reach this material"
	case protocol.QueueWaitingForPartner:
		return "Waiting for partner robot"
	case protocol.QueueFleetUnavailable:
		return "Robot system not responding — retrying"
	default:
		return ""
	}
}
