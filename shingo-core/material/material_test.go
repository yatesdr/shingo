package material

import (
	"encoding/json"
	"testing"

	"shingocore/store/bins"
	"shingocore/store/cms"
	"shingocore/store/nodes"
)

// ptrInt64 returns a pointer to i — convenience for building node
// trees with optional ParentID in-line.
func ptrInt64(i int64) *int64 { return &i }

// addNode inserts a Node into the fake, optionally marking it
// synthetic. parentID=0 means the node is a root.
func addNode(f *fakeStore, id int64, name string, synthetic bool, parentID int64) {
	n := &nodes.Node{
		ID:          id,
		Name:        name,
		IsSynthetic: synthetic,
	}
	if parentID != 0 {
		n.ParentID = ptrInt64(parentID)
	}
	f.nodes[id] = n
}

// ---------- FindCMSBoundary ----------------------------------------

func TestFindCMSBoundary_ParentlessSyntheticDefaultOn(t *testing.T) {
	f := newFakeStore()
	addNode(f, 1, "root", true, 0) // synthetic, no parent, no prop → enabled

	got, err := FindCMSBoundary(f, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.ID != 1 {
		t.Fatalf("expected boundary at node 1, got %+v", got)
	}
}

func TestFindCMSBoundary_ParentlessSyntheticExplicitlyOff(t *testing.T) {
	f := newFakeStore()
	addNode(f, 1, "root", true, 0)
	f.setProp(1, "log_cms_transactions", "false")

	got, err := FindCMSBoundary(f, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected no boundary (disabled root), got %+v", got)
	}
}

func TestFindCMSBoundary_ChildSyntheticDefaultOff(t *testing.T) {
	f := newFakeStore()
	addNode(f, 1, "root", true, 0)
	f.setProp(1, "log_cms_transactions", "false") // disable root so walk keeps going
	addNode(f, 2, "child", true, 1)               // synthetic child, no prop → disabled by default

	got, err := FindCMSBoundary(f, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected no boundary (child default off, root disabled), got %+v", got)
	}
}

func TestFindCMSBoundary_ChildSyntheticExplicitlyOn(t *testing.T) {
	f := newFakeStore()
	addNode(f, 1, "root", true, 0)
	addNode(f, 2, "child", true, 1)
	f.setProp(2, "log_cms_transactions", "true")

	got, err := FindCMSBoundary(f, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.ID != 2 {
		t.Fatalf("expected boundary at node 2, got %+v", got)
	}
}

func TestFindCMSBoundary_WalksToRootReturnsRoot(t *testing.T) {
	// Non-synthetic leaf, non-synthetic middle, synthetic root: walk
	// should climb past both non-syntheticals and stop at the root.
	f := newFakeStore()
	addNode(f, 1, "root", true, 0)
	addNode(f, 2, "mid", false, 1)
	addNode(f, 3, "leaf", false, 2)

	got, err := FindCMSBoundary(f, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.ID != 1 {
		t.Fatalf("expected boundary at root (1), got %+v", got)
	}
}

func TestFindCMSBoundary_NoSyntheticAncestor(t *testing.T) {
	// Walk bottoms out at a non-synthetic root → (nil, nil).
	f := newFakeStore()
	addNode(f, 1, "root", false, 0)
	addNode(f, 2, "leaf", false, 1)

	got, err := FindCMSBoundary(f, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected no boundary, got %+v", got)
	}
}

func TestFindCMSBoundary_CycleReturnsError(t *testing.T) {
	// Two non-synthetic nodes that point at each other. FindCMSBoundary
	// should detect the revisit and return (nil, err) so the engine
	// wrapper can log the anomaly without returning a bogus node.
	f := newFakeStore()
	f.nodes[1] = &nodes.Node{ID: 1, Name: "a", IsSynthetic: false, ParentID: ptrInt64(2)}
	f.nodes[2] = &nodes.Node{ID: 2, Name: "b", IsSynthetic: false, ParentID: ptrInt64(1)}

	got, err := FindCMSBoundary(f, 1)
	if err == nil {
		t.Fatalf("expected cycle error, got nil (result=%+v)", got)
	}
	if got != nil {
		t.Fatalf("expected nil node on cycle, got %+v", got)
	}
}

// ---------- BuildMovementTransactions -------------------------------

// setManifest sets a bin's Manifest JSON from the given entries.
func setManifest(b *bins.Bin, entries []bins.ManifestEntry) {
	body, _ := json.Marshal(bins.Manifest{Items: entries})
	s := string(body)
	b.Manifest = &s
}

func TestBuildMovement_SameBoundaryNoTxns(t *testing.T) {
	f := newFakeStore()
	addNode(f, 1, "boundary", true, 0) // single synthetic root

	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1"}
	setManifest(bin, []bins.ManifestEntry{{CatID: "C1", Quantity: 5}})
	f.bins[10] = bin

	got, err := BuildMovementTransactions(f, MovementEvent{
		BinID: 10, FromNodeID: 1, ToNodeID: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil slice when src==dst boundary, got %d txns", len(got))
	}
}

func TestBuildMovement_CrossBoundaryProducesPair(t *testing.T) {
	f := newFakeStore()
	addNode(f, 1, "src", true, 0)
	addNode(f, 2, "dst", true, 0)
	f.totals[1] = map[string]int64{"C1": 3}  // after leaving, src holds 3
	f.totals[2] = map[string]int64{"C1": 12} // after arrival, dst holds 12

	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1"}
	setManifest(bin, []bins.ManifestEntry{{CatID: "C1", Quantity: 5}})
	f.bins[10] = bin

	txns, err := BuildMovementTransactions(f, MovementEvent{
		BinID: 10, FromNodeID: 1, ToNodeID: 2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("expected 2 txns (one per boundary), got %d", len(txns))
	}

	src, dst := txns[0], txns[1]
	if src.NodeID != 1 || src.Delta != -5 || src.TxnType != "decrease" {
		t.Fatalf("src txn wrong: %+v", src)
	}
	if src.QtyAfter != 3 || src.QtyBefore != 8 {
		t.Fatalf("src totals wrong: before=%d after=%d", src.QtyBefore, src.QtyAfter)
	}
	if dst.NodeID != 2 || dst.Delta != 5 || dst.TxnType != "increase" {
		t.Fatalf("dst txn wrong: %+v", dst)
	}
	if dst.QtyAfter != 12 || dst.QtyBefore != 7 {
		t.Fatalf("dst totals wrong: before=%d after=%d", dst.QtyBefore, dst.QtyAfter)
	}
	if src.SourceType != "movement" || dst.SourceType != "movement" {
		t.Fatalf("source type should be 'movement', got src=%q dst=%q", src.SourceType, dst.SourceType)
	}
}

func TestBuildMovement_EmptyManifestNil(t *testing.T) {
	f := newFakeStore()
	addNode(f, 1, "src", true, 0)
	addNode(f, 2, "dst", true, 0)

	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1"} // no manifest
	f.bins[10] = bin

	got, err := BuildMovementTransactions(f, MovementEvent{
		BinID: 10, FromNodeID: 1, ToNodeID: 2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil slice for empty manifest, got %d txns", len(got))
	}
}

// ---------- BuildCorrectionTransactions -----------------------------

func TestBuildCorrection_DiffsProduceSignedDeltas(t *testing.T) {
	f := newFakeStore()
	addNode(f, 1, "boundary", true, 0)
	f.totals[1] = map[string]int64{"C1": 10, "C2": 4}

	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1"}
	f.bins[10] = bin

	old := []bins.ManifestEntry{{CatID: "C1", Quantity: 3}, {CatID: "C2", Quantity: 2}}
	nw := []bins.ManifestEntry{{CatID: "C1", Quantity: 5}} // C1 +2, C2 -2

	txns, err := BuildCorrectionTransactions(f, 10, 1, old, nw, "shift count")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("expected 2 txns, got %d", len(txns))
	}

	byCat := map[string]*cms.Transaction{}
	for _, t := range txns {
		byCat[t.CatID] = t
	}
	if c1 := byCat["C1"]; c1 == nil || c1.Delta != 2 || c1.TxnType != "increase" {
		t.Fatalf("C1 txn wrong: %+v", c1)
	}
	if c2 := byCat["C2"]; c2 == nil || c2.Delta != -2 || c2.TxnType != "decrease" {
		t.Fatalf("C2 txn wrong: %+v", c2)
	}
	for _, tx := range txns {
		if tx.SourceType != "correction" {
			t.Fatalf("expected source_type=correction, got %q", tx.SourceType)
		}
		if tx.Notes != "shift count" {
			t.Fatalf("expected reason in notes, got %q", tx.Notes)
		}
	}
}

func TestBuildCorrection_NoChangesNil(t *testing.T) {
	f := newFakeStore()
	addNode(f, 1, "boundary", true, 0)

	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1"}
	f.bins[10] = bin

	same := []bins.ManifestEntry{{CatID: "C1", Quantity: 4}}
	got, err := BuildCorrectionTransactions(f, 10, 1, same, same, "no-op")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil slice when nothing changed, got %d txns", len(got))
	}
}

func TestBuildCorrection_NoBoundaryUsesNodeItself(t *testing.T) {
	// No synthetic ancestors → the correction is logged against
	// nodeID / node.Name rather than silently dropped.
	f := newFakeStore()
	addNode(f, 1, "plain-root", false, 0)

	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1"}
	f.bins[10] = bin

	old := []bins.ManifestEntry{{CatID: "C1", Quantity: 1}}
	nw := []bins.ManifestEntry{{CatID: "C1", Quantity: 3}}

	txns, err := BuildCorrectionTransactions(f, 10, 1, old, nw, "count")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 1 {
		t.Fatalf("expected 1 txn, got %d", len(txns))
	}
	if txns[0].NodeID != 1 || txns[0].NodeName != "plain-root" {
		t.Fatalf("expected fallback to node itself, got node=%d name=%q",
			txns[0].NodeID, txns[0].NodeName)
	}
}

// ---------- txnType helper -----------------------------------------

func TestTxnType(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{5, "increase"},
		{0, "increase"},
		{-1, "decrease"},
		{-100, "decrease"},
	}
	for _, c := range cases {
		if got := txnType(c.in); got != c.want {
			t.Errorf("txnType(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
