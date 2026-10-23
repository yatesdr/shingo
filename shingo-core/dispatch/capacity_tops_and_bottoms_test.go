package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/dispatch/plan"
	"shingocore/store/nodes"
)

// TestCapacityGate_ACartAtZeroIsJudgedOnItsOwnLine: the capacity gate asks the
// resolver's own level question (binresolver.AtDeclaredLevel) with the cart the
// asking order places, its state read beside its type (BinTypeForOrder), so a
// group whose stack level is met admits the tops & bottoms its row is short of,
// and parks it at level when its type has no row; a stack parks at level.
func TestCapacityGate_ACartAtZeroIsJudgedOnItsOwnLine(t *testing.T) {
	t.Parallel()
	grpID := int64(300)
	for _, c := range []struct {
		name    string
		state   protocol.CarrierState
		row     bool
		atLevel bool
	}{
		{"tops & bottoms, its row short", protocol.CarrierTopsAndBottoms, true, false},
		{"tops & bottoms, no row", protocol.CarrierTopsAndBottoms, false, true},
		{"a stack", protocol.CarrierEmpty, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := &fakeCapacityDB{
				node:           &nodes.Node{ID: grpID, Name: "TB-GRP", IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP, Enabled: true},
				children:       []*nodes.Node{{ID: 301, Name: "TB-GRP-1", Enabled: true}, {ID: 302, Name: "TB-GRP-2", Enabled: true}},
				binsByChild:    map[int64]int{301: 0, 302: 0},
				maintainLevels: map[int64][]nodes.MaintainLevel{grpID: {{GroupNodeID: grpID, BinTypeID: 7, BinTypeCode: "TB-TOTE", Want: 2}}},
				emptyInGroup:   map[int64]int{grpID: 2},
				tbInGroup:      map[int64]int{grpID: 0},
				orderBinTypes:  map[int64]int64{55: 7},
				orderStates:    map[int64]protocol.CarrierState{55: c.state},
			}
			if c.row {
				db.tbRows = map[int64]int{7: 1}
			}
			blocked, v := plan.DropoffCapacity(db, "TB-GRP", 55)
			if got := blocked && v.Cause == plan.CauseNGRPAtLevel; got != c.atLevel {
				t.Errorf("blocked %v cause %q, want at level %v", blocked, v.Cause, c.atLevel)
			}
		})
	}
}
