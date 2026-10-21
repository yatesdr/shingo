package service

import (
	"context"
	"testing"
)

// TestBuildView_UnloaderAskResolverPopulatesNote: the engine's pending U1 note,
// injected via SetUnloaderAskResolver, lands on the matching node's AskPending
// (the board's card note, the station's ask_pending) and leaves the other
// nodes blank.
func TestBuildView_UnloaderAskResolverPopulatesNote(t *testing.T) {
	db, stationID, pressNodeID, positionNodeID, _ := positionScenario(t)
	svc := NewStationService(db)

	const note = "A full of PART-A is asked of Core; no answer yet"
	svc.SetUnloaderAskResolver(func(coreNodeName string) string {
		if coreNodeName == "PLN_A1" {
			return note
		}
		return ""
	})

	view, err := svc.BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	for i := range view.Nodes {
		n := &view.Nodes[i]
		switch n.Node.ID {
		case pressNodeID:
			if n.AskPending != note {
				t.Errorf("AskPending = %q, want the resolver's note %q", n.AskPending, note)
			}
		case positionNodeID:
			if n.AskPending != "" {
				t.Errorf("position AskPending = %q, want empty", n.AskPending)
			}
		}
	}
}
