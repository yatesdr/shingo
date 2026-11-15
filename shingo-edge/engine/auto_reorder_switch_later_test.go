package engine

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// TestFlipAutoReorder_ALineSwitchedOffCoversPositionsAddedLater: the line's
// switch is the line's. A position added to a switched-off line, or one that
// had no runtime row when the line was switched off, is off from its first
// runtime row, with the flip's who and when; after the line is switched on, a
// new position starts on.
func TestFlipAutoReorder_ALineSwitchedOffCoversPositionsAddedLater(t *testing.T) {
	t.Parallel()
	l := newIdleLine(t, time.Minute)
	rowless, err := l.db.CreateProcessNode(processes.NodeInput{
		ProcessID: l.processID, CoreNodeName: "ALN_IDLE_R", Code: "IR", Name: "ALN_IDLE_R", Sequence: 2, Enabled: true,
	})
	testutil.MustNoErr(t, err, "a position with no runtime row yet")
	testutil.MustNoErr(t, l.eng.FlipAutoReorder(l.processID, false, "Pat", domain.FlipFromProcesses), "switch off")

	added, err := l.db.CreateProcessNode(processes.NodeInput{
		ProcessID: l.processID, CoreNodeName: "ALN_IDLE_N", Code: "IN", Name: "ALN_IDLE_N", Sequence: 3, Enabled: true,
	})
	testutil.MustNoErr(t, err, "a position added after the switch off")
	for name, id := range map[string]int64{"added after": added, "no row at the flip": rowless} {
		rt, err := l.db.EnsureProcessNodeRuntime(id)
		testutil.MustNoErr(t, err, "its first runtime row")
		if rt.ReorderHold != domain.ReorderHoldPersonOff || rt.ReorderHoldNote != "Pat" || rt.ReorderHoldAt.IsZero() {
			t.Errorf("position %s: %q by %q at %v, want person_off by Pat, stamped", name, rt.ReorderHold, rt.ReorderHoldNote, rt.ReorderHoldAt)
		}
	}

	testutil.MustNoErr(t, l.eng.FlipAutoReorder(l.processID, true, "Sam", domain.FlipFromProcesses), "switch on")
	later, err := l.db.CreateProcessNode(processes.NodeInput{
		ProcessID: l.processID, CoreNodeName: "ALN_IDLE_L", Code: "IL", Name: "ALN_IDLE_L", Sequence: 4, Enabled: true,
	})
	testutil.MustNoErr(t, err, "a position added after the switch on")
	rt, err := l.db.EnsureProcessNodeRuntime(later)
	testutil.MustNoErr(t, err, "its first runtime row")
	if rt.ReorderHold != domain.ReorderHoldNone {
		t.Errorf("a position added to a line switched on holds %q, want nothing", rt.ReorderHold)
	}
}
