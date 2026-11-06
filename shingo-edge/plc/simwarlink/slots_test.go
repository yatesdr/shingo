package simwarlink

import "testing"

// TestLineCountOn_TheSiblingTicksAndTheCountedSlotStops: after the move the
// sibling's counter ticks with the line's strokes from where it stood and its
// CATID reads the part; the slot counted until then keeps its count and its
// CATID; a program change lands on the slot counted now; a move back resumes
// the first slot where it stopped.
func TestLineCountOn_TheSiblingTicksAndTheCountedSlotStops(t *testing.T) {
	l := NewLine(2, 1)
	l.SetIdentity(1001)
	for range 5 {
		l.Tick()
	}
	if got := l.Slots(); got[0] != (Slot{1001, 3}) || got[1] != (Slot{}) || l.CountingSlot() != 1 {
		t.Fatalf("before the move: slots %v on slot %d; want slot 1 {1001 3}, slot 2 unused", got, l.CountingSlot())
	}
	if err := l.CountOn(2); err != nil {
		t.Fatalf("CountOn(2): %v", err)
	}
	for range 2 {
		l.Tick()
	}
	st := l.State()
	if st.Slots[0] != (Slot{1001, 3}) || st.Slots[1] != (Slot{1001, 2}) || st.Slot != 2 || st.Counter != 2 || st.Loads != 7 {
		t.Fatalf("after the move and two strokes: %+v; want slot 1 stopped at {1001 3}, slot 2 {1001 2} counted, 7 loads", st)
	}
	l.Force(1002)
	if got := l.Slots(); got[0] != (Slot{1001, 3}) || got[1] != (Slot{1002, 0}) {
		t.Fatalf("a forced change on slot 2: %v; want slot 2 {1002 0}, slot 1 left {1001 3}", got)
	}
	if err := l.CountOn(1); err != nil {
		t.Fatalf("CountOn(1): %v", err)
	}
	l.Tick()
	if got := l.Slots(); got[0] != (Slot{1002, 4}) || got[1] != (Slot{1002, 0}) {
		t.Fatalf("back on slot 1 after a stroke: %v; want slot 1 {1002 4} (the part, from 3), slot 2 left {1002 0}", got)
	}
	for _, n := range []int{0, MESSlots + 1} {
		if err := l.CountOn(n); err == nil {
			t.Errorf("CountOn(%d) = nil; want refused", n)
		}
	}
}

// TestMESSlotOf_OnlyACounterSlotOfAStruct: the struct and the slot of a
// Prod_Counter_0N member; nothing for a bare leaf, another member, a slot past
// the struct's five, or a plain counter.
func TestMESSlotOf_OnlyACounterSlotOfAStruct(t *testing.T) {
	if s, n, ok := MESSlotOf("MES_WELD_6.Prod_Counter_02"); !ok || s != "MES_WELD_6" || n != 2 {
		t.Errorf("MESSlotOf(MES_WELD_6.Prod_Counter_02) = %q %d %t; want MES_WELD_6 2 true", s, n, ok)
	}
	for _, tag := range []string{"Prod_Counter_01", "MES_WELD_6.CATID_01", "MES_WELD_6.Prod_Counter_06", "WELD-6_COUNTER"} {
		if _, _, ok := MESSlotOf(tag); ok {
			t.Errorf("MESSlotOf(%q) = ok; want not a slot", tag)
		}
	}
}
