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
