package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// A LOAD THE CHANGEOVER ASKED FOR IS THE CHANGEOVER'S DEMAND.
//
// Attributed to the cell instead, it reads in demand-origin reporting as an
// orphan replenishment nobody can tie to the changeover it served — and the
// changeover's own expected-vs-actual ratio under-counts by exactly the bins
// it caused. That is the same units mistake the episode grain exists to
// prevent, one layer up.
func TestChangeoverLoadOrigin(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	processID, err := db.CreateProcess("CLO-PROC", "", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	node := &processes.Node{ID: 1, ProcessID: processID, CoreNodeName: "SMN_014", Name: "SMN_014"}

	on := &processes.NodeClaim{
		CoreNodeName:            "SMN_014",
		Role:                    protocol.ClaimRoleProduce,
		SwapMode:                protocol.SwapModeManualSwap,
		ChangeoverLoadDirective: true,
	}
	off := &processes.NodeClaim{
		CoreNodeName: "SMN_014",
		Role:         protocol.ClaimRoleProduce,
		SwapMode:     protocol.SwapModeManualSwap,
	}

	// No changeover running: nothing to attribute to, whatever the flag says.
	if o := eng.changeoverLoadOrigin(node, on); o.ID != "" {
		t.Errorf("no active changeover must yield no origin; got %+v", o)
	}

	toStyleID, err := db.CreateStyle("CLO-STYLE", "", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	// Inserted directly: the engine's start path plans and dispatches, and
	// this test is about attribution, not about a whole changeover running.
	res, err := db.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state, started_at, updated_at)
		VALUES (?, ?, 'in_progress', datetime('now'), datetime('now'))`, processID, toStyleID)
	if err != nil {
		t.Fatalf("insert changeover: %v", err)
	}
	coID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("changeover id: %v", err)
	}
	originID := "co-origin-123"
	if err := db.SetChangeoverOriginID(coID, originID); err != nil {
		t.Fatalf("set changeover origin: %v", err)
	}

	// The flag is what opts this loader in. A loader that was never opted in
	// serves its ordinary steady-state demand right through a changeover, and
	// attributing those would inflate the changeover's ratio with bins it did
	// not cause — the mirror of the under-count above.
	if o := eng.changeoverLoadOrigin(node, off); o.ID != "" {
		t.Errorf("a loader without the directive keeps its own demand; got %+v", o)
	}

	got := eng.changeoverLoadOrigin(node, on)
	if got.ID != originID {
		t.Errorf("origin = %q, want the changeover's episode %q", got.ID, originID)
	}
	if got.Class != protocol.OriginClassAttached {
		t.Errorf("origin class = %q, want %q", got.Class, protocol.OriginClassAttached)
	}

	// A nil claim is a node with no configuration; it cannot have opted in.
	if o := eng.changeoverLoadOrigin(node, nil); o.ID != "" {
		t.Errorf("a claimless node has no directive; got %+v", o)
	}
}
