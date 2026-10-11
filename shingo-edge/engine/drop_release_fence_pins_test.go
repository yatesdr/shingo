package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// The drop-changeover release is fenced like the lineside release (Lane H
// D11 (i), the drop path D20 left bumping at the release). A generation ends at
// the lift, not at the release: the release writes the declared count on the
// seat and carries the stream fence, so Core declares it with no bump and the
// ticks until the lift stay the carrier's in both books (R1); a drop released
// as empty is not charged afterwards (R3).

// dropConsumeRelease builds a consume cell whose incoming style drops the
// node, with its evac leg staged, and releases it with d.
func dropConsumeRelease(t *testing.T, d ReleaseDisposition) (*relHarness, []protocol.OrderRelease) {
	t.Helper()
	h := newRelHarness(t)
	coAt(coSpec{mode: protocol.SwapModeTwoRobot, role: protocol.ClaimRoleConsume, drop: true}, "evac", protocol.StatusStaged)(h)
	h.mark()
	testutil.MustNoErr(t, h.eng.ReleaseOrderWithLineside(h.leg("evac"), d), "drop release")
	envs := h.releaseEnvelopes()["evac"]
	if len(envs) != 1 {
		t.Fatalf("drop release sent %d OrderRelease envelopes for the evac leg, want 1", len(envs))
	}
	return h, envs
}

// TestPinDropRelease_IsFencedAndChargesTheBinUntilTheLift: SEND PARTIAL BACK 7
// on a consume drop. The release carries the fence for the seat's carrier and
// the seat reads the declared 7, so a tick after the release lands on 7 in
// both books.
func TestPinDropRelease_IsFencedAndChargesTheBinUntilTheLift(t *testing.T) {
	t.Parallel()
	h, envs := dropConsumeRelease(t, dispPartial(7))
	rel := envs[0]
	if rel.RemainingUOP == nil || *rel.RemainingUOP != 7 {
		t.Fatalf("release RemainingUOP = %v, want 7", rel.RemainingUOP)
	}
	if rel.AsOf == nil {
		t.Fatalf("the drop release carries no fence: Core bumps the generation at the release, and the ticks " +
			"until the lift are dropped as stale (R1: a generation ends at the lift)")
	}
	if rel.AsOf.BinID != fxBin {
		t.Errorf("fence bin = %d, want the seat's carrier %d", rel.AsOf.BinID, fxBin)
	}
	rt, err := h.db.GetProcessNodeRuntime(h.nodeID)
	testutil.MustNoErr(t, err, "read the seat")
	if rt.RemainingUOPCached != 7 {
		t.Errorf("seat count after the release = %d, want the declared 7 (the count the fence is as of)", rt.RemainingUOPCached)
	}
	if rt.ReleasedEmpty {
		t.Errorf("a release of 7 marked the seat released-empty")
	}
}

// TestPinDropRelease_EmptyIsNotChargedAfter: SEND PARTIAL BACK 0 on a consume
// drop marks the seat released-empty in the count write (R3), and the release
// is fenced.
func TestPinDropRelease_EmptyIsNotChargedAfter(t *testing.T) {
	t.Parallel()
	h, envs := dropConsumeRelease(t, dispPartial(0))
	if envs[0].AsOf == nil {
		t.Errorf("the empty drop release carries no fence")
	}
	rt, err := h.db.GetProcessNodeRuntime(h.nodeID)
	testutil.MustNoErr(t, err, "read the seat")
	if rt.RemainingUOPCached != 0 || !rt.ReleasedEmpty {
		t.Errorf("seat after an empty drop release: count %d released_empty %t, want 0 and true (R3)",
			rt.RemainingUOPCached, rt.ReleasedEmpty)
	}
}
