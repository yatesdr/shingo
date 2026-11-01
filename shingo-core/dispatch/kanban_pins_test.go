package dispatch

import (
	"testing"

	"shingocore/store/bins"
)

// kanban_pins_test.go — characterization pins with no database.

// TestKanbanPin_TheKeepersNeedReachesOnlyThePlantWideTier: what the keeper asks
// the finder (Maintainer.nothingToSource, its ask's acquire) names no source and
// an origin: the typed plant-wide search answers, the one search, carrying the
// fill rule's fence, and no group or loader tier runs.
func TestKanbanPin_TheKeepersNeedReachesOnlyThePlantWideTier(t *testing.T) {
	t.Parallel()
	const origin = "11111111-2222-3333-4444-555555555555"
	db := newFakeFinderDB()
	at := pinNode(301, "KP-FOUND-AT")
	db.addNode(at)
	db.addNode(pinGroup(302, "KP-GROUP"))
	db.maintainedType[origin] = "KP-T"
	db.maintainedGroup[origin] = "KP-GROUP"
	db.typedEmpty = map[string]*bins.Bin{"KP-T": {ID: 901, NodeID: &at.ID, NodeName: at.Name}}

	res := NewSourceFinder(db, nil, nil).FindSourceForNeed(SourceNeed{Intent: IntentEmpty, OriginID: origin})

	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 901 {
		t.Fatalf("outcome %v, bin %+v; want the typed plant-wide find", res.Outcome, res.Bin)
	}
	if db.typedGlobalCalls != 1 {
		t.Errorf("typed plant-wide searches = %d, want 1", db.typedGlobalCalls)
	}
	if db.typedGroupCalls != 0 || db.groupEmptyCalls != 0 || db.globalEmptyCalls != 0 || db.fifoCalls != 0 {
		t.Errorf("other searches ran: typed group %d, group %d, untyped plant-wide %d, FIFO %d; want none",
			db.typedGroupCalls, db.groupEmptyCalls, db.globalEmptyCalls, db.fifoCalls)
	}
	if db.lastFence != (bins.EmptyFence{OriginGroup: "KP-GROUP"}) {
		t.Errorf("fence = %+v, want the group it fills and no process", db.lastFence)
	}
}
