package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/store/nodes"
)

func wantText(h domain.Height) string {
	if n, ok := h.Stated(); ok {
		return fmt.Sprint(n)
	}
	return "none"
}

// TestToteWant_ReachesTheFinder: the height read with the set-down node is the
// one the empty finders rank by, on both empty tiers and both shapes of need:
// a complex leg (set down at its next dropoff, no delivery node) and a plain
// retrieve (set down at its delivery node). A delivery node that is not where
// the carrier is set down says nothing about its height, and a keeper ask
// (pinned to its episode's type) reads no destination and wants none.
func TestToteWant_ReachesTheFinder(t *testing.T) {
	t.Parallel()
	five := protocol.Layers(5)
	for _, tc := range []struct {
		name string
		need SourceNeed
		want domain.Height
	}{
		{"a complex leg from a group", SourceNeed{SourceNode: "TW-GRP", PayloadCode: "P",
			Intent: IntentEmpty, SetDownNode: "TW-OUT"}, five},
		{"a plain retrieve, plant-wide", SourceNeed{PayloadCode: "P", Intent: IntentEmpty,
			DeliveryNode: "TW-OUT", SetDownNode: "TW-OUT"}, five},
		{"a plain retrieve from a group", SourceNeed{SourceNode: "TW-GRP", PayloadCode: "P",
			Intent: IntentEmpty, DeliveryNode: "TW-OUT", SetDownNode: "TW-OUT"}, five},
		{"a delivery node that is not the set-down node", SourceNeed{PayloadCode: "P",
			Intent: IntentEmpty, DeliveryNode: "TW-OUT"}, domain.Height{}},
		{"no destination", SourceNeed{PayloadCode: "P", Intent: IntentEmpty}, domain.Height{}},
		{"a keeper ask", SourceNeed{PayloadCode: "P", Intent: IntentEmpty, OriginID: "ep-1",
			DeliveryNode: "TW-OUT", SetDownNode: "TW-OUT"}, domain.Height{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newFakeFinderDB()
			db.addNode(&nodes.Node{ID: 1, Name: "TW-GRP", IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP})
			db.addNode(&nodes.Node{ID: 2, Name: "TW-OUT"})
			db.wantAt = map[string]int{"TW-OUT": 5}
			db.maintainedType = map[string]string{"ep-1": "TW-TOTE"}
			NewSourceFinder(db, nil, nil).FindSourceForNeed(tc.need)
			if wantText(db.lastWant) != wantText(tc.want) {
				t.Errorf("the finder ranked by %s layers, want %s", wantText(db.lastWant), wantText(tc.want))
			}
		})
	}
}
