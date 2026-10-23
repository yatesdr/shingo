package binresolver

import (
	"testing"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// tops_and_bottoms_line_test.go — a maintained group's room, per line. A cart at
// 0 with no part (tops & bottoms) is judged on its type's tops & bottoms row,
// every other cart on the stack level, and neither line takes the other's room.

// lineCounts answers AtDeclaredLevel's count by type and height: stacks at
// no height said, tops & bottoms at TopsAndBottomsOnly. rowReads counts the row
// reads.
type lineCounts struct {
	stacks, topsAndBottoms map[string]int
	rows                   map[int64]int
	rowReads               int
}

func (c *lineCounts) count(code string, h domain.Height) (int, error) {
	if h == domain.TopsAndBottomsOnly {
		return c.topsAndBottoms[code], nil
	}
	return c.stacks[code], nil
}

func (c *lineCounts) row(binTypeID int64) (int, bool, error) {
	c.rowReads++
	want, ok := c.rows[binTypeID]
	return want, ok, nil
}

// TestAtDeclaredLevel_TwoLinesPerType: a group keeping two of type 7 (two
// stacks), with or without a tops & bottoms row of two.
func TestAtDeclaredLevel_TwoLinesPerType(t *testing.T) {
	t.Parallel()
	levels := []nodes.MaintainLevel{{BinTypeID: 7, BinTypeCode: "TOTE", Want: 2}}
	tb := KnownTypeInState(7, protocol.CarrierTopsAndBottoms)
	for _, c := range []struct {
		name        string
		stated      BinTypeStatement
		stacks, tbs int
		row         bool
		full        bool
		rowReads    int
	}{
		{"a stack, the level met", KnownTypeInState(7, protocol.CarrierEmpty), 2, 0, true, true, 0},
		{"a stack, short, however much tops & bottoms stands", KnownTypeInState(7, protocol.CarrierEmpty), 1, 5, true, false, 0},
		{"a type named with no cart: the stack line", KnownBinType(7), 2, 0, true, true, 0},
		{"an unverified cart: the stack line", KnownTypeInState(7, protocol.CarrierUnverified), 1, 2, true, false, 0},
		{"tops & bottoms, the stack level met, its row short", tb, 2, 1, true, false, 1},
		{"tops & bottoms, its row met", tb, 0, 2, true, true, 1},
		{"tops & bottoms, no row on a type the group keeps", tb, 0, 0, false, true, 1},
		{"tops & bottoms of a type the group declares no level of", KnownTypeInState(9, protocol.CarrierTopsAndBottoms), 0, 0, false, false, 0},
		{"untyped: the group total, tops & bottoms left out", UnknownBinType("test"), 1, 5, true, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			lc := &lineCounts{stacks: map[string]int{"TOTE": c.stacks}, topsAndBottoms: map[string]int{"TOTE": c.tbs}}
			if c.row {
				lc.rows = map[int64]int{7: 2}
			}
			full, err := AtDeclaredLevel(levels, c.stated, lc.count, lc.row)
			if err != nil {
				t.Fatal(err)
			}
			if full != c.full {
				t.Errorf("at level = %v, want %v", full, c.full)
			}
			if lc.rowReads != c.rowReads {
				t.Errorf("row reads = %d, want %d: the row is read for a cart at 0 alone", lc.rowReads, c.rowReads)
			}
		})
	}
}

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
