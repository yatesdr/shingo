package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/plc"
)

// board_nudge_test.go — the engine doors that change what a station view reads
// with no scoped event of their own. The board does not poll, so each says so
// with one EventBoardNudge (the SSE hub's coalesced nudge, every station takes
// it). Each emits at the change, not on every pass: the keeper and the curtain
// poll run on timers, and a nudge per pass would be a poll by another name.

func boardNudges(eng *Engine) *int {
	n := 0
	eng.Events.Subscribe(func(evt Event) {
		if evt.Type == EventBoardNudge {
			n++
		}
	})
	return &n
}

// TestBoardNudge_CoreLoadersAndCatalogApplied: Core's loader list (the tiles'
// loader block) and its payload catalog (the carriers a tile offers) are read
// by the view and changed at apply, with no event.
func TestBoardNudge_CoreLoadersAndCatalogApplied(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	n := boardNudges(eng)

	if err := eng.SetCoreLoaders([]protocol.LoaderInfo{{Name: "L", LoaderKey: "loader:1", Role: "produce",
		Layout: "shared_window", Positions: []protocol.LoaderPosition{{CoreNodeName: "SMN_1", Ordinal: 1}}}}); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	if *n != 1 {
		t.Fatalf("SetCoreLoaders gave %d board nudges, want 1", *n)
	}
	eng.SetPayloadBinTypes([]protocol.PayloadBinTypeInfo{{PayloadCode: "PART-A", BinTypeCode: "TOTE-1"}})
	if *n != 2 {
		t.Fatalf("SetPayloadBinTypes gave %d board nudges in all, want 2", *n)
	}
}

// TestBoardNudge_KeepStagedNoteOnChangeOnly: the keeper sets its sentence on
// every sweep; only a sentence that changed is news to a board.
func TestBoardNudge_KeepStagedNoteOnChangeOnly(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	n := boardNudges(eng)

	steps := []struct {
		note string
		want int
	}{
		{"Waiting for the refill", 1},
		{"Waiting for the refill", 1},
		{"Paused by a cancel", 2},
		{"", 3},
		{"", 3},
	}
	for _, s := range steps {
		eng.setKeepStagedNote("PLN_1", s.note)
		if *n != s.want {
			t.Fatalf("after note %q: %d board nudges, want %d", s.note, *n, s.want)
		}
	}
}

// TestBoardNudge_CurtainTagMoved: a station view stamps CurtainOK from the PLC
// cache; a watched tag whose reading changed nudges the boards, through the
// PLC manager's emitter.
func TestBoardNudge_CurtainTagMoved(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	n := boardNudges(eng)
	// As engine.New builds it: the manager emits through the engine's bus.
	mgr := plc.NewManager(eng.db, eng.cfg, &plcEmitter{bus: eng.Events}, nil)

	mgr.WatchCurtainTag("PLC-1", "CURTAIN_A")
	mgr.SetTagValueForTest("PLC-1", "CURTAIN_A", false)
	mgr.SetTagValueForTest("PLC-1", "CURTAIN_A", false)
	mgr.SetTagValueForTest("PLC-1", "COUNTER", 41)
	mgr.SetTagValueForTest("PLC-1", "CURTAIN_A", true)
	if *n != 2 {
		t.Fatalf("%d board nudges, want 2: the watched tag's first reading and its flip (an "+
			"unwatched tag and a repeated reading are not news)", *n)
	}
}
