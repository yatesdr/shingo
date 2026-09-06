package material

import (
	"encoding/json"
	"strings"
	"testing"

	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// ptrInt64 returns a pointer to i — convenience for building node
// trees with optional ParentID in-line.
func ptrInt64(i int64) *int64 { return &i }

// addBoundary inserts a synthetic root AND tags it with a storeroom code, so a
// movement fixture declares its boundaries rather than inheriting them. Under
// the old default-on predicate a bare synthetic root was a boundary for free;
// it is not any more, and a fixture that forgets the tag emits nothing.
func addBoundary(f *fakeStore, id int64, name, storeroom string) {
	addNode(f, id, name, true, 0)
	f.setProp(id, CMSStoreroomProperty, storeroom)
}

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

// TestFindCMSBoundary_UntaggedSyntheticRootIsNotABoundary is the fail-closed
// pin, and it is the whole point of the cms_storeroom rewrite. The predicate
// this replaced defaulted a parentless synthetic node ON — and nothing wrote
// the property in production, so the default WAS the behaviour. _TRANSIT, every
// node group and every per-robot carrier node were CMS boundaries, and a bin
// picked up by a robot booked a storeroom transfer into "the robot".
func TestFindCMSBoundary_UntaggedSyntheticRootIsNotABoundary(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"_TRANSIT", "_ROBOT:AMR-003", "NGRP-1"} {
		f := newFakeStore()
		addNode(f, 1, name, true, 0) // synthetic, parentless, untagged

		got, code, err := FindCMSBoundary(f, 1)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if got != nil || code != "" {
			t.Errorf("%s is a CMS boundary with no cms_storeroom set: node=%+v code=%q", name, got, code)
		}
	}
}

// TestFindCMSBoundary_TaggedNodeReturnsItsStoreroom: presence is the whole
// predicate, and the value is the answer the caller needs.
func TestFindCMSBoundary_TaggedNodeReturnsItsStoreroom(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addNode(f, 1, "root", true, 0)
	f.setProp(1, CMSStoreroomProperty, "21 SF")

	got, code, err := FindCMSBoundary(f, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.ID != 1 {
		t.Fatalf("expected boundary at node 1, got %+v", got)
	}
	if code != "21 SF" {
		t.Errorf("storeroom = %q, want %q — the property's VALUE is the CMS code, not a flag", code, "21 SF")
	}
}

// TestFindCMSBoundary_WalksUpToTaggedAncestor: depth carries no meaning. A
// tagged child is a boundary and so is a tagged root; the walk stops at the
// nearest one either way.
func TestFindCMSBoundary_WalksUpToTaggedAncestor(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addNode(f, 1, "root", true, 0)
	f.setProp(1, CMSStoreroomProperty, "MAN")
	addNode(f, 2, "mid", true, 1) // synthetic, untagged — walked past
	addNode(f, 3, "leaf", false, 2)

	got, code, err := FindCMSBoundary(f, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.ID != 1 || code != "MAN" {
		t.Fatalf("expected root boundary MAN, got node=%+v code=%q", got, code)
	}

	// Tag the middle node and the nearest one wins.
	f.setProp(2, CMSStoreroomProperty, "SM01")
	got, code, err = FindCMSBoundary(f, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.ID != 2 || code != "SM01" {
		t.Fatalf("expected nearest tagged ancestor SM01, got node=%+v code=%q", got, code)
	}
}

// TestFindCMSBoundary_PropertyReadErrorPropagates is the other half of
// fail-closed, and the half that is easy to get backwards. An unreadable
// property must reach the caller as an error; collapsing it to "not tagged"
// emits nothing for a real move, and treating UNSET as an error would make
// every untagged node a failed walk.
func TestFindCMSBoundary_PropertyReadErrorPropagates(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addNode(f, 1, "root", true, 0)
	f.failProp(1)

	got, code, err := FindCMSBoundary(f, 1)
	if err == nil {
		t.Fatalf("expected the property read error, got nil (node=%+v code=%q)", got, code)
	}
	if got != nil || code != "" {
		t.Errorf("expected nil node and empty code alongside the error, got %+v / %q", got, code)
	}
}

func TestFindCMSBoundary_WalksToRootReturnsRoot(t *testing.T) {
	t.Parallel()
	// Non-synthetic leaf, non-synthetic middle, synthetic root: walk
	// should climb past both non-syntheticals and stop at the root.
	f := newFakeStore()
	addNode(f, 1, "root", true, 0)
	f.setProp(1, CMSStoreroomProperty, "DOCK")
	addNode(f, 2, "mid", false, 1)
	addNode(f, 3, "leaf", false, 2)

	got, code, err := FindCMSBoundary(f, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.ID != 1 || code != "DOCK" {
		t.Fatalf("expected boundary at root (1) with code DOCK, got node=%+v code=%q", got, code)
	}
}

func TestFindCMSBoundary_NoSyntheticAncestor(t *testing.T) {
	t.Parallel()
	// Walk bottoms out at a non-synthetic root → (nil, nil).
	f := newFakeStore()
	addNode(f, 1, "root", false, 0)
	addNode(f, 2, "leaf", false, 1)

	got, code, err := FindCMSBoundary(f, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil || code != "" {
		t.Fatalf("expected no boundary, got node=%+v code=%q", got, code)
	}
}

func TestFindCMSBoundary_CycleReturnsError(t *testing.T) {
	t.Parallel()
	// Two non-synthetic nodes that point at each other. FindCMSBoundary
	// should detect the revisit and return (nil, err) so the engine
	// wrapper can log the anomaly without returning a bogus node.
	f := newFakeStore()
	f.nodes[1] = &nodes.Node{ID: 1, Name: "a", IsSynthetic: false, ParentID: ptrInt64(2)}
	f.nodes[2] = &nodes.Node{ID: 2, Name: "b", IsSynthetic: false, ParentID: ptrInt64(1)}

	got, code, err := FindCMSBoundary(f, 1)
	if err == nil {
		t.Fatalf("expected cycle error, got nil (result=%+v)", got)
	}
	if got != nil || code != "" {
		t.Fatalf("expected nil node and empty code on cycle, got %+v / %q", got, code)
	}
}

// TestFindCMSBoundary_StoreErrorPropagates pins the contract the deleted
// *Engine wrapper used to break: a failed store read is not "no boundary
// here". The walk itself always propagated; the wrapper collapsed it to a
// nil node one layer up, and a transient DB error then emitted zero CMS
// rows for a real physical move with only a log line as evidence.
func TestFindCMSBoundary_StoreErrorPropagates(t *testing.T) {
	t.Parallel()
	f := newFakeStore() // empty — GetNode errors for every id

	got, code, err := FindCMSBoundary(f, 1)
	if err == nil {
		t.Fatalf("expected store error, got nil (result=%+v)", got)
	}
	if got != nil || code != "" {
		t.Fatalf("expected nil node and empty code alongside the error, got %+v / %q", got, code)
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
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "boundary", "SM01") // one boundary, both endpoints

	f.setTemplate(100, "P1", 5, map[string]int64{"C1": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 5}
	setManifest(bin, []bins.ManifestEntry{{CatID: "C1"}})
	f.bins[10] = bin

	got, _, err := BuildMovementTransactions(f, MovementEvent{
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
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")
	// 5 cycles left, one C1 per cycle -> 5 parts on the move.
	f.setTemplate(100, "P1", 24, map[string]int64{"C1": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 5}
	setManifest(bin, []bins.ManifestEntry{{CatID: "C1"}})
	f.bins[10] = bin

	txns, _, err := BuildMovementTransactions(f, MovementEvent{
		BinID: 10, FromNodeID: 1, ToNodeID: 2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("expected 2 txns (one per boundary), got %d", len(txns))
	}

	src, dst := txns[0], txns[1]
	// The signed delta is the whole story — there is no separate type field
	// beside it to disagree with, and no stored before/after totals.
	if src.NodeID != 1 || src.Delta != -5 {
		t.Fatalf("src txn wrong: %+v", src)
	}
	if dst.NodeID != 2 || dst.Delta != 5 {
		t.Fatalf("dst txn wrong: %+v", dst)
	}
	if src.SourceType != "movement" || dst.SourceType != "movement" {
		t.Fatalf("source type should be 'movement', got src=%q dst=%q", src.SourceType, dst.SourceType)
	}
}

func TestBuildMovement_EmptyManifestNil(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")

	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1"} // no manifest
	f.bins[10] = bin

	got, _, err := BuildMovementTransactions(f, MovementEvent{
		BinID: 10, FromNodeID: 1, ToNodeID: 2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil slice for empty manifest, got %d txns", len(got))
	}
}

// TestBuildMovementTransactions_ParseErrorPropagates covers the other
// half of the same failure class: a manifest that will not decode is a
// failure to answer "what is in this bin", not an answer of "nothing".
// The build used to discard the parse error and return (nil, nil), which
// the caller reads as a legitimate no-op.
func TestBuildMovementTransactions_ParseErrorPropagates(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")

	bad := `{"items": [ this is not json`
	f.bins[10] = &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", Manifest: &bad}

	got, _, err := BuildMovementTransactions(f, MovementEvent{
		BinID: 10, FromNodeID: 1, ToNodeID: 2,
	})
	if err == nil {
		t.Fatalf("expected parse error, got nil (txns=%d)", len(got))
	}
	if got != nil {
		t.Fatalf("expected nil slice alongside the error, got %d txns", len(got))
	}
}

// TestBuildMovement_DerivesFromUOPTimesPartsPerCycle is the correctness fix
// this project exists for. The bin manifest carries no count; the quantity on
// the wire is what is physically in the carrier RIGHT NOW.
func TestBuildMovement_DerivesFromUOPTimesPartsPerCycle(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")
	// Two parts per cycle, eight cycles left: sixteen parts move.
	f.setTemplate(100, "P1", 24, map[string]int64{"A": 2})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 8}
	setManifest(bin, []bins.ManifestEntry{{CatID: "A"}})
	f.bins[10] = bin

	txns, _, err := BuildMovementTransactions(f, MovementEvent{BinID: 10, FromNodeID: 1, ToNodeID: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("txns = %d, want 2", len(txns))
	}
	if txns[0].Delta != -16 || txns[1].Delta != 16 {
		t.Errorf("deltas = %d, %d; want -16, +16 (8 cycles x 2 per cycle)", txns[0].Delta, txns[1].Delta)
	}
}

// TestBuildMovement_PartialFillReflectsActualCount is the round-3 finding as a
// regression pin. A bin loaded from a 24-cycle template but holding 5 must ship
// 5, not 24. Before the manifest lost its stored quantity, resolveTemplateManifest
// copied the template's full-bin nominal in regardless of how full the bin was,
// so this shipped 24.
func TestBuildMovement_PartialFillReflectsActualCount(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")
	f.setTemplate(100, "P1", 24, map[string]int64{"A": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 5}
	setManifest(bin, []bins.ManifestEntry{{CatID: "A"}})
	f.bins[10] = bin

	txns, _, err := BuildMovementTransactions(f, MovementEvent{BinID: 10, FromNodeID: 1, ToNodeID: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("txns = %d, want 2", len(txns))
	}
	if txns[0].Delta != -5 {
		t.Errorf("src delta = %d, want -5 (the bin holds 5, not the template's 24)", txns[0].Delta)
	}
}

// TestBuildMovement_MultiPartTemplateProducesTwoRowsPerPart: N parts in the
// template, N x 2 rows per move, each at its own ratio.
func TestBuildMovement_MultiPartTemplateProducesTwoRowsPerPart(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")
	f.setTemplate(100, "P1", 10, map[string]int64{"A": 1, "B": 3})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 4}
	setManifest(bin, []bins.ManifestEntry{{CatID: "A"}, {CatID: "B"}})
	f.bins[10] = bin

	txns, _, err := BuildMovementTransactions(f, MovementEvent{BinID: 10, FromNodeID: 1, ToNodeID: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 4 {
		t.Fatalf("txns = %d, want 4 (two parts, two boundaries)", len(txns))
	}
	byPart := map[string]int64{}
	for _, tx := range txns {
		if tx.Delta > 0 {
			byPart[tx.CatID] = tx.Delta
		}
	}
	if byPart["A"] != 4 || byPart["B"] != 12 {
		t.Errorf("arrival deltas A=%d B=%d, want 4 and 12 (4 cycles at 1 and 3 per cycle)",
			byPart["A"], byPart["B"])
	}
}

// TestBuildMovement_DrainedBinProducesNothing: a bin at zero UOP has nothing to
// transfer, whatever its manifest still lists. The rows would be zero-delta,
// and a zero-delta transfer is noise in an inventory ledger.
func TestBuildMovement_DrainedBinProducesNothing(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")
	f.setTemplate(100, "P1", 24, map[string]int64{"A": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 0}
	setManifest(bin, []bins.ManifestEntry{{CatID: "A"}})
	f.bins[10] = bin

	got, _, err := BuildMovementTransactions(f, MovementEvent{BinID: 10, FromNodeID: 1, ToNodeID: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("drained bin produced %d txns, want none", len(got))
	}
}

// TestBuildMovement_NoTemplateSkips: without a template there is no ratio, so
// there is no count. Skipping is the deliberate choice — a movement row with a
// guessed quantity is indistinguishable from a measured one once it reaches
// CMS, which makes it worse than no row.
//
// P-UNKNOWN, NOT "". An empty payload_code short-circuits partsPerCycle before
// GetPayloadByCode is reached, so the fixture this test used to carry proved
// only that a bare carrier is skipped — the not-found branch it names ran in no
// test at all, and the comment claiming the fake errors for "P-UNKNOWN"
// described a code path the fixture never took.
func TestBuildMovement_NoTemplateSkips(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")
	// No setTemplate call: the fake's GetPayloadByCode returns sql.ErrNoRows.
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P-UNKNOWN", UOPRemaining: 9}
	setManifest(bin, []bins.ManifestEntry{{CatID: "A"}})
	f.bins[10] = bin

	got, uncounted, err := BuildMovementTransactions(f, MovementEvent{BinID: 10, FromNodeID: 1, ToNodeID: 2})
	if err != nil {
		t.Fatalf("a code Core has no template for is not an error: %v", err)
	}
	if got != nil {
		t.Errorf("templateless bin produced %d txns, want none", len(got))
	}
	if uncounted != nil {
		t.Errorf("no template at all is not an uncounted LINE: %+v — the report is for a "+
			"template that exists and falls short, which is a different finding", uncounted)
	}
}

// TestBuildMovement_BareCarrierSkips keeps the case the fixture above used to
// cover by accident: a bin with no payload_code has no template by
// construction, and never reaches the store.
func TestBuildMovement_BareCarrierSkips(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "", UOPRemaining: 9}
	setManifest(bin, []bins.ManifestEntry{{CatID: "A"}})
	f.bins[10] = bin

	got, _, err := BuildMovementTransactions(f, MovementEvent{BinID: 10, FromNodeID: 1, ToNodeID: 2})
	if err != nil {
		t.Fatalf("a bare carrier is not an error: %v", err)
	}
	if got != nil {
		t.Errorf("bare carrier produced %d txns, want none", len(got))
	}
}

// TestBuildMovement_UnreadableTemplateIsAnError is the other side of the
// not-found branch, and the reason that branch has to test the SENTINEL rather
// than merely "err != nil".
//
// A database that cannot answer is not a bin without a template. Collapsing the
// two would turn every transient store failure into a movement silently booked
// at zero — a real move, no rows, nothing said.
func TestBuildMovement_UnreadableTemplateIsAnError(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")
	f.setTemplate(100, "P1", 10, map[string]int64{"A": 1})
	f.failPayloadLookup("P1")
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 9}
	setManifest(bin, []bins.ManifestEntry{{CatID: "A"}})
	f.bins[10] = bin

	got, _, err := BuildMovementTransactions(f, MovementEvent{BinID: 10, FromNodeID: 1, ToNodeID: 2})
	if err == nil {
		t.Fatalf("an unreadable template produced %d txns and no error — a store failure "+
			"must not read as a bin with no template", len(got))
	}
}

// TestBuildMovement_PartNotInTemplateContributesNothing: a manifest line whose
// part the template no longer carries has no ratio, so it contributes no rows —
// while the parts that DO have a ratio still ship. Guessing 1 for the orphan
// would put an invented number in an inventory ledger.
func TestBuildMovement_PartNotInTemplateContributesNothing(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")
	f.setTemplate(100, "P1", 10, map[string]int64{"A": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 6}
	setManifest(bin, []bins.ManifestEntry{{CatID: "A"}, {CatID: "GONE"}})
	f.bins[10] = bin

	txns, _, err := BuildMovementTransactions(f, MovementEvent{BinID: 10, FromNodeID: 1, ToNodeID: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("txns = %d, want 2 (only the part the template can count)", len(txns))
	}
	for _, tx := range txns {
		if tx.CatID != "A" {
			t.Errorf("unexpected row for %q: %+v", tx.CatID, tx)
		}
	}
}

// TestBuildMovement_PartNotInTemplateIsReportedNotSwallowed is the other half
// of the test above, and the half that was missing.
//
// Shipping the countable parts is right; doing it silently is not. GONE
// physically crossed the boundary and will be booked nowhere, and the only
// place that can ever be seen is the report this returns.
func TestBuildMovement_PartNotInTemplateIsReportedNotSwallowed(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")
	f.setTemplate(100, "P1", 10, map[string]int64{"A": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 6}
	setManifest(bin, []bins.ManifestEntry{{CatID: "A"}, {CatID: "GONE"}})
	f.bins[10] = bin

	_, uncounted, err := BuildMovementTransactions(f, MovementEvent{BinID: 10, FromNodeID: 1, ToNodeID: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uncounted == nil {
		t.Fatal("a manifest line the template cannot count was dropped silently")
	}
	if got := strings.Join(uncounted.CatIDs, ","); got != "GONE" {
		t.Errorf("uncounted = %q, want exactly \"GONE\" — once, not once per boundary", got)
	}
	if uncounted.PayloadCode != "P1" {
		t.Errorf("payload = %q, want P1 — the report has to name the template that fell short",
			uncounted.PayloadCode)
	}
}

// TestBuildMovement_AllPartsMissBooksBuildFailure is Defect #2 in miniature,
// and it is the shape a partial-release bin had in production: a manifest whose
// every catid is a payload code, and a template keyed on part numbers.
//
// Nothing is countable, so no rows are produced — and an empty slice is exactly
// what a bin that crossed no boundary returns. Without the report the two are
// indistinguishable, which is how a real physical move booked nothing and every
// count on the health page still read as a quiet plant.
func TestBuildMovement_AllPartsMissBooksBuildFailure(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")
	// The template keys on part numbers; the manifest names the payload code.
	f.setTemplate(100, "P1", 10, map[string]int64{"PART-A": 2})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 8}
	setManifest(bin, []bins.ManifestEntry{{CatID: "P1"}})
	f.bins[10] = bin

	txns, uncounted, err := BuildMovementTransactions(f, MovementEvent{BinID: 10, FromNodeID: 1, ToNodeID: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 0 {
		t.Fatalf("txns = %d, want 0 — nothing in the manifest has a ratio", len(txns))
	}
	if uncounted == nil {
		t.Fatal("a bin whose every manifest line is uncountable returned the same nil report " +
			"as a bin that crossed nothing — this is the silent loss itself")
	}
	if got := strings.Join(uncounted.CatIDs, ","); got != "P1" {
		t.Errorf("uncounted = %q, want \"P1\"", got)
	}
	if uncounted.PayloadCode != "P1" {
		t.Errorf("payload = %q, want P1", uncounted.PayloadCode)
	}
}

// TestBuildMovement_DrainedBinIsNotUncountable draws the line the report must
// not cross, and it is the SELECTIVITY half of the two tests above.
//
// The bin is at zero AND its manifest line has no ratio — both reasons for
// emitting nothing at once. Only one of them is a loss. Nothing was in the bin,
// so nothing went unbooked; a report here would fire on every empty carrier
// crossing a boundary whose template has since dropped a part, and drown the
// real findings in the noise.
//
// The out-of-template catid is load-bearing: with a part the template DOES
// carry, this test passes whether or not the drained guard exists at all.
func TestBuildMovement_DrainedBinIsNotUncountable(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")
	f.setTemplate(100, "P1", 24, map[string]int64{"A": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 0}
	setManifest(bin, []bins.ManifestEntry{{CatID: "GONE"}})
	f.bins[10] = bin

	txns, uncounted, err := BuildMovementTransactions(f, MovementEvent{BinID: 10, FromNodeID: 1, ToNodeID: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 0 {
		t.Fatalf("txns = %d, want 0", len(txns))
	}
	if uncounted != nil {
		t.Errorf("an empty bin was reported as uncountable: %+v — the ratio was there, "+
			"the parts were not", uncounted)
	}
}

// TestBuildMovement_StampsRobotIDAndOrderID: both come from the EVENT. The
// order id used to be read from bin.ClaimedBy, and ApplyArrival releases that
// claim before the event fires — so cms_transactions.order_id was NULL on every
// ordinary AMR delivery. A column written by one path and true only on the
// paths nobody looked at.
func TestBuildMovement_StampsRobotIDAndOrderID(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")
	f.setTemplate(100, "P1", 10, map[string]int64{"A": 1})

	// The bin's claim is ALREADY RELEASED, exactly as it is by event time.
	// Anything reading it would see nothing; the event is the only source.
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 4, ClaimedBy: nil}
	setManifest(bin, []bins.ManifestEntry{{CatID: "A"}})
	f.bins[10] = bin

	txns, _, err := BuildMovementTransactions(f, MovementEvent{
		BinID: 10, FromNodeID: 1, ToNodeID: 2,
		RobotID: "AMR-003", OrderID: 12345,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("txns = %d, want 2", len(txns))
	}
	for _, tx := range txns {
		if tx.RobotID != "AMR-003" {
			t.Errorf("robot_id = %q, want AMR-003 on %+v", tx.RobotID, tx)
		}
		if tx.OrderID == nil || *tx.OrderID != 12345 {
			t.Errorf("order_id = %v, want 12345 — the released claim must not be the source", tx.OrderID)
		}
	}
}

// TestBuildMovement_OperatorDragLeavesRobotAndOrderBlank: a person dragging a
// bin on the board is neither a robot nor an order. Blank is the accurate
// answer, and it has to survive rather than being backfilled from something
// nearby.
func TestBuildMovement_OperatorDragLeavesRobotAndOrderBlank(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "src", "SM01")
	addBoundary(f, 2, "dst", "MAN")
	f.setTemplate(100, "P1", 10, map[string]int64{"A": 1})

	// A claim IS present on the bin — an operator can drag a claimed bin — and
	// it must not be mistaken for the mover.
	claim := int64(999)
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 4, ClaimedBy: &claim}
	setManifest(bin, []bins.ManifestEntry{{CatID: "A"}})
	f.bins[10] = bin

	txns, _, err := BuildMovementTransactions(f, MovementEvent{BinID: 10, FromNodeID: 1, ToNodeID: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("txns = %d, want 2", len(txns))
	}
	for _, tx := range txns {
		if tx.RobotID != "" {
			t.Errorf("robot_id = %q on an operator drag, want empty", tx.RobotID)
		}
		if tx.OrderID != nil {
			t.Errorf("order_id = %v on an operator drag, want NULL — bin.ClaimedBy is not the mover", *tx.OrderID)
		}
	}
}

// TestBuildMovement_StoreroomIsTheCodeNotTheNodeName: the row carries the CMS
// storeroom code, which is what CMS understands, NOT the boundary node's name,
// which is what shingo calls it. The two are deliberately different in this
// fixture because in production they are — a node named "SUPERMARKET-A" is
// storeroom "SM01" — and a test where they matched would pass either way.
func TestBuildMovement_StoreroomIsTheCodeNotTheNodeName(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "SUPERMARKET-A", "SM01")
	addBoundary(f, 2, "MANUFACTURING", "MAN")
	f.setTemplate(100, "P1", 10, map[string]int64{"A": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 3}
	setManifest(bin, []bins.ManifestEntry{{CatID: "A"}})
	f.bins[10] = bin

	txns, _, err := BuildMovementTransactions(f, MovementEvent{BinID: 10, FromNodeID: 1, ToNodeID: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("txns = %d, want 2", len(txns))
	}
	src, dst := txns[0], txns[1]
	if src.Storeroom != "SM01" {
		t.Errorf("src storeroom = %q, want SM01 (the node is named %q)", src.Storeroom, src.NodeName)
	}
	if dst.Storeroom != "MAN" {
		t.Errorf("dst storeroom = %q, want MAN (the node is named %q)", dst.Storeroom, dst.NodeName)
	}
}

// TestBuildMovement_StoreroomComesFromTheTaggedANCESTOR: the code belongs to
// whichever node up the chain carries the property, not to the slot the bin sat
// in. A per-slot lookup would find nothing and ship an empty storeroom, which
// CMS would take as a real (and wrong) location.
func TestBuildMovement_StoreroomComesFromTheTaggedANCESTOR(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 1, "AREA", "DOCK")
	addNode(f, 2, "AISLE", true, 1)   // synthetic, untagged
	addNode(f, 3, "SLOT-7", false, 2) // where the bin actually is
	addBoundary(f, 4, "ELSEWHERE", "SM02")
	f.setTemplate(100, "P1", 10, map[string]int64{"A": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 2}
	setManifest(bin, []bins.ManifestEntry{{CatID: "A"}})
	f.bins[10] = bin

	txns, _, err := BuildMovementTransactions(f, MovementEvent{BinID: 10, FromNodeID: 3, ToNodeID: 4})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("txns = %d, want 2", len(txns))
	}
	if txns[0].Storeroom != "DOCK" {
		t.Errorf("src storeroom = %q, want DOCK from the tagged ancestor two levels up", txns[0].Storeroom)
	}
	if txns[1].Storeroom != "SM02" {
		t.Errorf("dst storeroom = %q, want SM02", txns[1].Storeroom)
	}
}
