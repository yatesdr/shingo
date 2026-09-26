package dispatch

import (
	"testing"

	"shingocore/dispatch/plan"
)

// TestPlanCausesAreDeclaredQueueCauses holds the planner's verdict vocabulary to
// dispatch's queue vocabulary: every plan.Cause is the value of a QueueCause
// declared in queue_cause.go, so the translation at sourceResultOf is a change
// of type and never of meaning, and every wait a plan can report has a
// causeReleasers row (TestEveryQueueCauseHasAReleaser covers the declared set).
//
// MUTATION: add a plan.Cause with a value no QueueCause declares — this names it.
func TestPlanCausesAreDeclaredQueueCauses(t *testing.T) {
	t.Parallel()
	declared := map[QueueCause]bool{}
	for _, dc := range declaredQueueCauses(t) {
		declared[dc.value] = true
	}
	for _, c := range plan.Causes {
		if !declared[queueCauseOf(c)] {
			t.Errorf("plan.Cause %q is not a declared QueueCause: an order parked on this plan's verdict "+
				"would carry a cause with no name in queue_cause.go and no releaser", c)
		}
	}
}
