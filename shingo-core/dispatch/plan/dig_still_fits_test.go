package plan

import (
	"errors"
	"testing"

	"shingocore/domain"
)

// TestDigStillFits is the verdict re-asked in the dig's hand, case by case: the
// facts unchanged, and a mouth granted since. Running digs' parking is no longer
// re-asked here: it is held on paper (C2b dig-parking ruling), and a slot taken
// since the walk refuses the birth's pencil instead.
func TestDigStillFits(t *testing.T) {
	t.Parallel()
	planned := domain.DigFitFacts{GroupName: "G"}
	if err := DigStillFits(planned, domain.DigFitFacts{GroupName: "G"}, "L1", 3, domain.OwnDrop{}); err != nil {
		t.Errorf("unchanged facts refused: %v", err)
	}
	var granted *DugMouthGrantedError
	if err := DigStillFits(planned, domain.DigFitFacts{GroupName: "G", GrantedTo: 9, GrantedSlot: "S0"}, "L1", 3, domain.OwnDrop{}); !errors.As(err, &granted) ||
		!errors.Is(err, ErrLaneMouthHeld) {
		t.Errorf("a mouth granted since answered %v, want a DugMouthGrantedError", err)
	}
}

// TestOwnDrop_IsOwedInItsOwnGroupLessWhatItHolds: a demand's drop into the dug
// group is owed a slot, a drop elsewhere is not, and a slot the demand already
// holds in the group pays for one. Re-asked in the hand, a slot given up since
// the plan is owed again, and the dig is refused.
func TestOwnDrop_IsOwedInItsOwnGroupLessWhatItHolds(t *testing.T) {
	t.Parallel()
	own := domain.OwnDrop{DemandID: 7, Groups: []string{"MARKET", "OTHER"}}
	if got := own.Owed(domain.DigFitFacts{GroupName: "MARKET"}); got != 1 {
		t.Errorf("one drop into the dug group owed %d, want 1", got)
	}
	if got := own.Owed(domain.DigFitFacts{GroupName: "MARKET", DemandSlotsHeld: 1}); got != 0 {
		t.Errorf("a drop the demand already holds a slot for owed %d, want 0", got)
	}
	if got := own.Owed(domain.DigFitFacts{GroupName: "ELSEWHERE"}); got != 0 {
		t.Errorf("drops into other groups owed %d, want 0", got)
	}
	planned := domain.DigFitFacts{GroupName: "MARKET", DemandSlotsHeld: 1}
	var short *DigShortError
	if err := DigStillFits(planned, domain.DigFitFacts{GroupName: "MARKET"}, "L1", 2, own); !errors.As(err, &short) {
		t.Errorf("a slot given up since the plan answered %v, want a DigShortError", err)
	}
}
