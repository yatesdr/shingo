package dispatch

import (
	"testing"
	"time"

	"shingocore/dispatch/binsource"
	"shingocore/domain"
	"shingocore/store/bins"
)

// bare_cart_belts_test.go — the two Go-side empty pickers that never composed
// bins.EmptyCarrierWhere, and so never excluded a bare cart. A bare cart has no
// payload, which is exactly what both of them read as "empty".

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

// TestDedicatedPool_NeverDrawsABareCart: the dedicated-loader pool (tier 2)
// fills a home from its buffers; a Fill want takes any fungible empty, and a
// bare cart standing on a buffer is payload-less.
func TestDedicatedPool_NeverDrawsABareCart(t *testing.T) {
	t.Parallel()
	bare := candFromBin(&bins.Bin{ID: 1, BinTypeID: 7, BinTypeBare: true, Status: domain.BinStatus("available"), CreatedAt: time.Now()})
	plain := candFromBin(&bins.Bin{ID: 2, BinTypeID: 8, Status: domain.BinStatus("available"), CreatedAt: time.Now()})
	want := binsource.Want{Payload: "PART-X", Intent: binsource.Fill}
	if r := binsource.RejectReason(bare, want); r == "" {
		t.Error("the dedicated pool accepts a bare cart as an empty to fill: a bare cart has no bin on it")
	}
	if r := binsource.RejectReason(plain, want); r != "" {
		t.Errorf("control: a plain empty is rejected (%s)", r)
	}
}
