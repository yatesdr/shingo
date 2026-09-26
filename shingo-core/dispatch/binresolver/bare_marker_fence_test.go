package binresolver

import (
	"testing"

	"shingocore/store/bins"
)

// TestCapacity_MarkerFitsWhereCarrierFits is the resolver's half of the same
// fence (the gate's half is dispatch.TestCapacity_MarkerFitsWhereCarrierFits):
// the two sites read the same rule, so a group the gate admits a bare cart to is
// one the resolver places it in.
func TestCapacity_MarkerFitsWhereCarrierFits(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	f.effBinTypes[42] = []*bins.BinType{{ID: 7, Code: "CART"}}
	f.bareOf = map[int64]int64{70: 7, 80: 8}
	r := &GroupResolver{DB: f}
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
		if got := r.binTypeAllowed(42, ptrTo(tc.bt)); got != tc.want {
			t.Errorf("%s (type %d) at a slot fenced to the carrier: allowed = %v, want %v", tc.name, tc.bt, got, tc.want)
		}
	}
}
