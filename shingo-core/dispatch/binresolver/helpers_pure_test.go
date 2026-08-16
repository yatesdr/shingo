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

// Resolve-around ranker semantics (owner rulings: depth always respected;
// compatibility is an equal-depth tiebreak only).

func TestBestStorageCandidate_ResolveAround_EqualDepthPrefersCompatible(t *testing.T) {
	t.Parallel()
	blocked := &nodes.Node{Name: "LANE-BLOCKED"}
	free := &nodes.Node{Name: "LANE-FREE"}
	// Same depth, same fill — only mouth compatibility differs. The compatible
	// lane must win so the order does not stall at a mode-held mouth.
	candidates := []storageCandidate{
		{node: blocked, hasMatch: true, depth: 3, count: 1, laneCompatible: false},
		{node: free, hasMatch: true, depth: 3, count: 1, laneCompatible: true},
	}
	if got := bestStorageCandidate(candidates); got.Name != "LANE-FREE" {
		t.Errorf("got %q, want LANE-FREE (compatible mouth wins the equal-depth tie)", got.Name)
	}
}

func TestBestStorageCandidate_ResolveAround_DepthBeatsCompatible(t *testing.T) {
	t.Parallel()
	deepBlocked := &nodes.Node{Name: "LANE-DEEP-BLOCKED"}
	shallowFree := &nodes.Node{Name: "LANE-SHALLOW-FREE"}
	// The deeper lane is mouth-blocked, the shallower lane is compatible. Depth is
	// ALWAYS respected: the deeper lane must still win — resolve-around never
	// demotes a deeper slot (the order waits at the mouth instead).
	candidates := []storageCandidate{
		{node: shallowFree, hasMatch: true, depth: 1, count: 0, laneCompatible: true},
		{node: deepBlocked, hasMatch: true, depth: 3, count: 5, laneCompatible: false},
	}
	if got := bestStorageCandidate(candidates); got.Name != "LANE-DEEP-BLOCKED" {
		t.Errorf("got %q, want LANE-DEEP-BLOCKED (depth outranks compatibility)", got.Name)
	}
}

func TestBestStorageCandidate_ResolveAround_CompatBeatsEmptiest(t *testing.T) {
	t.Parallel()
	fullCompat := &nodes.Node{Name: "LANE-COMPAT"}
	emptyBlocked := &nodes.Node{Name: "LANE-BLOCKED"}
	// Equal depth: compatibility sits ABOVE the emptiest-lane tiebreak, so the
	// compatible-but-fuller lane beats the emptier-but-blocked one.
	candidates := []storageCandidate{
		{node: emptyBlocked, hasMatch: true, depth: 2, count: 0, laneCompatible: false},
		{node: fullCompat, hasMatch: true, depth: 2, count: 4, laneCompatible: true},
	}
	if got := bestStorageCandidate(candidates); got.Name != "LANE-COMPAT" {
		t.Errorf("got %q, want LANE-COMPAT (compatibility outranks emptiest)", got.Name)
	}
}

func TestBestStorageCandidate_ResolveAround_OffIsByteIdentical(t *testing.T) {
	t.Parallel()
	// With the arm off, every candidate's laneCompatible is false, so ranking must
	// fall through to the historical depth→count order exactly as before.
	deep := &nodes.Node{Name: "LANE-DEEP"}
	shallowEmpty := &nodes.Node{Name: "LANE-SHALLOW-EMPTY"}
	candidates := []storageCandidate{
		{node: shallowEmpty, hasMatch: true, depth: 1, count: 0, laneCompatible: false},
		{node: deep, hasMatch: true, depth: 3, count: 9, laneCompatible: false},
	}
	if got := bestStorageCandidate(candidates); got.Name != "LANE-DEEP" {
		t.Errorf("got %q, want LANE-DEEP (arm off → depth packing unchanged)", got.Name)
	}
}
