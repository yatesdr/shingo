package dispatch

import (
	"testing"

	"shingocore/store/bins"
)

// TestCapacity_MarkerFitsWhereCarrierFits: a bare marker is admitted wherever
// its carrier is admitted. A cart is the same physical carrier with or without
// a bin on it, so a slot fenced to the carrier takes the bare cart too — the
// wait group whose slots are typed to the real cart, and the stage-2 window
// typed the same way. Without it, pull mode worked in an untyped sim and parked
// in a typed plant.
func TestCapacity_MarkerFitsWhereCarrierFits(t *testing.T) {
	t.Parallel()
	db := &fakeCapacityDB{
		effBinTypes: map[int64][]*bins.BinType{42: {{ID: 7, Code: "CART"}}},
		bareOf:      map[int64]int64{70: 7, 80: 8},
	}
	id := func(v int64) *int64 { return &v }
	for _, tc := range []struct {
		name string
		bt   int64
		want bool
	}{
		{"the carrier", 7, true},
		{"the carrier's marker", 70, true},
		{"another carrier's marker", 80, false},
		{"an unrelated type", 9, false},
	} {
		got, err := binTypeAllowedAt(db, 42, id(tc.bt))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s (type %d) at a slot fenced to the carrier: allowed = %v, want %v", tc.name, tc.bt, got, tc.want)
		}
	}
}
