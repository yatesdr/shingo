package binresolver

import (
	"testing"

	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

func TestBinUnavailableReason_Available(t *testing.T) {
	t.Parallel()
	b := &bins.Bin{Status: domain.BinStatusAvailable}
	if got := BinUnavailableReason(b, "PART-A"); got != "" {
		t.Errorf("available bin: got %q, want empty", got)
	}
}

func TestBinUnavailableReason_Claimed(t *testing.T) {
	t.Parallel()
	claimedBy := int64(42)
	b := &bins.Bin{Status: domain.BinStatusAvailable, ClaimedBy: &claimedBy}
	got := BinUnavailableReason(b, "PART-A")
	if got == "" {
		t.Error("claimed bin: got empty reason, want rejection")
	}
}

func TestBinUnavailableReason_BadStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []domain.BinStatus{
		domain.BinStatusMaintenance, domain.BinStatusFlagged,
		domain.BinStatusRetired, domain.BinStatusQualityHold,
	} {
		b := &bins.Bin{Status: status}
		got := BinUnavailableReason(b, "")
		if got == "" {
			t.Errorf("status=%q: got empty reason, want rejection", status)
		}
	}
}

func TestBinUnavailableReason_PayloadMismatch(t *testing.T) {
	t.Parallel()
	b := &bins.Bin{Status: domain.BinStatusAvailable, PayloadCode: "PART-B"}
	got := BinUnavailableReason(b, "PART-A")
	if got == "" {
		t.Error("payload mismatch: got empty reason, want rejection")
	}
}

func TestBinUnavailableReason_EmptyPayloadCode_Passes(t *testing.T) {
	t.Parallel()
	b := &bins.Bin{Status: domain.BinStatusAvailable, PayloadCode: "PART-B"}
	if got := BinUnavailableReason(b, ""); got != "" {
		t.Errorf("empty order payload code should pass: got %q", got)
	}
	binEmpty := &bins.Bin{Status: domain.BinStatusAvailable, PayloadCode: ""}
	if got := BinUnavailableReason(binEmpty, "PART-A"); got != "" {
		t.Errorf("empty bin payload code should pass: got %q", got)
	}
}

func TestBestStorageCandidate_Empty(t *testing.T) {
	t.Parallel()
	if got := bestStorageCandidate(nil); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestBestStorageCandidate_PrefersMatch(t *testing.T) {
	t.Parallel()
	n1 := &nodes.Node{Name: "SLOT-1"}
	n2 := &nodes.Node{Name: "SLOT-2"}
	candidates := []storageCandidate{
		{node: n1, hasMatch: false, count: 2},
		{node: n2, hasMatch: true, count: 5},
	}
	got := bestStorageCandidate(candidates)
	if got.Name != "SLOT-2" {
		t.Errorf("got %q, want SLOT-2 (has matching payload)", got.Name)
	}
}

func TestBestStorageCandidate_PrefersEmptiest(t *testing.T) {
	t.Parallel()
	n1 := &nodes.Node{Name: "SLOT-1"}
	n2 := &nodes.Node{Name: "SLOT-2"}
	n3 := &nodes.Node{Name: "SLOT-3"}
	candidates := []storageCandidate{
		{node: n1, hasMatch: true, count: 5},
		{node: n2, hasMatch: true, count: 3},
		{node: n3, hasMatch: true, count: 7},
	}
	got := bestStorageCandidate(candidates)
	if got.Name != "SLOT-2" {
		t.Errorf("got %q, want SLOT-2 (fewest bins)", got.Name)
	}
}

func TestBestStorageCandidate_Single(t *testing.T) {
	t.Parallel()
	n1 := &nodes.Node{Name: "SLOT-1"}
	candidates := []storageCandidate{{node: n1, hasMatch: false, count: 0}}
	got := bestStorageCandidate(candidates)
	if got.Name != "SLOT-1" {
		t.Errorf("got %v, want SLOT-1", got)
	}
}
