package material

import (
	"strings"
	"testing"

	"shingocore/store/bins"
)

// material_clear_test.go — BuildClearTransactions, the unloader's departure.
//
// At Hopkinsville the unloader takes bins out of the AMR supermarket by
// CLEARING them and moving the material to another CMS zone, so the clear is
// the departure. The arrival used to post and the departure did not, and the
// storeroom climbed forever.
//
// Every test here was verified RED against a deliberately broken tree before
// being trusted — see the note on each one where the break is not obvious.

// TestBuildClear_AtBoundaryBooksTheDeparture is the feature. The deltas are
// NEGATIVE and they sum to what the bin held at the moment of the clear, which
// is the whole contract: CMS has to see the material leave.
//
// Verified red by returning cms.SourceTypeMovement's positive sign from
// rowsAtBoundary (+1 instead of -1): the sum assertion fails.
func TestBuildClear_AtBoundaryBooksTheDeparture(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 14, "Supermarket Area", "AMR_SUPERMARKET_TEST")
	addNode(f, 20, "SMN_01", false, 14)
	// 10 cycles left at 24 parts per cycle -> 240 parts leaving.
	f.setTemplate(100, "KK21 5019 A PIA14", 12000, map[string]int64{"KK21 5019 A PIA14": 24})
	bin := &bins.Bin{ID: 10, Label: "BIN-0912", PayloadCode: "KK21 5019 A PIA14", UOPRemaining: 10}
	setManifest(bin, []bins.ManifestEntry{{PartNumber: "KK21 5019 A PIA14"}})
	f.bins[10] = bin

	txns, report, err := BuildClearTransactions(f, ClearEvent{BinID: 10, NodeID: 20})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report != nil {
		t.Errorf("a fully countable bin reported uncounted lines: %+v", report)
	}
	if len(txns) != 1 {
		t.Fatalf("built %d rows, want 1 (one manifest line, one boundary)", len(txns))
	}
	got := txns[0]
	if got.Delta != -240 {
		t.Errorf("delta = %d, want -240 (10 cycles x 24 parts, LEAVING)", got.Delta)
	}
	if got.NodeID != 14 || got.NodeName != "Supermarket Area" {
		t.Errorf("row names node %d/%q, want the BOUNDARY 14/Supermarket Area", got.NodeID, got.NodeName)
	}
	if got.Storeroom != "AMR_SUPERMARKET_TEST" {
		t.Errorf("storeroom = %q, want the walk's code", got.Storeroom)
	}
	if got.SourceType != "clear" {
		t.Errorf("source type = %q, want clear — a clear is not a movement, and a movement "+
			"is a PAIR whose other half a reader is entitled to expect", got.SourceType)
	}
}

// TestBuildClear_DeltasSumToThePreClearContents states the property over a
// multi-line bin rather than at one convenient line count: whatever the bin
// held before the clear is what leaves, no more and no less.
func TestBuildClear_DeltasSumToThePreClearContents(t *testing.T) {
	t.Parallel()
	for _, uop := range []int{1, 3, 25, 500} {
		f := newFakeStore()
		addBoundary(f, 14, "Supermarket Area", "SM01")
		addNode(f, 20, "SMN_01", false, 14)
		f.setTemplate(100, "KIT", 1000, map[string]int64{"51015-LH": 2, "51015-RH": 3})
		bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "KIT", UOPRemaining: uop}
		setManifest(bin, []bins.ManifestEntry{{PartNumber: "51015-LH"}, {PartNumber: "51015-RH"}})
		f.bins[10] = bin

		txns, _, err := BuildClearTransactions(f, ClearEvent{BinID: 10, NodeID: 20})
		if err != nil {
			t.Fatalf("uop %d: unexpected error: %v", uop, err)
		}
		if len(txns) != 2 {
			t.Fatalf("uop %d: built %d rows, want 2 (one per line)", uop, len(txns))
		}
		var sum int64
		for _, tx := range txns {
			if tx.Delta >= 0 {
				t.Errorf("uop %d: row for %s has delta %d — a departure is negative", uop, tx.CatID, tx.Delta)
			}
			sum += tx.Delta
		}
		want := int64(uop) * -5 // 2 + 3 parts per cycle
		if sum != want {
			t.Errorf("uop %d: deltas sum to %d, want %d", uop, sum, want)
		}
	}
}

// TestBuildClear_UntaggedNodeProducesNothing. An untagged clear is invisible to
// CMS and that is CORRECT — the plant is full of carriers nobody has told CMS
// about, and booking a departure from a storeroom that does not exist is worse
// than booking nothing.
//
// It also pins the blast radius: the boundary is resolved before the bin is
// read, so nothing below it — the manifest parse, the template lookup, the
// refusal on a line naming no part — can fail for a site that tagged nothing.
// The bin here carries a template whose line names NO PART — the one input that
// makes a tagged site's build refuse. An untagged site must never see that
// refusal, and the only thing standing between it and one is the order of the two
// resolutions: move the boundary check below readBinContents and this test goes
// red with an error a plant that configured nothing has no business getting.
func TestBuildClear_UntaggedNodeProducesNothing(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addNode(f, 14, "Supermarket Area", true, 0) // synthetic, parentless, UNTAGGED
	addNode(f, 20, "SMN_01", false, 14)
	f.setUncorrectedTemplate(100, "P1", 24, map[string]int64{"C1": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 5}
	setManifest(bin, []bins.ManifestEntry{{PartNumber: "C1"}})
	f.bins[10] = bin

	txns, report, err := BuildClearTransactions(f, ClearEvent{BinID: 10, NodeID: 20})
	if err != nil {
		t.Fatalf("an untagged clear FAILED: %v — nothing below the boundary check may run "+
			"for a site that has tagged nothing", err)
	}
	if txns != nil {
		t.Errorf("an untagged clear built %d rows, want none: %+v", len(txns), txns)
	}
	if report != nil {
		t.Errorf("an untagged clear reported uncounted lines: %+v — nothing crossed a "+
			"boundary, so nothing went uncounted", report)
	}
}

// TestBuildClear_FailedBoundaryLookupIsNotNoBoundary is the fail-loud pin, and
// the one the whole three-valued walk exists for. A property read that FAILS is
// a third answer: read as "no boundary" it books nothing for material that
// physically left, and the clear that destroys the evidence still succeeds.
//
// Verified red by swallowing the error in BuildClearTransactions
// (`if err != nil { return nil, nil, nil }`): the test then sees a nil error and
// no rows, which is exactly the silence it exists to refuse.
func TestBuildClear_FailedBoundaryLookupIsNotNoBoundary(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addNode(f, 14, "Supermarket Area", true, 0)
	addNode(f, 20, "SMN_01", false, 14)
	f.failProp(14) // the read fails; it does not answer "untagged"
	f.setTemplate(100, "P1", 24, map[string]int64{"C1": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 5}
	setManifest(bin, []bins.ManifestEntry{{PartNumber: "C1"}})
	f.bins[10] = bin

	txns, report, err := BuildClearTransactions(f, ClearEvent{BinID: 10, NodeID: 20})
	if err == nil {
		t.Fatalf("a FAILED boundary lookup returned no error — it is indistinguishable "+
			"from an untagged node, and both then emit nothing (rows=%d)", len(txns))
	}
	if !strings.Contains(err.Error(), "property read failed") {
		t.Errorf("error = %v, want the store's own failure — a wrapped-away cause cannot "+
			"be told from a cycle or a missing node", err)
	}
	if txns != nil || report != nil {
		t.Errorf("a failed lookup returned rows=%+v report=%+v, want neither", txns, report)
	}
}

// TestBuildClear_CycleInTheTreeIsAnError covers the walk's other failure: a
// malformed parent chain is a failure to LOCATE the boundary, not a finding that
// there is none above this node.
func TestBuildClear_CycleInTheTreeIsAnError(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addNode(f, 1, "a", true, 2)
	addNode(f, 2, "b", true, 1) // a -> b -> a
	f.bins[10] = &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 5}

	_, _, err := BuildClearTransactions(f, ClearEvent{BinID: 10, NodeID: 1})
	if err == nil {
		t.Fatal("a parent-chain cycle returned no error")
	}
}

// TestBuildClear_DrainedBinProducesNothingAndReportsNothing. uop_remaining of
// zero makes every count zero for a reason the template answered perfectly well,
// so it is not an uncountable line — reporting it would put a finding on every
// empty carrier the unloader ever clears, which is most of them.
//
// BOTH TEMPLATES ARE COVERED, AND THE SECOND IS THE ONE THAT PINS THE GUARD. A
// drained bin whose line the template DOES count never reaches the uncounted
// branch anyway (the ratio is positive), so it cannot tell whether the
// uop_remaining > 0 guard is there. Only a drained bin whose line has NO ratio
// distinguishes them: with the guard, silence; without it, a finding on every
// empty carrier in the plant. The first case was the whole test once, and it went
// green against a tree with the guard deleted.
func TestBuildClear_DrainedBinProducesNothingAndReportsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		uop      int
		perCycle map[string]int64
	}{
		{"drained, template counts the line", 0, map[string]int64{"C1": 1}},
		{"drained, template counts nothing", 0, map[string]int64{"OTHER": 1}},
		{"overpacked past zero, template counts nothing", -4, map[string]int64{"OTHER": 1}},
		{"overpacked past zero, template counts the line", -4, map[string]int64{"C1": 1}},
	} {
		f := newFakeStore()
		addBoundary(f, 14, "Supermarket Area", "SM01")
		addNode(f, 20, "SMN_01", false, 14)
		f.setTemplate(100, "P1", 24, tc.perCycle)
		bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: tc.uop}
		setManifest(bin, []bins.ManifestEntry{{PartNumber: "C1"}})
		f.bins[10] = bin

		txns, report, err := BuildClearTransactions(f, ClearEvent{BinID: 10, NodeID: 20})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if txns != nil {
			t.Errorf("%s: built %d rows, want none", tc.name, len(txns))
		}
		if report != nil {
			t.Errorf("%s: reported %+v — a drained bin is not an uncountable one, and a "+
				"finding here lands on every empty carrier the unloader clears", tc.name, report)
		}
	}
}

// TestBuildClear_UncountableLineIsReportedNotSwallowed. The build succeeded and
// part of the answer is missing: material left the storeroom with no ratio to
// count it by. On a movement that loss comes back on the bin's next load; on a
// CLEAR it does not, because the clear IS the end of this load.
func TestBuildClear_UncountableLineIsReportedNotSwallowed(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 14, "Supermarket Area", "SM01")
	addNode(f, 20, "SMN_01", false, 14)
	// The template counts C1 and says nothing about C2.
	f.setTemplate(100, "P1", 24, map[string]int64{"C1": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 5}
	setManifest(bin, []bins.ManifestEntry{{PartNumber: "C1"}, {PartNumber: "C2"}})
	f.bins[10] = bin

	txns, report, err := BuildClearTransactions(f, ClearEvent{BinID: 10, NodeID: 20})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 1 {
		t.Fatalf("built %d rows, want 1 (C1 only)", len(txns))
	}
	if report == nil {
		t.Fatal("C2 left the storeroom uncounted and nothing reported it")
	}
	if len(report.CatIDs) != 1 || report.CatIDs[0] != "C2" {
		t.Errorf("report names %v, want [C2]", report.CatIDs)
	}
}

// TestBuildClear_NoRobotAndNoOrder. A clear is a person at a station, so there
// is no AMR in Resource and no order to key it to. An invented resource would be
// a claim about the plant that is not true.
func TestBuildClear_NoRobotAndNoOrder(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 14, "Supermarket Area", "SM01")
	addNode(f, 20, "SMN_01", false, 14)
	f.setTemplate(100, "P1", 24, map[string]int64{"C1": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 5}
	setManifest(bin, []bins.ManifestEntry{{PartNumber: "C1"}})
	f.bins[10] = bin

	txns, _, err := BuildClearTransactions(f, ClearEvent{BinID: 10, NodeID: 20})
	if err != nil || len(txns) != 1 {
		t.Fatalf("build failed: rows=%d err=%v", len(txns), err)
	}
	if txns[0].RobotID != "" {
		t.Errorf("RobotID = %q, want blank — no robot cleared this bin", txns[0].RobotID)
	}
	if txns[0].OrderID != nil {
		t.Errorf("OrderID = %v, want nil", *txns[0].OrderID)
	}
}

// TestBuildClear_UnresolvedTemplateLineRefusesToBuild. A line that points at no
// part still holds whatever was typed into a box labelled CATID, and posting
// that identifies the departure by a number the middleware has never seen. The
// refusal is the same one movements get — and the caller does NOT turn it into a
// blocked door, because that would make the bin permanently unclearable.
func TestBuildClear_UnresolvedTemplateLineRefusesToBuild(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 14, "Supermarket Area", "SM01")
	addNode(f, 20, "SMN_01", false, 14)
	f.setUncorrectedTemplate(100, "P1", 24, map[string]int64{"C1": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 5}
	setManifest(bin, []bins.ManifestEntry{{PartNumber: "C1"}})
	f.bins[10] = bin

	txns, _, err := BuildClearTransactions(f, ClearEvent{BinID: 10, NodeID: 20})
	if err == nil {
		t.Fatalf("a line naming no part built %d rows, want a refusal", len(txns))
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error = %v, want it to say a line names no part", err)
	}
}

// TestBuildClear_BareCarrierAndNoTemplateProduceNothing. A carrier with no
// payload has no template by construction, and an unknown payload code has none
// either; a departure row with a guessed quantity is worse than no row, because
// once it reaches CMS it is indistinguishable from a measured one.
func TestBuildClear_BareCarrierAndNoTemplateProduceNothing(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"", "NEVER-SEEN"} {
		f := newFakeStore()
		addBoundary(f, 14, "Supermarket Area", "SM01")
		addNode(f, 20, "SMN_01", false, 14)
		bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: code, UOPRemaining: 5}
		setManifest(bin, []bins.ManifestEntry{{PartNumber: "C1"}})
		f.bins[10] = bin

		txns, report, err := BuildClearTransactions(f, ClearEvent{BinID: 10, NodeID: 20})
		if err != nil {
			t.Fatalf("payload %q: unexpected error: %v", code, err)
		}
		if txns != nil || report != nil {
			t.Errorf("payload %q: rows=%+v report=%+v, want neither", code, txns, report)
		}
	}
}

// TestBuildClear_TaggedNodeItselfIsItsOwnBoundary: the walk returns the node
// when the node carries the property, so tagging a single station works without
// a group above it.
func TestBuildClear_TaggedNodeItselfIsItsOwnBoundary(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	addBoundary(f, 20, "SMN_01", "SM01") // the node itself, synthetic and tagged
	f.setTemplate(100, "P1", 24, map[string]int64{"C1": 1})
	bin := &bins.Bin{ID: 10, Label: "B10", PayloadCode: "P1", UOPRemaining: 5}
	setManifest(bin, []bins.ManifestEntry{{PartNumber: "C1"}})
	f.bins[10] = bin

	txns, _, err := BuildClearTransactions(f, ClearEvent{BinID: 10, NodeID: 20})
	if err != nil || len(txns) != 1 {
		t.Fatalf("rows=%d err=%v, want 1 row", len(txns), err)
	}
	if txns[0].NodeID != 20 || txns[0].Delta != -5 {
		t.Errorf("row = node %d delta %d, want node 20 delta -5", txns[0].NodeID, txns[0].Delta)
	}
}
