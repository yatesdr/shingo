package reservations_test

import (
	"reflect"
	"testing"

	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestDigLockTags_MatchTheReservedByTags holds orders.DigLockTags, which
// store/orders spells as literals because it may not import this package, equal
// to the tags the dig lock actually writes. A renamed tag would otherwise move
// every dig lock into holds-at-rest without a word.
func TestDigLockTags_MatchTheReservedByTags(t *testing.T) {
	want := []string{reservations.ByExcavation, reservations.ByDigHandoff}
	if !reflect.DeepEqual(orders.DigLockTags, want) {
		t.Fatalf("orders.DigLockTags = %v, want %v", orders.DigLockTags, want)
	}
}
