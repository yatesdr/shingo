package binsource

import (
	"testing"

	"shingocore/domain"
)

// TestPin_LabelledCarrierAtZeroIsNeitherStockNorContainer characterises the
// Core half of the SHIM loop (RULINGS-C2b-rebase6-and-loader-orphan §3): a
// carrier still labelled X that reads 0 units is not stock of X (Drain) and not
// a container for X (Fill), so a dedicated loader's pool holding only it answers
// finder-pool-empty either way. The Edge half is shingo-edge engine
// TestPin_LoaderRefusesALabelledCarrierAtZero.
func TestPin_LabelledCarrierAtZeroIsNeitherStockNorContainer(t *testing.T) {
	c := Cand{BinID: 1, Payload: X, UOP: 0, Cap: cap10, CreatedAt: tOld, ManifestConfirmed: true,
		Status: domain.BinStatusAvailable}
	if got := RejectReason(c, Want{Payload: X, Intent: Drain}); got != "uop<=0" {
		t.Errorf("Drain of %s: reject = %q, want uop<=0", X, got)
	}
	if got := RejectReason(c, Want{Payload: X, Intent: Fill}); got != "full-not-a-container" {
		t.Errorf("Fill of %s: reject = %q, want full-not-a-container", X, got)
	}
	if _, ok := Source([]Cand{c}, Want{Payload: X, Intent: Drain}); ok {
		t.Error("the pool sourced a labelled carrier at 0 as stock")
	}
	if _, ok := Source([]Cand{c}, Want{Payload: X, Intent: Fill}); ok {
		t.Error("the pool sourced a labelled carrier at 0 as an empty")
	}
}
