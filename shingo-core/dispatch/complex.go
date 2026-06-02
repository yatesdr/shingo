package dispatch

import (
	"errors"
	"fmt"
	"strings"
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
// bin. Surfaced to production logs by claimComplexBins so silent claim
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

// emptyNodeSkipReason is the literal reason string set in claimComplexBins
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
	// ResolutionCapacity covers the NGRP-saturated and
	// NGRP-empty shapes ("no available slot in node group N", "no
	// bin of requested payload in node group N"). Intake queues the
	// order with queue_reason = err.Error(); the scanner replays.
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

// classifyResolutionError inspects err and returns the typed class
// plus the typed-payload pointer when the class carries one
// (*BuriedError for ResolutionBuried, *StructuralError for
// ResolutionStructural). The payload is nil for the other classes.
//
// Replaces the v6 pair (isCapacityResolutionError + isBuriedResolutionError)
// with a single decision point so both the complex-intake and the
// simple-retrieve paths route through the same classifier. Buried
// detection uses errors.Is against the ErrBuried sentinel so it
// survives any wrap chain; capacity detection still uses substring
// match against the resolver's stable error shapes (the resolver
// returns plain `fmt.Errorf` for those, with no typed sentinel to
// match — a future cleanup would type those too).
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
	msg := err.Error()
	if strings.Contains(msg, "no available slot in node group") ||
		strings.Contains(msg, "no bin of requested payload in node group") {
		return ResolutionCapacity, nil
	}
	// DB-layer wraps that aren't structural or capacity.
	if strings.Contains(msg, "list children of") ||
		strings.Contains(msg, "get target depth") ||
		strings.Contains(msg, "list lane slots") {
		return ResolutionTransient, nil
	}
	return ResolutionFatal, nil
}
