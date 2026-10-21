package protocol

import "time"

// Wait kinds: who may advance a wait step. Core and the Edge read the same
// field off the same steps, so the values and the predicate live here once.

// WaitKindStation marks a wait the STATION advances (the operator's RELEASE):
// a swap choreography's wait, a tooling hold. The Edge stamps it on every wait
// it authors.
const WaitKindStation = "station"

// WaitKindLane marks a wait only Core's lane evaluator may advance. Core stamps
// it on the lane waits it splices into a plan.
const WaitKindLane = "lane"

// WaitKindVacate marks a wait Core releases once a named bin has left a named
// node — the vacate fact, read from the lift (bins.last_lift_at), never from
// the bin's recorded node alone. Core reaches it two ways
// (dispatch.coreOwnsWait): stored on a wait of a plan Core authors, or
// derived, when a station wait's next drop carries a held vacate stamp
// because the station released it before the bin had left. On the wire it is
// what Core tells the station a staged leg is waiting on.
const WaitKindVacate = "vacate"

// IsCoreWaitKind reports whether a wait of this kind is Core's to release: a
// lane wait, a vacate wait, and any kind this build does not know. Untagged is
// not: an older Core sends no kind, and the station keeps the point it last
// recorded.
//
// AN UNKNOWN KIND IS CORE'S, NEVER THE STATION'S. Core and the Edge deploy at
// different times, and a new kind is added on the side that releases it: an
// Edge that read a newer Core's kind as station-owned would offer RELEASE on a
// leg Core is holding, and its sim operator would press it. Reading it as
// Core's costs only a button that waits for Core's next OrderStaged. So this
// is the complement of IsStationWaitKind, and every kind is exactly one side's
// (pinned by TestWaitKindsPartition).
func IsCoreWaitKind(kind string) bool {
	return !IsStationWaitKind(kind)
}

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

// VacateStamp is the vacate stamp on a dropoff: what Core must see leave a
// node before the drop goes. Core writes it on the plans it persists
// (plan.Vacate is this type) and reads it to decide who owns a wait
// (dispatch.coreOwnsWait); it does not cross the wire, and the Edge sees only
// its consequence, the kind WaitKindVacate on OrderStaged. It is declared here
// beside the wait kinds so the kinds, the ownership predicate and the stamp
// have one home (pinned by TestWaitOwnershipIsDeclaredOnce).
//
// It records what the vacated-slot rule counted when it admitted a drop, or
// what Core must see leave a node (the sibling stamp, a home clear's partial
// move). Partner is 0 for an own-lift (a) grant, which is never held — one
// robot lifts before it drops — and for a home clear's stamp, which Core holds
// before dispatch (shingo-core dispatch/clear_home.go, clearDropHeld).
type VacateStamp struct {
	Node    string `json:"node"`
	Bin     int64  `json:"bin"`
	Partner int64  `json:"partner,omitempty"`
	// Kind is "" for the rule's grant and VacateKindSibling for the stamp Core
	// writes at release on a grant's sibling. Only a grant admits a drop:
	// plan.PartnerStamp, which the slot claim's partner arm reads, never
	// returns a sibling stamp.
	Kind string `json:"kind,omitempty"`
	// StampedAt is when the stamp was written, on Core's clock — the clock the
	// lift stamps bins.last_lift_at with. The vacate fact is a lift after it
	// (bins.LeftSince). Set on a partner stamp only: an own-lift (a) grant is
	// never held.
	StampedAt *time.Time `json:"stamped_at,omitempty"`
	// HeldAt is set when the station released the wait before this drop while
	// the bin still stood on the node. From then the wait is Core's
	// (dispatch.coreOwnsWait) until the bin lifts.
	HeldAt *time.Time `json:"held_at,omitempty"`
}

// VacateKindSibling marks the stamp on a grant's sibling: a leg dropping on a
// node holding a bin its stamped, staged sibling has claimed and will lift.
const VacateKindSibling = "sibling"
