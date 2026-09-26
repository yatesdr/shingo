package dispatch

import (
	"testing"

	"shingocore/store/bins"
)

// bare_cart_belts_test.go — one of the two Go-side empty pickers that never composed
// bins.EmptyCarrierWhere, and so never excluded a bare cart. A bare cart has no
// payload, which is exactly what both of them read as "empty". The other, the
// dedicated-loader pool, moved into the planner with the picker
// (plan/bare_cart_belts_test.go).

// TestTier4_NeverOffersABareCart: the complex allocator's empty filter
// (emptyBinsOnly) hands a produce node's empty pickup leg a payload-less
// carrier. A bare cart is payload-less and must not be one.
func TestTier4_NeverOffersABareCart(t *testing.T) {
	t.Parallel()
	bare := &bins.Bin{ID: 1, Label: "BARE", BinTypeBare: true}
	plain := &bins.Bin{ID: 2, Label: "EMPTY"}
	got := emptyBinsOnly([]*bins.Bin{bare, plain})
	if len(got) != 1 || got[0].ID != plain.ID {
		labels := make([]string, 0, len(got))
		for _, b := range got {
			labels = append(labels, b.Label)
		}
		t.Fatalf("emptyBinsOnly = %v, want only the plain empty: a bare cart has no bin on it to fill", labels)
	}
}
