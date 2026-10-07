package plan

import (
	"errors"
	"testing"

	"shingocore/domain"
)

// TestDigStillFits is the verdict re-asked in the dig's hand, case by case: the
// facts unchanged, fewer owed, more owed, and a mouth granted since.
func TestDigStillFits(t *testing.T) {
	t.Parallel()
	planned := domain.DigFitFacts{Owed: 2}
	if err := DigStillFits(planned, domain.DigFitFacts{Owed: 2}, "L1", 3); err != nil {
		t.Errorf("unchanged facts refused: %v", err)
	}
	if err := DigStillFits(planned, domain.DigFitFacts{Owed: 1}, "L1", 3); err != nil {
		t.Errorf("fewer owed refused: %v", err)
	}
	var short *DigShortError
	if err := DigStillFits(planned, domain.DigFitFacts{Owed: 3}, "L1", 3); !errors.As(err, &short) ||
		!errors.Is(err, ErrNoShuffleSlot) {
		t.Errorf("more owed answered %v, want a DigShortError that is a no-shuffle-slot", err)
	}
	var granted *DugMouthGrantedError
	if err := DigStillFits(planned, domain.DigFitFacts{Owed: 2, GrantedTo: 9, GrantedSlot: "S0"}, "L1", 3); !errors.As(err, &granted) ||
		!errors.Is(err, ErrLaneMouthHeld) {
		t.Errorf("a mouth granted since answered %v, want a DugMouthGrantedError", err)
	}
}
