package plan

import (
	"testing"
	"time"

	"shingocore/dispatch/binsource"
	"shingocore/domain"
	"shingocore/store/bins"
)

// bare_cart_belts_test.go — the dedicated-loader pool's half of the bare-cart
// belt (the allocator's half is dispatch/bare_cart_belts_test.go). A bare cart
// has no payload, which is exactly what the pool reads as "empty".

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
