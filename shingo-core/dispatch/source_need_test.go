package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// source_need_test.go — C(i): the need-shaped finder seam.
//
// C(i) is a ratchet, not a behavior change. These tests pin the two properties
// the ratchet exists for — NodeLocal makes tier 5 unreachable by type, and the
// tier-4 IntentEmpty extension is real but DORMANT — plus the adapter identity
// that keeps every existing caller byte-equivalent.

// TestSourceNeed_NodeLocalEmptyNeverCoOccursToday pins the dormancy claim at
// its anchor: SourceIntentForType is the ONLY producer of the Local intent, and
// it pairs Local with Move — a full-intent shape. RetrieveEmpty (the only
// empty-intent producer) maps to SourceIntentEmpty, never Local. So no caller
// can reach tier 4's empty branch until C(ii) constructs a need by hand, which
// is exactly the extension's purpose.
func TestSourceNeed_NodeLocalEmptyNeverCoOccursToday(t *testing.T) {
	t.Parallel()

	for _, ot := range []protocol.OrderType{
		OrderTypeRetrieve, OrderTypeRetrieveEmpty, OrderTypeMove,
		OrderTypeStore, OrderTypeComplex,
	} {
		intent := SourceIntentForType(ot)
		isLocal := intent == SourceIntentLocal
		isEmpty := ot == OrderTypeRetrieveEmpty

		if isLocal && isEmpty {
			t.Fatalf("%s stamps Local AND is the empty-intent type — the tier-4 extension is no longer dormant; re-verify C(i)'s no-behavior-change claim", ot)
		}
		if isLocal && ot != OrderTypeMove {
			t.Errorf("%s stamps SourceIntentLocal; only Move may (the dormancy argument rests on it)", ot)
		}
	}
}

// TestFindSourceForNeed_NodeLocalEmpty exercises the tier-4 IntentEmpty
// extension directly — the C(ii)-enabling path. A node-local empty need must
// resolve to the EMPTY carrier at its node (never the full), queue scoped when
// none is present, and NEVER touch a plant-wide finder in either case.
func TestFindSourceForNeed_NodeLocalEmpty(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	srcID := int64(700)
	db.addNode(&nodes.Node{ID: srcID, Name: "SEAT-SRC"})
	full := &bins.Bin{ID: 71, PayloadCode: "PART-X", NodeID: &srcID, Status: "available"}
	empty := &bins.Bin{ID: 72, PayloadCode: "", NodeID: &srcID, Status: "available"}
	db.addBin(full)
	db.addBin(empty)
	// A plant-wide empty exists and must NOT be reached.
	db.globalEmpty = &bins.Bin{ID: 99, PayloadCode: "", Status: "available"}

	finder := NewSourceFinder(db, nil, nil)
	need := SourceNeed{SourceNode: "SEAT-SRC", PayloadCode: "PART-X", Intent: IntentEmpty, NodeLocal: true}

	res := finder.FindSourceForNeed(need)
	if res.Outcome != OutcomeFound {
		t.Fatalf("outcome = %v, want Found", res.Outcome)
	}
	if res.Bin == nil || res.Bin.ID != empty.ID {
		t.Errorf("bin = %v, want the EMPTY carrier %d (the full must be filtered out)", res.Bin, empty.ID)
	}
	if db.globalEmptyCalls != 0 || db.fifoCalls != 0 {
		t.Errorf("plant-wide finders called (%d empty, %d fifo) — NodeLocal must make tier 5 unreachable",
			db.globalEmptyCalls, db.fifoCalls)
	}

	// Node dry: scoped Wait, still no widening.
	db.binsByNode[srcID] = []*bins.Bin{full} // only the full remains
	res = finder.FindSourceForNeed(need)
	if res.Outcome != OutcomeWait {
		t.Fatalf("dry node: outcome = %v, want Wait", res.Outcome)
	}
	if res.QueueCause != "finder-node-empty" {
		t.Errorf("QueueCause = %q, want finder-node-empty (the scoped sentence)", res.QueueCause)
	}
	if res.QueueParams.Kind != "empty" || res.QueueParams.Destination != "SEAT-SRC" {
		t.Errorf("QueueParams = %+v, want Kind=empty Destination=SEAT-SRC", res.QueueParams)
	}
	if db.globalEmptyCalls != 0 || db.fifoCalls != 0 {
		t.Errorf("dry node widened plant-wide (%d empty, %d fifo) — it must QUEUE, never widen",
			db.globalEmptyCalls, db.fifoCalls)
	}
}

// TestFindSource_AdapterIdentity pins that the order-shaped entry point is a
// pure projection onto the need-shaped one — same fake, same order-derived
// need, same outcome — so no existing caller's behavior moved under C(i).
func TestFindSource_AdapterIdentity(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	srcID := int64(710)
	db.addNode(&nodes.Node{ID: srcID, Name: "ID-SRC"})
	db.addNode(&nodes.Node{ID: 711, Name: "ID-DEST"})
	db.addBin(&bins.Bin{ID: 73, PayloadCode: "P", NodeID: &srcID, Status: "available"})

	finder := NewSourceFinder(db, nil, nil)
	viaOrder := finder.FindSource(&orders.Order{
		SourceNode: "ID-SRC", DeliveryNode: "ID-DEST", PayloadCode: "P",
		SourceIntent: SourceIntentLocal,
	}, IntentFull)
	viaNeed := finder.FindSourceForNeed(SourceNeed{
		SourceNode: "ID-SRC", DeliveryNode: "ID-DEST", PayloadCode: "P",
		Intent: IntentFull, NodeLocal: true,
	})

	if viaOrder.Outcome != viaNeed.Outcome {
		t.Fatalf("adapter drift: order-shaped %v vs need-shaped %v", viaOrder.Outcome, viaNeed.Outcome)
	}
	if viaOrder.Bin.ID != viaNeed.Bin.ID || viaOrder.Node.Name != viaNeed.Node.Name {
		t.Errorf("adapter drift: bin/node differ (%d/%s vs %d/%s)",
			viaOrder.Bin.ID, viaOrder.Node.Name, viaNeed.Bin.ID, viaNeed.Node.Name)
	}
}
