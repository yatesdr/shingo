package protocol

// Wait kinds: who may advance a wait step. Core and the Edge read the same
// field off the same steps, so the values and the predicate live here once.

// WaitKindStation marks a wait the STATION advances (the operator's RELEASE):
// a swap choreography's wait, a tooling hold. The Edge stamps it on every wait
// it authors.
const WaitKindStation = "station"

// WaitKindLane marks a wait only Core's lane evaluator may advance. Core stamps
// it on the lane waits it splices into a plan.
const WaitKindLane = "lane"

// IsStationWaitKind reports whether a station may advance a wait of this kind.
//
// THE DRAIN WINDOW LIVES HERE, in one place, so there is exactly one thing to
// change when it closes. Untagged reads as station-owned today — the
// historical default, so no plan in flight changes meaning — and the drift
// tests (TestEveryEdgeAuthoredWaitIsStamped, TestSplice_FenceHoldsOnASplicedPlan)
// already fail on any NEW untagged wait from either author. When the last
// pre-ruling order has drained, delete the `== ""` arm and an untagged wait
// becomes what it should be: unowned, and refused by both fences.
func IsStationWaitKind(kind string) bool {
	return kind == WaitKindStation || kind == ""
}
