package engine

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"shingo/protocol/testutil"
)

// operator_active_pull_test.go — the operator's explicit set/change control for
// which side of an A/B pair the line is pulling from.
//
// ── WHY THERE IS A SECOND DOOR AT ALL ─────────────────────────────────────
//
// The flip is and stays the CANONICAL writer of active_pull: it moves the line
// and writes the bit in the same click, which is what makes the two agree. What
// it cannot do is answer the state a tooling evacuate leaves behind —
// clearActivePullForEvacuate darkens BOTH sides, correctly ("the press is down,
// so the line is pulling from nothing"), and then nothing re-asserts the bit
// when the press comes back up. Both sides read 0, the release guard is silent
// on a running press, and the only click that fixes it is a flip the operator
// may not want (he may already be on the side he wants).
//
// So the operator gets to SAY which side the line is on. It is a declaration of
// fact, not a choreography step — and it carries the same posture as every other
// override in this family: audited by name, and closed to the PLC, which cannot
// see the aisle.

// pullBits reads both halves of the pair.
func pullBits(t *testing.T, eng *Engine, a, b int64) (bool, bool) {
	t.Helper()
	ra, err := eng.db.GetProcessNodeRuntime(a)
	testutil.MustNoErr(t, err, "runtime a")
	rb, err := eng.db.GetProcessNodeRuntime(b)
	testutil.MustNoErr(t, err, "runtime b")
	return ra.ActivePull, rb.ActivePull
}

// TestSetPullSide_WritesBothSidesCoherently is the state the control exists for:
// a tooling evacuate darkened both positions and nothing re-asserted either.
//
// ONE TRUE, PARTNER FALSE, and both in the same write. Two sides reading true is
// worse than two reading false — the release guard then refuses both, and the
// UOP tick attributes to whichever row it reads first.
func TestSetPullSide_WritesBothSidesCoherently(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, activeNodeID, parkedNodeID, _ := seedSequentialScenario(t, db, false)

	// What a tooling evacuate leaves: nobody is pulling from anything.
	testutil.MustNoErr(t, db.SetActivePull(activeNodeID, false), "darken SEQ-A")
	testutil.MustNoErr(t, db.SetActivePull(parkedNodeID, false), "darken SEQ-B")

	if err := eng.SetActivePullSide(parkedNodeID, OperatorFlip("op")); err != nil {
		t.Fatalf("the operator could not declare which side the line is pulling from: %v. After a "+
			"tooling evacuate both sides read 0 and nothing re-asserts the bit, so without this "+
			"control the release guard stays silent on a press that is running again.", err)
	}
	a, b := pullBits(t, eng, activeNodeID, parkedNodeID)
	if b != true || a != false {
		t.Fatalf("after declaring SEQ-B: SEQ-A=%v SEQ-B=%v, want SEQ-A=false SEQ-B=true. The pair is "+
			"one fact written twice — declaring one side must darken the other in the same write, or "+
			"the guard and the UOP tick read different answers.", a, b)
	}

	// And back again: the control CHANGES the side, not only sets it once.
	if err := eng.SetActivePullSide(activeNodeID, OperatorFlip("op")); err != nil {
		t.Fatalf("declaring the other side was refused: %v", err)
	}
	if a, b := pullBits(t, eng, activeNodeID, parkedNodeID); a != true || b != false {
		t.Fatalf("after declaring SEQ-A: SEQ-A=%v SEQ-B=%v, want SEQ-A=true SEQ-B=false", a, b)
	}
}

// TestSetPullSide_PLCCannotDeclare — same posture as the flip's own PLC arm. A
// bit cannot look at the aisle, and this control's entire content is a claim
// about what somebody looked at.
func TestSetPullSide_PLCCannotDeclare(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, activeNodeID, parkedNodeID, _ := seedSequentialScenario(t, db, false)
	testutil.MustNoErr(t, db.SetActivePull(activeNodeID, false), "darken SEQ-A")
	testutil.MustNoErr(t, db.SetActivePull(parkedNodeID, false), "darken SEQ-B")

	err := eng.SetActivePullSide(parkedNodeID, FlipRequest{ByPLC: true, Confirm: true, CalledBy: "plc"})
	if err == nil {
		t.Fatal("a PLC declared which side the line is pulling from. This control's whole content is " +
			"'a person looked at the aisle and this is what is true' — a PLC has no eyes, and letting " +
			"it write here would let a stale bit re-assert itself as an operator's statement.")
	}
	if a, b := pullBits(t, eng, activeNodeID, parkedNodeID); a || b {
		t.Errorf("the refused PLC call wrote anyway: SEQ-A=%v SEQ-B=%v", a, b)
	}
}

// TestSetPullSide_IsAudited — the write is an operator overriding the system's
// recorded belief about a physical fact, which is the same class as the release
// and flip confirms, and it is logged the same way: who, which pair, which way.
func TestSetPullSide_IsAudited(t *testing.T) {
	// No t.Parallel — stdlib log.SetOutput is global.
	db := testEngineDB(t)
	eng, _, activeNodeID, parkedNodeID, _ := seedSequentialScenario(t, db, false)
	testutil.MustNoErr(t, db.SetActivePull(activeNodeID, false), "darken SEQ-A")
	testutil.MustNoErr(t, db.SetActivePull(parkedNodeID, false), "darken SEQ-B")

	var buf bytes.Buffer
	prevW, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevW); log.SetFlags(prevFlags) })

	testutil.MustNoErr(t, eng.SetActivePullSide(parkedNodeID, OperatorFlip("press-2-op")),
		"declare SEQ-B")

	logged := buf.String()
	for _, want := range []string{"AUDIT", "SEQ-B", "SEQ-A", "press-2-op"} {
		if !strings.Contains(logged, want) {
			t.Errorf("audit line %q does not mention %q. An unlogged override of a physical fact is "+
				"indistinguishable afterwards from the system having believed it all along.",
				logged, want)
		}
	}
}
