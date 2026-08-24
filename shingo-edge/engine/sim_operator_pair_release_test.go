//go:build sim

package engine

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/config"
)

// ---------------------------------------------------------------------------
// The sim operator can push the RELEASE BUTTON, not just release one leg.
//
// ReleaseOrderWithLineside is the per-ORDER API door; ReleaseStagedOrders is
// the per-NODE one, and it is the only thing an operator can actually press.
// While the per-leg path was the only one a sim ran, everything the pair path
// owns had no sim coverage at all: the collision gate that holds a placing leg
// while its sibling is still coming, the produce paperwork's ordering against
// that gate, the deferred-sibling re-fire, and the disposition split.
// ---------------------------------------------------------------------------

// TestSimOperator_SwapReleaseDelayIsConfigurable pins the knob. The default is
// a good imitation of a person and a poor instrument: a scenario built to
// observe a HELD release has three seconds to look before the operator releases
// anyway, and a run against the round-4 collision gate lost that window 480
// times to this timer.
func TestSimOperator_SwapReleaseDelayIsConfigurable(t *testing.T) {
	t.Parallel()
	op := newTestSimOperator(nil)
	if got := op.swapReleaseDelay(); got != defaultSwapReleaseDelay {
		t.Errorf("unset delay = %s, want the %s default", got, defaultSwapReleaseDelay)
	}
	op.ops = config.SimOperatorsConfig{SwapRelease: 90 * time.Second}
	if got := op.swapReleaseDelay(); got != 90*time.Second {
		t.Errorf("configured delay = %s, want 90s — a window the scenario can actually look through", got)
	}
}

// TestSimOperator_PairReleaseDrivesTheNodeDoor is the mode itself: with
// pair_release on, a staged leg releases through ReleaseStagedOrders and the
// whole pair goes.
func TestSimOperator_PairReleaseDrivesTheNodeDoor(t *testing.T) {
	t.Parallel()
	eng, nodeID, evacID, supplyID := seedSwapPairAt(t,
		protocol.SwapModeTwoRobotPressIndex, protocol.StatusStaged, protocol.StatusStaged)

	op := &simOperator{
		e:   eng,
		ops: config.SimOperatorsConfig{PairRelease: true},
	}
	op.releaseAsPair(evacID)

	// BOTH legs moved. The per-leg path would have released one and left the
	// other for its own staged event, which is the difference under test.
	for _, id := range []int64{evacID, supplyID} {
		o, err := eng.db.GetOrder(id)
		testutil.MustNoErr(t, err, "read leg back")
		if o.Status == protocol.StatusStaged {
			t.Errorf("leg %d is still staged — the node door releases the PAIR", id)
		}
	}
	_ = nodeID
}

// TestSimOperator_PairReleaseSendsCaptureLinesideOnProduce pins the computed
// disposition.
//
// A blank disposition is what the per-leg path sends, and it is exactly what
// keeps the U1 side-cycle trigger dormant: the trigger fires on
// capture_lineside, so a sim that always sends "" can never observe it. A
// produce node's operator is declaring a full bin.
func TestSimOperator_PairReleaseSendsCaptureLinesideOnProduce(t *testing.T) {
	t.Parallel()
	// Real steps: isSupplyOrderInTwoRobotSwap refuses a leg it cannot classify,
	// so a stepless fixture would exercise the refusal instead of the path.
	eng, nodeID, evacID, _ := seedStagedPressIndexPair(t, "uuid-simpair")

	rec := &recordingLoaderStore{}
	eng.loaderStore = rec

	op := &simOperator{e: eng, ops: config.SimOperatorsConfig{PairRelease: true}}
	op.releaseAsPair(evacID)

	// The U1 lookup happening at all is the observable: it is reached only from
	// the capture_lineside branch on a produce claim.
	if len(rec.asked) == 0 {
		t.Errorf("the pair release sent no capture_lineside disposition — with a blank one the "+
			"U1 side-cycle trigger is unreachable, which is how it stayed unobservable on every "+
			"sim run so far (node %d)", nodeID)
	}
}

// TestSimOperator_PairReleaseReportsAHoldAsAHold: the collision gate refusing is
// the gate WORKING. A run that logs every hold as a rejection is a run nobody
// can tell a wedge from.
func TestSimOperator_PairReleaseReportsAHoldAsAHold(t *testing.T) {
	t.Parallel()
	eng, _, evacID, _ := seedSwapPairAt(t,
		protocol.SwapModeTwoRobotPressIndex, protocol.StatusQueued, protocol.StatusStaged)

	var lines []string
	eng.logFn = func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }

	op := &simOperator{e: eng, ops: config.SimOperatorsConfig{PairRelease: true}}
	op.releaseAsPair(evacID)

	found := false
	for _, l := range lines {
		if containsAll(l, "HELD") {
			found = true
		}
	}
	if !found {
		t.Errorf("a held release was not reported as HELD; log was %v", lines)
	}
}
