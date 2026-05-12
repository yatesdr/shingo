package dispatch

import (
	"fmt"
	"strings"
)

// resolvedStep is a step with concrete node names after resolution.
type resolvedStep struct {
	Action string `json:"action"`
	Node   string `json:"node,omitempty"`
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
