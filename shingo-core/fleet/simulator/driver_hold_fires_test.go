//go:build sim

package simulator

import (
	"testing"
	"time"

	"shingocore/config"
	"shingocore/fleet"
)

// stubGate holds every placement behind a fixed owner, so the driver's
// not-a-queue diagnostic can be driven without an engine.
type stubGate struct {
	blocker *int64 // the owner the gate reports for the blocking bin
}

func (g stubGate) CanEnterPosition(vendorOrderID, location, binTask string) fleet.PositionHold {
	if binTask != "JackUnload" {
		return fleet.PositionHold{}
	}
	return fleet.PositionHold{
		Held:             true,
		Reason:           "stub: position occupied",
		BlockerClaimedBy: g.blocker,
	}
}

// THE CLOCK THE DIAGNOSTIC FIRES ON SPLITS ON WHO OWNS THE BLOCKER.
//
// A blocker claimed by NOBODY means the hold can never clear — nothing is
// scheduled to move that bin — so the driver says so on the FIRST tick of the
// hold. Waiting the five-minute bound to state a settled fact only delayed the
// loudest signal a wedged run produces: in the demo-plant incident the line
// fired (would have fired) at minute five of a hold that had already cost two
// robots and two lineside positions by minute three.
func TestNotAQueueFiresImmediatelyWhenNobodyOwnsTheBlocker(t *testing.T) {
	d, s, m, em := newTestDriver(t, testCfgNoFail(), 1)
	_ = em // the emitter is not asserted here; the hold is the behaviour under test
	s.SetPositionGate(stubGate{blocker: nil})

	vid := mkTransport(t, s, "o1") // JackLoad@A, JackUnload@B
	runTicks(d, m, 20)             // through both transits, well inside the five-minute bound

	p := d.progress[vid]
	if p == nil || p.heldAt != "B" {
		t.Fatalf("order %s should be held at B, got %+v", vid, p)
	}
	if !p.heldWarned {
		t.Fatalf("a hold behind a blocker NOBODY owns fired no not-a-queue line in %s of holding — "+
			"the case is settled at tick one and must not wait out the five-minute bound",
			5*time.Second)
	}
}

// A blocker another order owns might still be a genuine queue: that order
// finishes, its bin leaves, the hold clears. The five-minute bound exists to
// out-wait exactly that, so the diagnostic must stay silent inside it.
func TestNotAQueueKeepsTheBoundWhenAnOrderOwnsTheBlocker(t *testing.T) {
	d, s, m, em := newTestDriver(t, testCfgNoFail(), 1)
	_ = em
	owner := int64(42)
	s.SetPositionGate(stubGate{blocker: &owner})

	vid := mkTransport(t, s, "o1")
	runTicks(d, m, int(4*time.Minute/time.Second))

	p := d.progress[vid]
	if p == nil || p.heldAt != "B" {
		t.Fatalf("order %s should be held at B, got %+v", vid, p)
	}
	if p.heldWarned {
		t.Fatalf("the not-a-queue line fired inside the bound (%s) behind an OWNED blocker — that "+
			"is a queue shape, and the bound exists so the diagnostic cannot cry deadlock on it",
			unresolvableHoldAfter)
	}

	// Past the bound it does fire: an owned blocker that out-waits the longest
	// genuine queue is no longer presumptively a queue either.
	runTicks(d, m, int(unresolvableHoldAfter/time.Second)+1)
	if !d.progress[vid].heldWarned {
		t.Fatalf("the not-a-queue line never fired after %s behind an owned blocker", unresolvableHoldAfter)
	}
}

func testCfgNoFail() (c config.SimConfig) {
	c.TransitTime = 5 * time.Second
	c.JitterPct = 0
	c.FailRate = 0
	return c
}
