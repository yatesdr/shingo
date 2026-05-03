// wiring_completion_uop.go — pure UOP-decision functions extracted
// from the nine completion handlers in wiring_completion.go.
//
// Phase 3 pre-work (plan §Phase 3, Dev 4 + Dev 3): the nine completion
// handlers share two UOP decisions — "replenish from a delivered bin"
// and "clear to zero." Each was inlined with subtle differences in
// fallback handling, making a holistic refactor risky.
//
// Extracting them into pure functions accomplishes two things:
//
//   1. Tests pin the decision matrix once, decoupled from handler
//      orchestration. Phase 3's authority flip changes ONE function
//      (resolveReplenishUOP becomes "read bin authoritative via Core"
//      instead of "read OrderDelivered.BinUOPRemaining snapshot")
//      rather than nine handlers.
//
//   2. The remaining handler bodies stay small enough to read at a
//      glance: state-machine transitions, side-cycle dispatch, etc.
//      The "what UOP value goes here?" question moves out.
//
// This is a pure refactor — no behavior change. All existing wiring
// tests pass unchanged.
package engine

import (
	"shingo/protocol"
)

// resolveReplenishUOP returns the runtime UOP value to set when a bin
// arrives at a node via a delivery completion (handleNormalReplenishment,
// handleChangeoverRelease, handleComplexOrderBCompletion, the
// done-branch of handleKeepStagedOrderBCompletion).
//
// Decision matrix (post-Item-8):
//
//   - role == Produce → 0. A produce node receives an empty bin; the
//     line ticks UP into it.
//   - role != Produce → claimCapacity. The runtime is initialized to
//     "assume a full bin"; the reconciler then heals from Core's
//     authoritative read on the next pass (typically within 60s).
//
// Pre-Item-8 this function consulted an OrderDelivered.BinUOPRemaining
// snapshot to handle partial-back returns directly. Item 8 retired
// the snapshot — the runtime cache (kept in lockstep with Core by the
// reconciler) is now the source of truth. The trade-off — a brief
// "looks like full bin" UI on partial-back returns until the heal —
// is SME-accepted.
func resolveReplenishUOP(role protocol.ClaimRole, claimCapacity int) int {
	if role == protocol.ClaimRoleProduce {
		return 0
	}
	return claimCapacity
}
