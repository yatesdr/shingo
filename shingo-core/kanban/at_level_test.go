package kanban

import (
	"testing"

	"shingo/protocol"
	"shingocore/domain"
)

// at_level_test.go — a kanban's room, per line. A cart at 0 with no part (tops &
// bottoms) is judged on its type's tops & bottoms row, every other cart on the stack
// level, and neither line takes the other's room. Through the resolver:
// binresolver's TestResolveStore_ACartAtZeroGoesToItsOwnLine and TestLevelFilter_*.

// lineCounts answers AtLevel's count by type and height: stacks at no height said,
// tops & bottoms at TopsAndBottomsOnly. rowReads counts the row reads.
type lineCounts struct {
	stacks, topsAndBottoms map[string]int
	rows                   map[int64]int
	rowReads               int
}

func (c *lineCounts) count(code string, h domain.Height) (int, error) {
	if h == protocol.TopsAndBottomsOnly {
		return c.topsAndBottoms[code], nil
	}
	return c.stacks[code], nil
}

func (c *lineCounts) row(binTypeID int64) (int, bool, error) {
	c.rowReads++
	want, ok := c.rows[binTypeID]
	return want, ok, nil
}

// TestAtLevel_TwoLinesPerType: a group keeping two of type 7 (two stacks), with or
// without a tops & bottoms row of two.
func TestAtLevel_TwoLinesPerType(t *testing.T) {
	t.Parallel()
	levels := []TypeLevel{{BinTypeID: 7, BinTypeCode: "TOTE", Want: 2}}
	seven, nine := int64(7), int64(9)
	tb := protocol.CarrierTopsAndBottoms
	for _, c := range []struct {
		name        string
		typeID      *int64
		state       protocol.CarrierState
		stacks, tbs int
		row         bool
		full        bool
		rowReads    int
	}{
		{"a stack, the level met", &seven, protocol.CarrierEmpty, 2, 0, true, true, 0},
		{"a stack, short, however much tops & bottoms stands", &seven, protocol.CarrierEmpty, 1, 5, true, false, 0},
		{"a type named with no cart: the stack line", &seven, "", 2, 0, true, true, 0},
		{"an unverified cart: the stack line", &seven, protocol.CarrierUnverified, 1, 2, true, false, 0},
		{"tops & bottoms, the stack level met, its row short", &seven, tb, 2, 1, true, false, 1},
		{"tops & bottoms, its row met", &seven, tb, 0, 2, true, true, 1},
		{"tops & bottoms, no row on a type the group keeps", &seven, tb, 0, 0, false, true, 1},
		{"tops & bottoms of a type the group declares no level of", &nine, tb, 0, 0, false, false, 0},
		{"untyped: the group total, tops & bottoms left out", nil, "", 1, 5, true, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			lc := &lineCounts{stacks: map[string]int{"TOTE": c.stacks}, topsAndBottoms: map[string]int{"TOTE": c.tbs}}
			if c.row {
				lc.rows = map[int64]int{7: 2}
			}
			full, err := AtLevel(levels, c.typeID, c.state, lc.count, lc.row)
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
