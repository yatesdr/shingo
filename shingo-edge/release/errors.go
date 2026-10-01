package release

import (
	"fmt"
	"strings"
)

// CurtainHeldError is the light curtain's refusal. Its text is the sentence
// the operator sees: on the toast for a click, on the order's chip for an
// automatic release.
type CurtainHeldError struct {
	Node     string
	Sentence string
}

func (c *CurtainHeldError) Error() string { return c.Sentence }

// SwapPairNotReadyError refuses a RELEASE that would drop a bin onto a press
// its sibling has not cleared yet.
//
// ADVISORY: nothing is broken and nothing needs fixing. The other robot is on
// its way, and the operator's only correct action is to click again once it
// arrives. Rendered red it reads as a fault to escalate; the Advisory() marker
// is what makes the station show it as a notice.
type SwapPairNotReadyError struct {
	NodeName     string
	SiblingState string
}

func (e *SwapPairNotReadyError) Error() string {
	return fmt.Sprintf("node %s: the other robot has not cleared the press yet (%s) — "+
		"release again once it is staged", e.NodeName, e.SiblingState)
}

// Advisory marks this as the system working rather than a fault.
func (e *SwapPairNotReadyError) Advisory() bool { return true }

// QueueReasonSuffix renders Core's mirrored blocking reason for an operator-
// facing refusal, or "" when Core has not told us one. Leading separator
// included so callers can append it unconditionally.
func QueueReasonSuffix(reason, code string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ""
	}
	if code = strings.TrimSpace(code); code != "" {
		return fmt.Sprintf(" — %s (%s)", reason, code)
	}
	return " — " + reason
}
