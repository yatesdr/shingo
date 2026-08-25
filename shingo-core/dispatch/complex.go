package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"shingo/protocol"
)

// resolvedStep is a step with concrete node names after resolution.
type resolvedStep struct {
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
}

// WaitKindLane marks a wait ONLY the lane evaluator may advance: its
// precondition is a lane fact (a dig, a robot inside, a slot reachable) that
// nothing outside Core can observe.
//
// The absence of this value means an OPERATOR wait — a station reports
// something Core genuinely cannot see, and HandleOrderRelease advances it. That
// asymmetry is deliberate: the older, larger population is the one that needs
// no marker.
const WaitKindLane = "lane"

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
const WaitKindStation = "station"

// IsStationWait reports whether a station may advance this wait.
//
// THE DRAIN WINDOW LIVES HERE, in one place, so there is exactly one thing to
// change when it closes. Untagged reads as station-owned today — the historical
// default, so no plan in flight changes meaning — and the drift tests
// (TestEveryEdgeAuthoredWaitIsStamped, TestSplice_FenceHoldsOnASplicedPlan) already
// fail on any NEW untagged wait from either author. When the last pre-ruling
// order has drained, delete the `== ""` arm and an untagged wait becomes what it
// should be: unowned, and refused by both fences.
func IsStationWait(kind string) bool {
	return kind == WaitKindStation || kind == ""
}

// CoreOwnsWaitAt reports whether the wait an order is parked at is CORE's to
// advance — the read behind the hard-release affordance and anything else that
// must distinguish the two owners from outside this package.
//
// It is the exact complement of IsStationWait over a real plan, so the drain
// window's default (untagged = the station's) is honoured in one place rather
// than re-decided by each caller. An unreadable plan, or an order parked at no
// wait, answers FALSE: nothing should offer a Core override for a wait it cannot
// identify.
func CoreOwnsWaitAt(stepsJSON string, waitIndex int) bool {
	if stepsJSON == "" {
		return false
	}
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		return false
	}
	w, ok := waitAt(steps, waitIndex)
	if !ok {
		return false
	}
	return !IsStationWait(w.WaitKind)
}

// claimedBin records which bin was claimed at which pickup step.
type claimedBin struct {
	binID     int64
	stepIndex int
	nodeName  string
}

// pickupSkip records why a pickup step in a complex order failed to claim a
// bin. Surfaced to production logs by ApplyComplexPlan so silent claim
// failures (the ALN_002 → SMN_003 incident class) become diagnosable from
// the log instead of only from the late-bind manifest fallback path.
type pickupSkip struct {
	stepIndex int
	nodeName  string
	reason    string
}

// joinRejects formats per-bin reject reasons into a single log line. Caps at
// the first 6 entries so a node with many bins doesn't blow up the log; the
// summary still notes the count even if entries are truncated.
func joinRejects(rejects []string) string {
	const maxEntries = 6
	if len(rejects) <= maxEntries {
		return strings.Join(rejects, "; ")
	}
	return strings.Join(rejects[:maxEntries], "; ") + fmt.Sprintf("; ... +%d more", len(rejects)-maxEntries)
}

// ResolutionErrorClass tags every resolver-error shape with the
// disposition complex intake (and simple-retrieve planning) should
// apply. Single classifier replaces the v6-era pair of
// substring-based + sentinel-based detection helpers — see v7 Step 1.
type ResolutionErrorClass int

const (
	// ResolutionOK is the zero value, returned when err == nil.
	ResolutionOK ResolutionErrorClass = iota
	// ResolutionCapacity covers the momentarily-unsourceable NGRP shapes,
	// both directions of a swap: a saturated dropoff group ("no available
	// slot in node group N", "no bin of requested payload in node group N")
	// and a dry empty-fetch pool ("cannot resolve empty in group N", "no
	// empty carrier in group N"). Intake queues the order with
	// queue_reason = err.Error(); the scanner replays when the shortfall clears.
	ResolutionCapacity
	// ResolutionBuried wraps *BuriedError. Intake routes through
	// the reshuffle planner; simple-retrieve planning calls
	// planBuriedReshuffle.
	ResolutionBuried
	// ResolutionStructural wraps *StructuralError — permanent
	// configuration failure (group has no enabled children, or no
	// child accepts the payload). Terminal-fail.
	ResolutionStructural
	// ResolutionTransient covers wrapped DB errors that the
	// resolver surfaces as "list children of N: ...", "get target
	// depth: ...", etc. We treat them as terminal here rather than
	// looping on them — see §3 in the scope doc for the rationale.
	ResolutionTransient
	// ResolutionFatal covers everything else — unknown shape, treat
	// as terminal.
	ResolutionFatal
)

// capacityKind is the typed payload classifyResolutionError returns alongside
// ResolutionCapacity, naming WHICH of the four capacity shapes matched. The four
// shapes split into two operator-facing categories:
//
//   - capacitySlot: a saturated dropoff group ("no available slot in node
//     group") → the order is waiting on a SLOT at its destination.
//   - capacityPayload / capacityBin: the group is missing CONTENTS — a payload
//     bin or an empty carrier → the order is waiting on MATERIAL.
//
// The split is what lets intake/ngrp resolution pick the right queue code
// (waiting_for_slot vs waiting_for_material) without re-sniffing the same
// substrings at the call site.
//
// capacityPayload used to share capacitySlot's branch, which is the F1 defect
// from the 2026-07-20 Springfield study: "no bin of requested payload in node
// group AMR Supermarket" is a MATERIAL shortage in the SUPERMARKET, but it was
// coded waiting_for_slot and rendered "Waiting for a slot at ALN_003" — the
// wrong problem at the wrong node. Every "Waiting for a slot" order on the floor
// that morning was actually this. Splitting the kind changes the recorded
// queue_code for that condition, which is intended.
type capacityKind int

const (
	// capacityUnknown is the zero value — no capacity shape matched.
	capacityUnknown capacityKind = iota
	// capacitySlot: dropoff group has no free slot to drop into.
	capacitySlot
	// capacityPayload: the source group holds no bin of the requested payload.
	capacityPayload
	// capacityBin: empty-fetch pool is dry (no empty carrier available).
	capacityBin
)

// capacityDetail is the ResolutionCapacity payload: the shape that matched plus
// the context recoverable from the resolver's error text. The resolver returns
// plain fmt.Errorf, so Group and Step are parsed here ONCE rather than re-sniffed
// at each call site — the same reason the kind is returned typed.
type capacityDetail struct {
	Kind capacityKind
	// Group is the node group named by the resolver ("AMR Supermarket").
	// Empty when the message carried no group.
	Group string
	// Step is the zero-based step index of a multi-step order; HasStep says
	// whether it was present, since step 0 is a real step.
	Step    int
	HasStep bool
}

// groupFromResolutionError pulls the node-group name out of a resolver error.
// Both shapes end with the group name:
//
//	"no bin of requested payload in node group AMR Supermarket"
//	"no available slot in node group AMR Supermarket"
//
// The outer wrap ("cannot resolve group X: ...") repeats it, so the LAST
// occurrence is the innermost, most specific one.
func groupFromResolutionError(msg string) string {
	const marker = "node group "
	i := strings.LastIndex(msg, marker)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(msg[i+len(marker):])
}

// stepFromResolutionError pulls the leading "step N:" index that the complex
// replay path prefixes onto a step failure.
func stepFromResolutionError(msg string) (int, bool) {
	const marker = "step "
	if !strings.HasPrefix(msg, marker) {
		return 0, false
	}
	rest := msg[len(marker):]
	end := strings.IndexByte(rest, ':')
	if end <= 0 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest[:end]))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// classifyResolutionError inspects err and returns the typed class
// plus the typed-payload pointer when the class carries one
// (*BuriedError for ResolutionBuried, *StructuralError for
// ResolutionStructural, capacityKind for ResolutionCapacity). The payload is
// nil for the other classes.
//
// Replaces the v6 pair (isCapacityResolutionError + isBuriedResolutionError)
// with a single decision point so both the complex-intake and the
// simple-retrieve paths route through the same classifier. Buried
// detection uses errors.Is against the ErrBuried sentinel so it
// survives any wrap chain; capacity detection still uses substring
// match against the resolver's stable error shapes (the resolver
// returns plain `fmt.Errorf` for those, with no typed sentinel to
// match), but now ALSO returns a typed kind so callers pick the right
// queue code without re-sniffing the same substrings.
func classifyResolutionError(err error) (ResolutionErrorClass, any) {
	if err == nil {
		return ResolutionOK, nil
	}
	// Typed sentinel / typed wrapper detection first — they're more
	// specific than the substring shapes.
	var buriedErr *BuriedError
	if errors.Is(err, ErrBuried) {
		if errors.As(err, &buriedErr) {
			return ResolutionBuried, buriedErr
		}
		// Sentinel matched but typed extraction failed — treat as
		// fatal rather than crashing.
		return ResolutionFatal, nil
	}
	var structErr *StructuralError
	if errors.As(err, &structErr) {
		return ResolutionStructural, structErr
	}
	// Capacity-shaped errors (untyped fmt.Errorf strings from the
	// resolver). resolveStepNode wraps with "cannot resolve group X:
	// <original>" — the substring survives.
	//
	// Two shapes per direction, because a swap has both:
	//   - consume/full retrieve, dropoff into a saturated group:
	//     "no available slot in node group", "no bin of requested payload in node group"
	//   - produce/empty fetch, the "bring a fresh carrier to fill the press" leg
	//     resolving against a momentarily DRY empty pool (resolveStepNode's
	//     step.Empty branch): "cannot resolve empty in group", "no empty carrier in group".
	// Both are sourceable-eventually — an empty returns to the pool, a slot frees —
	// so both QUEUE and retry rather than terminal-reject at intake. The empty pair
	// was missed when the empty-fetch leg was added (0b665a4d, after this classifier
	// already existed for the full pair), so a dry pool aborted produce swaps and
	// half-stranded the press until an operator intervened (2026-07-14 sim run).
	//
	// Still substring-matched, like the rest of this classifier — the resolver
	// returns plain fmt.Errorf for all four with no typed sentinel — but the kind
	// is returned typed so a caller picks its queue code from the kind, not by
	// re-sniffing the message.
	msg := err.Error()
	detail := func(k capacityKind) (ResolutionErrorClass, any) {
		d := &capacityDetail{Kind: k, Group: groupFromResolutionError(msg)}
		d.Step, d.HasStep = stepFromResolutionError(msg)
		return ResolutionCapacity, d
	}
	// SLOT: the group has room-for-nothing. A genuine dropoff-capacity wait.
	if strings.Contains(msg, "no available slot in node group") {
		return detail(capacitySlot)
	}
	// MATERIAL: the group has room but not the CONTENTS. Kept separate from the
	// slot shape above — see the capacityKind doc comment (F1).
	if strings.Contains(msg, "no bin of requested payload in node group") {
		return detail(capacityPayload)
	}
	if strings.Contains(msg, "cannot resolve empty in group") ||
		strings.Contains(msg, "no empty carrier in group") {
		return detail(capacityBin)
	}
	// DB-layer wraps that aren't structural or capacity.
	if strings.Contains(msg, "list children of") ||
		strings.Contains(msg, "get target depth") ||
		strings.Contains(msg, "list lane slots") {
		return ResolutionTransient, nil
	}
	return ResolutionFatal, nil
}

// queueCodeForCapacity maps a typed capacity kind to its operator-facing queue
// code. A slot-shaped capacity error waits on a destination slot; a payload- or
// bin-shaped one waits on MATERIAL. capacityUnknown still parks under
// waiting_for_material (the broader category) so an uncategorized capacity error
// gets a real code rather than rendering empty — but see queueParamsForCapacity,
// which withholds the payload in that case so the sentence does not invent a
// specificity the classifier did not earn.
func queueCodeForCapacity(k capacityKind) protocol.QueueCode {
	switch k {
	case capacitySlot:
		return protocol.QueueWaitingForSlot
	default:
		return protocol.QueueWaitingForMaterial
	}
}

// queueParamsForCapacity builds the sentence params for a classified capacity
// error. It is the single place the F1 location rule is enforced: a payload
// shortage names the GROUP it is short in, never the order's lineside delivery
// node, because the operator has to go look in the group.
//
// For capacityUnknown the payload is deliberately withheld. Classification is
// substring matching over untyped resolver errors, so an unrecognised message is
// a real possibility — and "Waiting for material: 74368-6SA0A.06" would be a
// confident claim derived from an unclassified error. It renders "Waiting for
// material" instead (F7).
// payloadCode and deliveryNode come from the order at replay, or from the parsed
// envelope at intake where no order row exists yet — hence plain values rather
// than an *orders.Order.
func queueParamsForCapacity(d *capacityDetail, payloadCode, deliveryNode string) QueueParams {
	if d == nil {
		return QueueParams{}
	}
	p := QueueParams{Step: d.Step, HasStep: d.HasStep}
	switch d.Kind {
	case capacitySlot:
		// A genuine dropoff-capacity wait: the group IS the destination here.
		p.Destination = d.Group
		if p.Destination == "" {
			p.Destination = deliveryNode
		}
	case capacityPayload:
		p.Payload = payloadCode
		p.Group = d.Group
	case capacityBin:
		p.Kind = "empty"
		p.Group = d.Group
	default: // capacityUnknown — say only what we know.
		p.Group = d.Group
	}
	return p
}

// capacityDetailFrom narrows the ResolutionCapacity payload. Older call sites
// type-asserted a bare capacityKind; the payload is now a *capacityDetail.
func capacityDetailFrom(payload any) *capacityDetail {
	d, _ := payload.(*capacityDetail)
	return d
}

// kindOf is the nil-safe kind accessor for a capacity payload.
func (d *capacityDetail) kindOf() capacityKind {
	if d == nil {
		return capacityUnknown
	}
	return d.Kind
}
