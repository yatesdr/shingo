package simwarlink

// This file carries no build tag, like line.go: the MES struct's slots are part
// of the pure line model a docker-tagged scenario drives and the sim-tagged
// FakeClient publishes.

import (
	"fmt"
	"strings"
)

// MESSlots is how many (CATID_0N, Prod_Counter_0N) pairs a cell's MES struct
// carries: Prod_Counter_01 … Prod_Counter_05.
const MESSlots = 5

// Slot is one pair of a cell's MES struct as the PLC writes it: slot N's part
// identity (CATID_0N) and production counter (Prod_Counter_0N). A slot the
// program has never counted on reads 0 and 0, a slot that names no part.
type Slot struct {
	CATID   int64 `json:"catid"`
	Counter int64 `json:"counter"`
}

// CATIDMember names slot n's part identity in the struct (1-based): CATID_0n.
func CATIDMember(n int) string { return fmt.Sprintf("CATID_%02d", n) }

// CounterMember names slot n's production counter in the struct (1-based):
// Prod_Counter_0n.
func CounterMember(n int) string { return fmt.Sprintf("Prod_Counter_%02d", n) }

// MESSlotOf splits a counter tag naming slot n of an MES struct,
// "<struct>.Prod_Counter_0n" with n in 1..MESSlots, into the struct's name and
// n; ok is false for any other tag.
func MESSlotOf(tag string) (structName string, n int, ok bool) {
	i := strings.LastIndex(tag, ".")
	if i <= 0 {
		return "", 0, false
	}
	for n := 1; n <= MESSlots; n++ {
		if tag[i+1:] == CounterMember(n) {
			return tag[:i], n, true
		}
	}
	return "", 0, false
}

// CountOn moves the line's counting to slot n of its MES struct (1-based), as
// a program edit that counts the part on another slot does: from the next
// count, slot n's counter ticks with the line's strokes from where it stands
// and its CATID reads the part the line runs; the slot counted until now
// stops where it stands, its CATID left as is. A program change from then on
// changes and resets slot n. Moving to the slot already counted changes
// nothing; a slot outside 1..MESSlots is refused.
func (l *Line) CountOn(n int) error {
	if n < 1 || n > MESSlots {
		return fmt.Errorf("simwarlink: slot %d is not one of the MES struct's 1..%d", n, MESSlots)
	}
	if n-1 == l.slot {
		return nil
	}
	l.held[l.slot] = Slot{CATID: l.identity, Counter: l.counter}
	l.slot = n - 1
	l.counter, l.held[l.slot] = l.held[l.slot].Counter, Slot{}
	return nil
}

// CountingSlot is the slot the line counts on, 1-based.
func (l *Line) CountingSlot() int { return l.slot + 1 }

// Slots returns the MES struct's pairs as the PLC publishes them: the slot the
// line counts on reads the line's identity and counter, every other slot what
// it read when the count left it.
func (l *Line) Slots() [MESSlots]Slot {
	out := l.held
	out[l.slot] = Slot{CATID: l.identity, Counter: l.counter}
	return out
}
