package dispatch

import (
	"encoding/json"
	"errors"
	"shingocore/dispatch/plan"
	"strconv"
	"strings"

	"shingo/protocol"
)

// resolvedStep is a step with concrete node names after resolution: the
// planner's plan.Step (the leg shape, B1), under the name every dispatch reader
// of steps_json uses.
type resolvedStep = plan.Step

// The wait kinds, declared with the step they tag (plan/legs.go).
const (
	WaitKindLane    = plan.WaitKindLane
	WaitKindStation = plan.WaitKindStation
)

// CoreOwnsWaitAt reports whether the wait an order is parked at is CORE's to
// advance — the read behind the hard-release affordance and anything else that
// must distinguish the two owners from outside this package.
//
// It is the exact complement of protocol.IsStationWaitKind over a real plan, so the drain
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
	return !protocol.IsStationWaitKind(w.WaitKind)
}

// claimedBin records which bin was claimed at which pickup step.
type claimedBin struct {
	binID     int64
	stepIndex int
	nodeName  string
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
	// capacityEmptiesOnly: the destination group declares a maintain level, which
	// makes it an empties bank, and this store carries a payload. Grouped with
	// the capacity shapes because it must reach the SAME disposition they do —
	// queue rather than terminal-fail, and let the group's configured overflow
	// destination catch it at admission. It is not a shortage of anything, so it
	// gets its own kind rather than borrowing capacitySlot's: an operator told
	// "waiting for a slot" would go looking for a full group and find an empty
	// one.
	capacityEmptiesOnly
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
	//
	// The buried and structural arms are the planner's reading
	// (plan.BuriedOrStructural): the picker asks the same two questions of
	// the same errors, so the two cannot disagree.
	if buriedErr, structErr := plan.BuriedOrStructural(err); buriedErr != nil {
		return ResolutionBuried, buriedErr
	} else if structErr != nil {
		return ResolutionStructural, structErr
	}
	if errors.Is(err, ErrBuried) {
		// Sentinel matched but typed extraction failed — treat as
		// fatal rather than crashing.
		return ResolutionFatal, nil
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
	// The four capacity shapes are the planner's to name (plan.CapacityShape), so
	// the vacated-slot arm, which asks the same question, cannot disagree with
	// this classifier. Most specific first: the empties-only statement is about
	// the same destination as the slot shape, and the group may well have free
	// slots. MATERIAL (the group has room but not the CONTENTS) is kept apart
	// from SLOT — see the capacityKind doc comment (F1).
	switch plan.CapacityShape(err) {
	case plan.CapacityEmptiesOnly:
		return detail(capacityEmptiesOnly)
	case plan.CapacitySlot:
		return detail(capacitySlot)
	case plan.CapacityPayload:
		return detail(capacityPayload)
	case plan.CapacityBin:
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
	case capacitySlot, capacityEmptiesOnly:
		// capacityEmptiesOnly waits on a DESTINATION, not on material: the
		// carrier is in hand and has nowhere to go. Nothing anybody brings to
		// the plant releases it.
		return protocol.QueueWaitingForSlot
	default:
		return protocol.QueueWaitingForMaterial
	}
}

// causeForCapacity gives a classified capacity shape its own queue cause, or
// zero to keep the call site's coarse cause.
//
// ONLY THE NEW SHAPE OVERRIDES. The two call sites write CauseIntakeResolve and
// CauseNGRPResolve — the site's own name for "resolution could not place this",
// which is honest for the shortage shapes, where the group really is short.
// Rewriting those would re-code rows at both plants for no gain. The
// empties-only refusal is a different question with a different answer ("this
// will never fit, look at the overflow"), so it names itself and the rest are
// left alone.
func causeForCapacity(k capacityKind, siteCause QueueCause) QueueCause {
	if k == capacityEmptiesOnly {
		return CauseGroupHoldsEmptiesOnly
	}
	return siteCause
}

// queueParamsForCapacity builds the sentence params for a classified capacity
// error. It is the single place the F1 location rule is enforced: a payload
// shortage names the GROUP it is short in, never the order's lineside delivery
// node, because the operator has to go look in the group.
//
// For capacityUnknown the payload is deliberately withheld. Classification is
// substring matching over untyped resolver errors, so an unrecognised message is
// a real possibility — and "Waiting for material: SYN-PART06A.06" would be a
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
	case capacitySlot, capacityEmptiesOnly:
		// A destination-side wait: the group IS the destination here.
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
