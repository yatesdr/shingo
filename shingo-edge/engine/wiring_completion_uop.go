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

// resolveReplenishUOP returns the runtime UOP value to set on a delivery
// completion. Produce nodes start at 0 (line ticks UP into the bin);
// other roles reset to full claim capacity (line ticks DOWN from there).
//
// The binID parameter is accepted for symmetry with other completion-
// path helpers but doesn't affect the value — even when no bin is
// arriving (removal completion), the cached UOP stays at capacity.
// active_bin_id (set separately on the same write) carries the
// "is there a bin here?" signal; binAtNode keys off it so PLC ticks
// don't attribute to an empty slot regardless of the cached UOP value.
func resolveReplenishUOP(role protocol.ClaimRole, claimCapacity int, binID *int64) int {
	_ = binID
	if role == protocol.ClaimRoleProduce {
		return 0
	}
	return claimCapacity
}
