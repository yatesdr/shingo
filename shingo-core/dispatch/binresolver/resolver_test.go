package binresolver

import (
	"testing"
	"time"

	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// --- Non-NGRP retrieve -----------------------------------------------------

func TestDefaultResolver_Retrieve_PicksFirstChildWithAvailableBin(t *testing.T) {
	f := newFakeStore()
	parent := directChild(1, "parent")
	childA := directChild(10, "child-A")
	childB := directChild(11, "child-B")
	f.children[parent.ID] = []*nodes.Node{childA, childB}
	// child-A has only an unavailable bin; child-B has an available one.
	f.bins[childA.ID] = []*bins.Bin{unavailBin(100, "P1")}
	f.bins[childB.ID] = []*bins.Bin{availBin(101, "P1", time.Now())}

	r := &DefaultResolver{DB: f}
	got, err := r.Resolve(parent, "retrieve", "P1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Node != childB {
		t.Fatalf("expected child-B, got %s", got.Node.Name)
	}
}

// --- IsAvailableAtConcreteNode (payload-match trap fix) -------------------

func TestIsAvailableAtConcreteNode_ClearedBinPasses(t *testing.T) {
	// Post-completion state: payload cleared, manifest_confirmed=false, status=staged.
	// This is exactly what ClearAndClaim leaves behind.
	cleared := &bins.Bin{
		ID:                1,
		Status:            "staged",
		ManifestConfirmed: false,
		PayloadCode:       "",
	}
	if !IsAvailableAtConcreteNode(cleared, "PAYLOAD-X") {
		t.Error("cleared bin at concrete node should be available for lineside pickup")
	}
}

func TestIsAvailableAtConcreteNode_MatchingPayloadPasses(t *testing.T) {
	bin := &bins.Bin{
		ID:                2,
		Status:            "staged",
		ManifestConfirmed: true,
		PayloadCode:       "PAYLOAD-X",
	}
	if !IsAvailableAtConcreteNode(bin, "PAYLOAD-X") {
		t.Error("bin with matching payload should be available")
	}
}

func TestIsAvailableAtConcreteNode_MismatchedPayloadRejected(t *testing.T) {
	// Wrong part parked at wrong station — should be rejected.
	bin := &bins.Bin{
		ID:                3,
		Status:            "staged",
		ManifestConfirmed: true,
		PayloadCode:       "PAYLOAD-Y",
	}
	if IsAvailableAtConcreteNode(bin, "PAYLOAD-X") {
		t.Error("bin with wrong payload at concrete node should be rejected")
	}
}

func TestIsAvailableAtConcreteNode_ClaimedBinRejected(t *testing.T) {
	orderID := int64(99)
	bin := &bins.Bin{
		ID:                4,
		Status:            "staged",
		ManifestConfirmed: false,
		PayloadCode:       "",
		ClaimedBy:         &orderID,
	}
	if IsAvailableAtConcreteNode(bin, "PAYLOAD-X") {
		t.Error("claimed bin should be rejected")
	}
}

func TestIsAvailableAtConcreteNode_BadStatusRejected(t *testing.T) {
	for _, status := range []string{"maintenance", "flagged", "retired", "quality_hold"} {
		bin := &bins.Bin{
			ID:                5,
			Status:            status,
			ManifestConfirmed: false,
			PayloadCode:       "",
		}
		if IsAvailableAtConcreteNode(bin, "PAYLOAD-X") {
			t.Errorf("bin with status %q should be rejected", status)
		}
	}
}

func TestIsAvailableAtConcreteNode_EmptyPayloadCodeAccepted(t *testing.T) {
	// When the order itself has no payload filter, any bin should pass
	// except claimed/bad-status.
	bin := &bins.Bin{
		ID:                6,
		Status:            "staged",
		ManifestConfirmed: false,
		PayloadCode:       "",
	}
	if !IsAvailableAtConcreteNode(bin, "") {
		t.Error("bin with empty payload filter should pass")
	}
}

func TestDefaultResolver_Retrieve_NoAvailableBins(t *testing.T) {
	f := newFakeStore()
	parent := directChild(1, "parent")
	child := directChild(10, "only-child")
	f.children[parent.ID] = []*nodes.Node{child}
	f.bins[child.ID] = []*bins.Bin{claimedBin(100, "P1", 7)}

	r := &DefaultResolver{DB: f}
	if _, err := r.Resolve(parent, "retrieve", "P1", nil); err == nil {
		t.Fatal("expected error when no child has an available bin")
	}
}

func TestDefaultResolver_Retrieve_PayloadFilter(t *testing.T) {
	f := newFakeStore()
	parent := directChild(1, "parent")
	child := directChild(10, "c")
	f.children[parent.ID] = []*nodes.Node{child}
	// Bin exists but its payload code does not match the request.
	f.bins[child.ID] = []*bins.Bin{availBin(100, "OTHER", time.Now())}

	r := &DefaultResolver{DB: f}
	if _, err := r.Resolve(parent, "retrieve", "P1", nil); err == nil {
		t.Fatal("expected error when no bin matches requested payload")
	}
}

// --- Non-NGRP store --------------------------------------------------------

func TestDefaultResolver_Store_PicksConsolidationCandidate(t *testing.T) {
	f := newFakeStore()
	parent := directChild(1, "parent")
	a := directChild(10, "empty-A")
	b := directChild(11, "consolidate-B")
	f.children[parent.ID] = []*nodes.Node{a, b}
	// Both empty (count 0), but B already holds a bin with matching
	// payload — resolveStore prefers the consolidation candidate.
	f.bins[b.ID] = []*bins.Bin{availBin(100, "P1", time.Now())}
	// Override counts: the real resolveStore skips nodes with count>=1.
	// We want both reported as empty for ranking purposes.
	f.binCounts[a.ID] = 0
	f.binCounts[b.ID] = 0

	r := &DefaultResolver{DB: f}
	got, err := r.Resolve(parent, "store", "P1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Node != b {
		t.Fatalf("expected consolidate-B, got %s", got.Node.Name)
	}
}

func TestDefaultResolver_Store_SkipsOccupiedAndSynthetic(t *testing.T) {
	f := newFakeStore()
	parent := directChild(1, "parent")
	syn := &nodes.Node{ID: 10, Name: "syn", IsSynthetic: true, Enabled: true}
	full := directChild(11, "full")
	empty := directChild(12, "empty")
	f.children[parent.ID] = []*nodes.Node{syn, full, empty}
	f.binCounts[full.ID] = 1
	f.binCounts[empty.ID] = 0

	r := &DefaultResolver{DB: f}
	got, err := r.Resolve(parent, "store", "P1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Node != empty {
		t.Fatalf("expected empty, got %s", got.Node.Name)
	}
}

func TestDefaultResolver_Store_NoCandidate(t *testing.T) {
	f := newFakeStore()
	parent := directChild(1, "parent")
	full := directChild(10, "full")
	f.children[parent.ID] = []*nodes.Node{full}
	f.binCounts[full.ID] = 1

	r := &DefaultResolver{DB: f}
	if _, err := r.Resolve(parent, "store", "P1", nil); err == nil {
		t.Fatal("expected error when no child has room")
	}
}

// --- Unknown order type / empty synthetic ---------------------------------

func TestDefaultResolver_UnknownOrderType_FirstEnabled(t *testing.T) {
	f := newFakeStore()
	parent := directChild(1, "parent")
	disabled := disabledChild(9, "off")
	on := directChild(10, "on")
	f.children[parent.ID] = []*nodes.Node{disabled, on}

	r := &DefaultResolver{DB: f}
	got, err := r.Resolve(parent, "weird", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Node != on {
		t.Fatal("unknown order type should return the first enabled child")
	}
}

func TestDefaultResolver_NoChildren(t *testing.T) {
	f := newFakeStore()
	parent := directChild(1, "lonely")

	r := &DefaultResolver{DB: f}
	if _, err := r.Resolve(parent, "retrieve", "", nil); err == nil {
		t.Fatal("expected error for parent with no children")
	}
}

// --- NGRP delegation -------------------------------------------------------

func TestDefaultResolver_Retrieve_NGRPDelegatesToGroupResolver(t *testing.T) {
	f := newFakeStore()
	ngrp := ngrpNode(1, "group")
	lane := laneChild(10, "lane-1")
	slot := slotInLane(100, "slot-1")
	f.nodes[slot.ID] = slot
	bin := availBin(1000, "P1", time.Now())
	attachSlot(bin, slot)
	f.children[ngrp.ID] = []*nodes.Node{lane}
	f.sourceInLane[lane.ID] = bin

	r := &DefaultResolver{DB: f, LaneLock: NewLaneLock()}
	got, err := r.Resolve(ngrp, "retrieve", "P1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Bin != bin || got.Node != slot {
		t.Fatalf("expected bin from lane-1 / slot-1, got %+v", got)
	}
}
