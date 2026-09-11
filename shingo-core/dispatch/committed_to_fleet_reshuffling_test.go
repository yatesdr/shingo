package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/store/orders"
)

// TestSwapLegCommittedToFleet_ReshufflingIsNotCommitted pins the arm that was
// correct by accident.
//
// A leg waiting on a dig wears `reshuffling`. It used to land in the `default`
// branch and read as NOT COMMITTED — the right answer, reached without anyone
// deciding it, and undocumented: the predicate's header enumerates the
// not-committed cases as "acquiring" and "failure", and `reshuffling` is
// neither.
//
// ── WHY THE ACCIDENT IS DANGEROUS ─────────────────────────────────────────
//
// The plausible mistake is specific. A leg in `reshuffling` is visibly DOING
// something — robots moving, blockers being carried out — so adding it to the
// committed list reads like a correction rather than a change. It is not: a
// holder mid-dig has collected nothing yet.
//
// ── WHAT IT PINS NOW ──────────────────────────────────────────────────────
//
// The predicate's callers are the lane hand-off gates in dig_lock_release.go
// (handOffDugLane's gate 3 and the two beside it), where this answer decides
// what happens to a holder's dig row and its corridor. The swap gates it was
// written for are deleted. The pair's own version of the question — a partner
// digging its own bin out holds its partner back — is pinned by behaviour, not
// by this predicate: TestPairRule_ASupplyDiggingForItsBinHoldsItsEvac (census 1).
//
// ── AND IT IS AN ORDINARY STATE ───────────────────────────────────────────
//
// Under §R.91 the demand that raises a dig BECOMES the dig's parent and wears
// `reshuffling` while it runs, so a leg in `reshuffling` is routine, not rare.
//
// MUTATION (verified): move StatusReshuffling into the committed list and this
// fails.
func TestSwapLegCommittedToFleet_ReshufflingIsNotCommitted(t *testing.T) {
	t.Parallel()

	if swapLegCommittedToFleet(&orders.Order{Status: StatusReshuffling}) {
		t.Error("a leg mid-dig reads as COMMITTED TO THE FLEET. It holds no vendor order, is not en " +
			"route and has collected nothing — and the lane hand-off gates in dig_lock_release.go read " +
			"this answer to decide what happens to its dig row and its corridor.")
	}
}

// TestSwapLegCommittedToFleet_CoversEveryStatusDeliberately is the totality
// half: every status this predicate can be asked about has a stated answer, so
// a new status cannot arrive and silently take `default`.
//
// The predicate is asked about a SIBLING, which can be in any status a swap leg
// reaches. Reading `default: return false` as a decision is the mistake this
// whole entry is about — it is a fallback, and a fallback that happens to be
// right is indistinguishable from one that is not.
func TestSwapLegCommittedToFleet_CoversEveryStatusDeliberately(t *testing.T) {
	t.Parallel()

	// committed: the fleet has this leg and is acting on it.
	for _, s := range []struct {
		status protocol.Status
		why    string
	}{
		{StatusDispatched, "handed to the fleet"},
		{StatusInTransit, "a robot is carrying it"},
		{StatusStaged, "parked mid-plan, still the fleet's"},
		{StatusDelivered, "done its part"},
		{StatusConfirmed, "done and acknowledged"},
	} {
		if !swapLegCommittedToFleet(&orders.Order{Status: s.status}) {
			t.Errorf("%s reads as NOT committed (%s) — its partner would keep waiting on a leg that "+
				"is already doing the work, which is the deadlock direction", s.status, s.why)
		}
	}

	// not committed: the fleet does not have this leg, or will not act on it.
	for _, s := range []struct {
		status protocol.Status
		why    string
	}{
		{StatusPending, "not yet acquiring"},
		{StatusQueued, "acquiring — the state a leg is held FROM"},
		{StatusSourcing, "acquiring"},
		{StatusReshuffling, "mid-dig; has not begun this swap's own work"},
		{StatusFailed, "will not do its part; a recovery may still come"},
		{StatusCancelled, "will not do its part"},
	} {
		if swapLegCommittedToFleet(&orders.Order{Status: s.status}) {
			t.Errorf("%s reads as COMMITTED (%s) — its partner would release early, and on the "+
				"supply side that is the line clearing with no replacement coming", s.status, s.why)
		}
	}
}
