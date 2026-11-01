package binresolver

import (
	"testing"

	"shingo/protocol"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// tops_and_bottoms_line_test.go — a maintained group's room, per line. A cart at
// 0 with no part (tops & bottoms) is judged on its type's tops & bottoms row,
// every other cart on the stack level, and neither line takes the other's room.

// TestResolveStore_ACartAtZeroGoesToItsOwnLine: through the resolver, a group
// whose stack level is met takes the tops & bottoms its row is short of (the
// cart stated by its order, BinTypeForOrder), and refuses it when the type has
// no row; a stack is refused either way.
func TestResolveStore_ACartAtZeroGoesToItsOwnLine(t *testing.T) {
	t.Parallel()
	bt := int64(7)
	for _, c := range []struct {
		name  string
		state protocol.CarrierState
		row   bool
		takes bool
	}{
		{"tops & bottoms, its row short", protocol.CarrierTopsAndBottoms, true, true},
		{"tops & bottoms, no row", protocol.CarrierTopsAndBottoms, false, false},
		{"a stack", protocol.CarrierEmpty, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f, grp := levelStore(200,
				[]nodes.MaintainLevel{{GroupNodeID: 200, BinTypeID: bt, BinTypeCode: "TB-TOTE", Want: 2}},
				map[groupKey]int{{200, "TB-TOTE"}: 2})
			f.orderBinTypes = map[int64]int64{55: bt}
			f.orderStates = map[int64]protocol.CarrierState{55: c.state}
			if c.row {
				f.tbRows = map[int64]int{bt: 1}
			}
			r := &GroupResolver{DB: f}
			res, err := r.ResolveStore(grp, "", UnknownBinType("test: derive from the order"),
				reservations.DigAsker{OrderID: 55})
			if took := err == nil && res != nil; took != c.takes {
				t.Errorf("took = %v (err %v), want %v", took, err, c.takes)
			}
			wantReads := 0
			if c.state == protocol.CarrierTopsAndBottoms {
				wantReads = 1
			}
			if f.tbRowReads != wantReads {
				t.Errorf("row reads = %d, want %d", f.tbRowReads, wantReads)
			}
		})
	}
}
