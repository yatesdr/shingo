package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// TestKanbanPin_ACartLeavesForTheOneOutboundWhateverItsType: at main 26a82fdf and
// after the Kanban build, the Edge never routes. A cart off an unloader window goes
// to the loader's one outbound_dest, whichever type the operator named and through
// whichever door; choosing another group is Core's. Core resolves the destination
// when the move reaches it.
func TestKanbanPin_ACartLeavesForTheOneOutboundWhateverItsType(t *testing.T) {
	t.Parallel()
	layers := 3
	for _, tc := range []struct {
		name string
		tap  func(eng *Engine, nodeID int64) error
	}{
		{"CLEAR naming no type", func(eng *Engine, id int64) error { return eng.ClearBin(id, "", false, nil) }},
		{"PUSH AS one type", func(eng *Engine, id int64) error { return eng.ClearBin(id, "KP-TYPE-A", false, nil) }},
		{"PUSH AS another type", func(eng *Engine, id int64) error { return eng.ClearBin(id, "KP-TYPE-B", false, nil) }},
		{"PUSH AS a height", func(eng *Engine, id int64) error { return eng.ClearBin(id, "KP-TYPE-A", false, &layers) }},
		{"PUSH EMPTY", func(eng *Engine, id int64) error { return eng.PushEmptyOut(id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const window = "KP-W1"
			eng, nodeID := emptyOutWindow(t, window)
			holdActableBins(t, eng, protocol.NodeBinsRow{Node: window, Occupied: true, BinID: emptyOutReadBinID})
			testutil.MustNoErr(t, tc.tap(eng, nodeID), tc.name)
			moves := emptyOutMoves(t, eng, window)
			if len(moves) != 1 {
				t.Fatalf("moves off the window = %d, want 1", len(moves))
			}
			if moves[0].DeliveryNode != "EMPTY-TOTES" {
				t.Errorf("the cart goes to %q, want the loader's one outbound EMPTY-TOTES", moves[0].DeliveryNode)
			}
		})
	}
}
