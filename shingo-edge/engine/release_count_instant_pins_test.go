package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// A release's count and its fence are one instant (decision 44a). The fence is
// read at the commit, under countMu and the accumulator's flush lock; the count
// with nothing typed used to be the leg's plan-time read of the seat, and
// between the two sits the act's Core round trip for the release point. A
// stroke in that gap was inside AsOfNet and outside the count, so both books
// agreed on a count high by the stroke.

// strokeDuringTheAct builds a steady consume swap with its evac staged and
// has the fake Core's release-point answer record one stroke at the seat
// before it answers: a stroke between the leg's runtime read and its commit.
func strokeDuringTheAct(t *testing.T) *relHarness {
	t.Helper()
	h := newRelHarness(t)
	pairAt(pairSpec{mode: protocol.SwapModeTwoRobot, role: protocol.ClaimRoleConsume},
		"evac", protocol.StatusStaged, "supply", protocol.StatusDispatched)(h)
	style, err := h.db.GetActiveStyleID(h.processID)
	testutil.MustNoErr(t, err, "read the active style")
	h.core.mu.Lock()
	answer := h.core.points
	h.core.points = func(req protocol.ReleasePointsRequest) protocol.ReleasePointsResponse {
		h.eng.handleCounterDelta(CounterDeltaEvent{ProcessID: h.processID, StyleID: *style, Delta: 1})
		return answer(req)
	}
	h.core.mu.Unlock()
	h.mark()
	return h
}

// TestRelease_AStrokeDuringTheActIsInTheDeclaredCount: the seat reads 42 when
// the leg is planned, one stroke lands while the act asks Core for the release
// point, and the release declares 41 — on the envelope, on the wire
// disposition and on the seat — with the stroke inside the fence. An untouched
// pre-fill (42 sent back as suggested) is Shingo's number and takes the seat's
// count, with the suggestion set to it (no override row at Core); a count the
// person changed, or one sent with no suggestion, is kept.
func TestRelease_AStrokeDuringTheActIsInTheDeclaredCount(t *testing.T) {
	t.Parallel()
	n := func(v int) *int { return &v }
	cases := []struct {
		name              string
		count, suggested  *int
		want              int
		wantSuggested     int
		wantSeatAfter     int
		wantFenceDrawsOne bool
	}{
		{name: "no count", want: fxCount - 1, wantSuggested: -1, wantSeatAfter: fxCount - 1, wantFenceDrawsOne: true},
		{name: "the untouched pre-fill", count: n(fxCount), suggested: n(fxCount),
			want: fxCount - 1, wantSuggested: fxCount - 1, wantSeatAfter: fxCount - 1, wantFenceDrawsOne: true},
		{name: "a changed count", count: n(30), suggested: n(fxCount),
			want: 30, wantSuggested: fxCount, wantSeatAfter: 30, wantFenceDrawsOne: true},
		{name: "a count with no suggestion", count: n(fxCount),
			want: fxCount, wantSuggested: -1, wantSeatAfter: fxCount, wantFenceDrawsOne: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := strokeDuringTheAct(t)
			disp := ReleaseDisposition{Mode: DispositionSendPartialBack, PartialCount: c.count,
				PartialCountSuggested: c.suggested, CalledBy: "operator"}
			testutil.MustNoErr(t, h.eng.ReleaseOrderWithLineside(h.leg("evac"), disp), "release the evac")
			envs := h.releaseEnvelopes()["evac"]
			if len(envs) != 1 {
				t.Fatalf("the evac got %d OrderRelease envelopes, want 1", len(envs))
			}
			rel := envs[0]
			if rel.RemainingUOP == nil || *rel.RemainingUOP != c.want {
				t.Errorf("RemainingUOP = %v, want %d", deref(rel.RemainingUOP), c.want)
			}
			if d := rel.Disposition; d == nil || d.Kind != protocol.DispositionReleasePartial || d.Count != c.want {
				t.Errorf("disposition = %+v, want release_partial count %d", d, c.want)
			} else if got := deref(d.CountSuggested); got != c.wantSuggested {
				t.Errorf("disposition CountSuggested = %d, want %d (-1: none)", got, c.wantSuggested)
			}
			if rel.AsOf == nil {
				t.Fatal("the release carries no fence")
			}
			if c.wantFenceDrawsOne && rel.AsOf.AsOfNet != -1 {
				t.Errorf("fence AsOfNet = %d, want -1: the stroke is inside the fence", rel.AsOf.AsOfNet)
			}
			rt, err := h.db.GetProcessNodeRuntime(h.nodeID)
			testutil.MustNoErr(t, err, "read the seat")
			if rt.RemainingUOPCached != c.wantSeatAfter {
				t.Errorf("seat after the release = %d, want %d (the count declared)", rt.RemainingUOPCached, c.wantSeatAfter)
			}
		})
	}
}

// deref reads an optional count for a message; -1 for none.
func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}
