package protocol

import "time"

// SubjectHoldDropped — Edge → Core: one produce seat's hold (the seat's
// pending_uop_delta / pending_uop_payload bucket) was dropped, in part or in
// whole, without being replayed onto a bin. Core writes one hold_dropped row on
// bin_uop_exception per message (bin_id NULL: a hold has no carrier) and lists
// it on /inventory beside the declared-full stamps, because a dropped hold
// makes the next bin short and that bin is then stamped, so the two rows
// describe the same parts.
//
// A SUBJECT on the existing shingo.orders topic (the SubjectLinesideLevelReport
// precedent — NOT a new topic, NOT a new consumer group). Value schema is
// ADDITIVE-only. An older Core that does not register this subject logs and
// returns through the SubjectRouter's unknown-subject path and commits the
// offset; the Edge outbox acks a row when the broker takes it, not when Core
// handles it, so nothing behind it waits (pinned by
// router.TestUnknownSubjectIsDroppedAndTheNextIsHandled). The rows sent to an
// older Core are lost: deploy Core first.
//
// A DISCRETE EVENT, so it is neither coalesced nor allowed to expire: it
// carries NoExpiry (subjectTTLs) and never dead-letters (the outbox drainer's
// neverDeadLetter), the treatment TypeOrderIngest gets. A late copy is
// harmless and a redelivered one writes nothing: Core keys the row on the
// envelope id, which the outbox row and every Kafka redelivery carry
// unchanged.
const SubjectHoldDropped = "inventory.hold_dropped"

// Why a produce seat's hold was dropped. The Edge writes one of these on every
// non-replay exit from the hold; Core stores it verbatim in the row's detail
// and the /inventory list prints it. Stable strings: historical rows carry
// them.
const (
	// HoldDropNoCarrier: an unbound seat's hold, with no live order placing a
	// carrier at the node, found at a tick that would hold.
	HoldDropNoCarrier = "no_carrier"
	// HoldDropBackstop: a hold larger than the pack found at replay onto a
	// bound bin; the tick's own input is charged and the hold is dropped.
	HoldDropBackstop = "backstop"
	// HoldDropHoldCap: a bound seat at its pack whose hold would pass one
	// pack; the excess over one pack is dropped.
	HoldDropHoldCap = "hold_cap"
	// HoldDropCutover: the process's active style flipped to a part the hold
	// is not of; the departing part's hold is cleared at the flip.
	HoldDropCutover = "cutover"
	// HoldDropForeignHold: a hold of another part beside a bound bin; what
	// does not fit the bin is dropped rather than relabelled.
	HoldDropForeignHold = "foreign_hold"
	// HoldDropSweep: the periodic or boot sweep found an unbound seat's hold
	// with no live order placing a carrier at the node.
	HoldDropSweep = "sweep"
	// HoldDropFlipCarryOverflow: a sequential flip carried the departing
	// seat's hold to its partner and part of it fit nowhere.
	HoldDropFlipCarryOverflow = "flip_carry_overflow"
)

// HoldDropped is one dropped hold.
//
//   - Node: the Core node name of the produce seat whose hold was dropped.
//   - PayloadCode: the part the hold was of ("" when the hold carried none).
//   - Qty: the units dropped (cycles, the hold's unit), > 0.
//   - Reason: one of the HoldDrop* constants.
//   - CalledBy: who or what dropped it (an operator, or the Edge component).
//   - OccurredAt: when the Edge dropped it, on the Edge's clock.
//   - OrderID: the Edge UUID of the order the drop names, when it names one
//     (the order the hold was waiting for, or the flip's order); "" otherwise.
//
// The station is the envelope's source; it is not repeated here.
type HoldDropped struct {
	Node        string    `json:"node"`
	PayloadCode string    `json:"payload_code"`
	Qty         int64     `json:"qty"`
	Reason      string    `json:"reason"`
	CalledBy    string    `json:"called_by,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
	OrderID     string    `json:"order_id,omitempty"`
}
