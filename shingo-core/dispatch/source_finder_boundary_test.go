package dispatch

import (
	"errors"
	"testing"

	"shingocore/store/bins"
)

// source_finder_boundary_test.go — MG3-2. What happens when a need meets a fence
// it NAMED.

// ── MG3-2: the explicit-group boundary ──────────────────────────────────────

// A need that names a strict group it is not supported at PARKS WITH A CAUSE.
//
// NEITHER SILENTLY SERVED NOR SILENTLY WIDENED, which is two separate
// assertions because they are two separate ways to get this wrong:
//
//   - serving it would defeat the fence at the one place the fence is most
//     explicit — somebody configured a claim to source from that group;
//   - widening to the plant-wide scan would be the Hopkinsville
//     wrong-supermarket pull, and being fenced out is not a reason to start
//     doing it.
func TestGroupBoundary_FencedNeedParksAndDoesNotWiden(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(pinGroup(300, "BND-STRICT-GRP"))
	db.addNode(pinNode(301, "BND-DEST"))
	db.fencedGroups[300] = true
	// Material everywhere the need could reach if it widened.
	db.groupEmpty = &bins.Bin{ID: 3000, Label: "BND-IN-GROUP"}
	db.globalEmpty = &bins.Bin{ID: 3001, Label: "BND-PLANT-WIDE"}

	got := NewSourceFinder(db, nil, nil).FindSourceForNeed(SourceNeed{
		SourceNode: "BND-STRICT-GRP", DeliveryNode: "BND-DEST",
		PayloadCode: "PANEL-A", Intent: IntentEmpty, ProcessNode: "BND-OUTSIDER",
	})

	if got.Outcome != OutcomeWait {
		t.Fatalf("Outcome = %v, want Wait — a fenced need does not get served", got.Outcome)
	}
	if got.QueueCause != CauseFinderGroupFenced {
		t.Errorf("QueueCause = %q, want %q. 'The group is empty' would send an operator to "+
			"look for material standing right in front of them; the fact is that the group "+
			"is not this asker's", got.QueueCause, CauseFinderGroupFenced)
	}
	if db.groupEmptyCalls != 0 || db.typedGroupCalls != 0 {
		t.Errorf("group finders were called (%d untyped, %d typed) — the boundary is decided "+
			"BEFORE the group is searched, or a fenced need still learns what is in there",
			db.groupEmptyCalls, db.typedGroupCalls)
	}
	if db.globalEmptyCalls != 0 || db.typedGlobalCalls != 0 {
		t.Errorf("the need widened to the plant-wide scan (%d untyped, %d typed). A scoped "+
			"need that widens is the wrong-supermarket pull",
			db.globalEmptyCalls, db.typedGlobalCalls)
	}
}

// A SUPPORTED need is served from the group exactly as before.
func TestGroupBoundary_SupportedNeedIsUnaffected(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(pinGroup(310, "BND-OK-GRP"))
	at := int64(311)
	db.addNode(pinNode(at, "BND-OK-SLOT"))
	db.addNode(pinNode(312, "BND-OK-DEST"))
	// Not fenced against this asker.
	db.groupEmpty = &bins.Bin{ID: 3100, Label: "BND-OK-BIN", NodeID: &at}

	got := NewSourceFinder(db, nil, nil).FindSourceForNeed(SourceNeed{
		SourceNode: "BND-OK-GRP", DeliveryNode: "BND-OK-DEST",
		PayloadCode: "PANEL-A", Intent: IntentEmpty, ProcessNode: "BND-PRESS-1",
	})
	if got.Outcome != OutcomeFound || got.Bin == nil || got.Bin.ID != 3100 {
		t.Fatalf("outcome=%v bin=%v, want the group's carrier. Dedication binds OUTSIDERS, "+
			"not the press the group serves", got.Outcome, got.Bin)
	}
}

// AN UNREADABLE FENCE PARKS, and does not serve.
//
// Whether this need may be served is UNKNOWN when the read fails, and unknown
// is not permission. It parks under the unreadable cause — the same disposition
// every other failed read in the cascade gets — rather than falling through to
// the group, which would make a database blip the way past a fence.
func TestGroupBoundary_UnreadableFenceParks(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(pinGroup(320, "BND-ERR-GRP"))
	db.addNode(pinNode(321, "BND-ERR-DEST"))
	db.fencedGroupErr = errors.New("connection refused")
	db.groupEmpty = &bins.Bin{ID: 3200, Label: "BND-ERR-BIN"}

	got := NewSourceFinder(db, nil, nil).FindSourceForNeed(SourceNeed{
		SourceNode: "BND-ERR-GRP", DeliveryNode: "BND-ERR-DEST",
		PayloadCode: "PANEL-A", Intent: IntentEmpty, ProcessNode: "BND-SOMEBODY",
	})
	if got.Outcome != OutcomeWait {
		t.Fatalf("Outcome = %v, want Wait", got.Outcome)
	}
	if got.QueueCause != CauseFinderSourceUnreadable {
		t.Errorf("QueueCause = %q, want %q — a fence that could not be read is not a fence "+
			"that said yes", got.QueueCause, CauseFinderSourceUnreadable)
	}
	if db.groupEmptyCalls != 0 {
		t.Error("the group was searched after the fence read failed — a database blip must " +
			"not be the way past a fence")
	}
}
