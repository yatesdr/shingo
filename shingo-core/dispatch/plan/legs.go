package plan

import "shingo/protocol"

// legs.go — the leg shapes: the step a plan's legs are made of, and the shape of
// a single-bin transport.

// Step is one leg step with concrete node names after resolution (dispatch
// calls it resolvedStep, and persists a list of them as steps_json).
type Step struct {
	Action string `json:"action"`
	Node   string `json:"node,omitempty"`
	// Group is the originating NGRP name when Node was resolved from a node
	// group. Retained so a store drop-off that loses the dispatch-time slot
	// claim can revert Node->Group and be re-resolved to a free slot on the
	// next scanner tick. Empty (the field) for concrete (non-group) nodes.
	Group string `json:"group,omitempty"`
	// Empty mirrors protocol.ComplexOrderStep.Empty: a pickup leg that must
	// fetch an EMPTY carrier (produce "bring an empty to fill") rather than a
	// payload-matching full. Threaded through resolution + claim so the
	// distinction survives steps_json persistence and scanner replay.
	Empty bool `json:"empty,omitempty"`
	// PayloadCode mirrors protocol.ComplexOrderStep.PayloadCode: the payload
	// THIS leg's bin selection resolves against, when it must differ from the
	// order's. Threaded through resolution + claim + steps_json for exactly the
	// reason Empty is, and the sim proved the reason on 2026-08-25: without it
	// the FIRST resolution honoured the incoming style's carrier type and parked
	// correctly when none was free, and the REPLAY rebuilt the step without the
	// payload, fell back to the order's — the outgoing style's — and delivered
	// the carrier the press was leaving. Right until it retried.
	PayloadCode string `json:"payload_code,omitempty"`
	// ExclusiveSlot mirrors protocol.ComplexOrderStep.ExclusiveSlot: this dropoff
	// lands on a node holding one bin at a time (a staging node), which Core's
	// role test cannot recognise on its own. Threaded through resolution for the
	// same reason as Empty — slotNeeds reads it off the PERSISTED steps on every
	// scanner replay, not just on the intake pass, so losing it at any hop would
	// silently un-reserve the node on the first retry.
	ExclusiveSlot bool `json:"exclusive_slot,omitempty"`

	// ── WHO RELEASES THIS WAIT ────────────────────────────────────────────
	//
	// Meaningful only on an ActionWait step. One plan can hold an operator wait
	// and a lane wait at the same time, and no column on the order row can say
	// which one the order is parked at — wait_index names a POSITION, and the
	// kind is a property of the step in that position. So it lives here.
	//
	// This is the final form of core_staged/edge_staged: the distinction is
	// real, and it is on the STEP rather than on the status.
	//
	// ZERO VALUE IS THE OPERATOR WAIT, which is what every plan ever written
	// carries, so nothing needs migrating and an unstamped step keeps exactly
	// the meaning it had. WaitKindLane is stamped by the one thing that creates
	// a lane wait.
	WaitKind string `json:"wait_kind,omitempty"`
	// WaitLane is the lane whose evaluator owns this wait — 0 unless WaitKind
	// is WaitKindLane.
	//
	// A NODE ID, not a name, and it is the one field here that is not a name.
	// Node and Group are names because they are RESOLVED against the node graph
	// and re-resolved as the plant changes (an NGRP becomes a concrete child).
	// This is not resolved against anything: it is an identity pin, written once
	// when the wait is created and read only by Core's own routing, whose entire
	// API is already keyed on lane id (EvaluateLaneReleases, laneGates.lock,
	// LaneIDsForGateEvent, ListHeldLegParentsInLane). A name would need a lookup
	// per candidate on the release path and would not survive a lane rename —
	// and unlike Node, nothing downstream re-derives it from the plant.
	WaitLane int64 `json:"wait_lane,omitempty"`

	// Vacate is set on a DROPOFF the vacated-slot rule admitted: a drop that
	// would otherwise have waited, onto a node whose only bin a committed lift
	// takes first (vacated.go). It names the node it was granted for, and
	// every reader honours it only while the step still drops there
	// (StampHonoured), so a stamp left behind by a writer that re-pointed the
	// step stops counting on its own. Only Core reads Core's steps_json.
	Vacate *Vacate `json:"vacate,omitempty"`
	// Anchor is the Edge-authored home a dedicated-loader return was pointed at
	// before the vacated arm re-pointed it (placeForLoader). Placement
	// re-derives from it on a later pass, so a parked pair does not re-anchor on
	// the slot its partner happened to vacate last time. Written only by the
	// vacated arm; every other placement leaves it empty and behaves as before.
	Anchor string `json:"anchor,omitempty"`
}

// WaitKindLane marks a wait ONLY the lane evaluator may advance: its
// precondition is a lane fact (a dig, a robot inside, a slot reachable) that
// nothing outside Core can observe.
//
// The absence of this value means an OPERATOR wait — a station reports
// something Core genuinely cannot see, and HandleOrderRelease advances it. That
// asymmetry is deliberate: the older, larger population is the one that needs
// no marker.
const WaitKindLane = protocol.WaitKindLane

// WaitKindStation marks a wait the STATION advances — the swap choreography's
// own gates ("hold at staging until the line clears", "tooling done", "ready",
// the changeover cutover). Its precondition is something a person or a station
// observes and Core cannot, which is the mirror image of WaitKindLane.
//
// ── IT EXISTS BECAUSE ABSENCE COULD NOT CROSS THE WIRE ────────────────────
//
// The rule used to be "zero value means operator wait", and inside Core that
// was enough: the splice stamps the lane waits, everything else is the station's
// by elimination. It stops being enough at the boundary. The Edge holds the plan
// it authored and receives no stamp at all, so it cannot distinguish "no kind
// because the station owns it" from "no kind because nobody said" — and neither
// can the HMI it draws. Absence is not a claim; it is the lack of one.
//
// So ownership is now DECLARED by whoever authors the wait, on both sides, and
// the zero value is reserved for pre-ruling plans still in flight.
const WaitKindStation = protocol.WaitKindStation

// TransportLegs expresses a single-bin transport — retrieve, retrieve_empty, or
// move — as the canonical two-step plan: pick the (already-resolved) source bin
// up, drop it at the (already-resolved) delivery node. stepsToBlocks yields
// [pickup@source, dropoff@dest] → [JackLoad@source, JackUnload@dest], the
// TransportOrderRequest the transport tail has always built.
//
// emptyPickup marks the pickup as an empty-carrier fetch (retrieve_empty) so the
// intent survives as STEP DATA (Step.Empty) — the property that replaced the
// OrderType==RetrieveEmpty scanner fork — rather than as the order's type. It
// does not change the fleet block shape.
//
// Moved from dispatch/plan_simple.go unchanged (B1, buildTransportPlan).
func TransportLegs(sourceNode, deliveryNode string, emptyPickup bool) []Step {
	return []Step{
		{Action: protocol.ActionPickup, Node: sourceNode, Empty: emptyPickup},
		{Action: protocol.ActionDropoff, Node: deliveryNode},
	}
}
