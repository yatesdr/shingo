package dispatch

import (
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

// stepSkipSummaries renders per-step skip summaries as compact "step N at
// NODE: REASON" tuples for the order-level missed-step rollup log line.
func stepSkipSummaries(skips []pickupSkip) []string {
	out := make([]string, 0, len(skips))
	for _, s := range skips {
		out = append(out, fmt.Sprintf("step %d at %s: %s", s.stepIndex, s.nodeName, s.reason))
	}
	return out
}

// emptyNodeSkipReason is the literal reason string set in ApplyComplexPlan
// when ListBinsByNode returns zero rows for a pickup step. allStepSkipsAreEmptyNode
// keys on this exact value to distinguish "source was emptied externally"
// (terminal-skip) from "bins were there but rejected" (terminal-fail).
const emptyNodeSkipReason = "no bins at node"

// allStepSkipsAreEmptyNode reports whether every pickup-step skip was the
// "no bins at node" empty-source case — the signal that the order can be
// safely skipped rather than failed. Returns false for an empty input
// (zero skips means zero pickup steps, which is a different bug — the
// dispatcher should surface that as a malformed order, not auto-skip).
func allStepSkipsAreEmptyNode(skips []pickupSkip) bool {
	if len(skips) == 0 {
		return false
	}
	for _, s := range skips {
		if s.reason != emptyNodeSkipReason {
			return false
		}
	}
	return true
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
